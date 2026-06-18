package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// caddySitesDir holds one Caddy site snippet per app. The main Caddyfile
// (set up by install.sh) imports these: `import /etc/thalesops/caddy/*.caddy`.
const caddySitesDir = "/etc/thalesops/caddy"

// proxyAvailable reports whether Caddy is installed on this server.
func proxyAvailable() bool {
	_, err := exec.LookPath("caddy")
	return err == nil
}

// configureProxy points the app's domains at its localhost port via Caddy, then
// reloads. Caddy auto-provisions + renews Let's Encrypt certs for each domain.
//
// Best-effort: a proxy hiccup must not fail an otherwise-successful deploy — the
// container is already running and health-checked at this point.
func configureProxy(ctx context.Context, sh *LogShipper, appSlug string, hostPort int, domains []string) {
	if len(domains) == 0 || hostPort <= 0 {
		return
	}
	if _, err := exec.LookPath("caddy"); err != nil {
		sh.Write("system", "Caddy is not installed — skipping domain routing (app still reachable on its host).")
		return
	}
	if err := os.MkdirAll(caddySitesDir, 0o755); err != nil {
		sh.Write("stderr", "could not create proxy config dir: "+err.Error())
		return
	}

	// <domain1>, <domain2> {
	//     reverse_proxy 127.0.0.1:<hostPort>
	// }
	block := fmt.Sprintf("%s {\n\treverse_proxy 127.0.0.1:%d\n}\n",
		strings.Join(domains, ", "), hostPort)

	path := filepath.Join(caddySitesDir, sanitizeName(appSlug)+".caddy")
	if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
		sh.Write("stderr", "could not write proxy config: "+err.Error())
		return
	}

	if err := reloadCaddy(ctx); err != nil {
		sh.Write("stderr", "proxy reload failed: "+err.Error())
		return
	}
	sh.System("Routing live (HTTPS auto-provisioned): " + strings.Join(domains, ", "))
}

// reloadCaddy applies the updated config without dropping connections.
func reloadCaddy(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "systemctl", "reload", "caddy").Run(); err == nil {
		return nil
	}
	return exec.CommandContext(ctx, "caddy", "reload", "--config", "/etc/caddy/Caddyfile").Run()
}
