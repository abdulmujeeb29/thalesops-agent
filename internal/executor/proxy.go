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

// nginxConfDir is where we drop per-app nginx server blocks.
const nginxConfDir = "/etc/nginx/conf.d"

// detectProxyBackend decides how to expose apps on THIS server, by respecting
// whatever already owns port 443:
//
//	"caddy"  → Caddy owns :443, or :443 is free and Caddy is available
//	"nginx"  → nginx already owns :443 (or is installed) → integrate, don't fight it
//	"none"   → some other server owns :443 → leave it alone, app stays on host port
//
// This is the key "be a good guest" rule: we never displace a web server the user
// set up themselves; if nginx is there, we use nginx.
func detectProxyBackend() string {
	switch portOwner(443) {
	case "nginx":
		return "nginx"
	case "caddy":
		return "caddy"
	case "": // :443 is free
		if _, err := exec.LookPath("caddy"); err == nil {
			return "caddy"
		}
		if _, err := exec.LookPath("nginx"); err == nil {
			return "nginx"
		}
		return "none"
	default: // apache / haproxy / unknown → don't touch it
		return "none"
	}
}

// portOwner returns the process name listening on the given TCP port
// ("caddy", "nginx", …), or "" if nothing is listening / it can't be determined.
func portOwner(port int) string {
	out, err := exec.Command("ss", "-ltnp").CombinedOutput()
	if err != nil {
		return ""
	}
	needle := fmt.Sprintf(":%d ", port)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		if i := strings.Index(line, `users:(("`); i != -1 {
			rest := line[i+len(`users:(("`):]
			if j := strings.Index(rest, `"`); j != -1 {
				return rest[:j]
			}
		}
		return "unknown"
	}
	return ""
}

// configureProxy routes the app's domains to its localhost port using whichever
// backend fronts this server. Best-effort: a proxy hiccup must not fail an
// otherwise-successful deploy (the container is already running + health-checked).
func configureProxy(ctx context.Context, sh *LogShipper, backend, appSlug string, hostPort int, domains []string) {
	if len(domains) == 0 || hostPort <= 0 {
		return
	}
	switch backend {
	case "caddy":
		configureCaddy(ctx, sh, appSlug, hostPort, domains)
	case "nginx":
		configureNginx(ctx, sh, appSlug, hostPort, domains)
	}
}

// ── Caddy backend (clean servers): automatic HTTPS, one snippet per app ─────────

func configureCaddy(ctx context.Context, sh *LogShipper, appSlug string, hostPort int, domains []string) {
	if err := os.MkdirAll(caddySitesDir, 0o755); err != nil {
		sh.Write("stderr", "could not create proxy config dir: "+err.Error())
		return
	}
	// The @dotfiles matcher refuses probes for secret dotfiles (.env, .git, ...)
	// before they reach the app. Caddy's regexes are RE2 (no lookahead), so the
	// ACME path is excluded with a `not` submatcher instead.
	block := fmt.Sprintf(`%s {
	@dotfiles {
		path_regexp /\.
		not path /.well-known/*
	}
	respond @dotfiles 404

	reverse_proxy 127.0.0.1:%d
}
`, strings.Join(domains, ", "), hostPort)
	path := filepath.Join(caddySitesDir, sanitizeName(appSlug)+".caddy")
	if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
		sh.Write("stderr", "could not write proxy config: "+err.Error())
		return
	}
	if err := reloadCaddy(ctx); err != nil {
		sh.Write("stderr", "proxy reload failed: "+err.Error())
		return
	}
	sh.System("Routing live via Caddy (HTTPS auto-provisioned): " + strings.Join(domains, ", "))
}

func reloadCaddy(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "systemctl", "reload", "caddy").Run(); err == nil {
		return nil
	}
	return exec.CommandContext(ctx, "caddy", "reload", "--config", "/etc/caddy/Caddyfile").Run()
}

// ── nginx backend (servers already running nginx): server block + certbot ───────

func configureNginx(ctx context.Context, sh *LogShipper, appSlug string, hostPort int, domains []string) {
	if err := os.MkdirAll(nginxConfDir, 0o755); err != nil {
		sh.Write("stderr", "could not access nginx config dir: "+err.Error())
		return
	}
	serverName := strings.Join(domains, " ")
	block := fmt.Sprintf(`# Managed by ThalesOps
server {
    listen 80;
    server_name %s;

    # Refuse probes for secret dotfiles (.env, .git, ...) at the edge so they
    # never reach the app container. /.well-known stays open (ACME renewals).
    location ~ /\.(?!well-known) {
        deny all;
        return 404;
    }

    location / {
        proxy_pass http://127.0.0.1:%d;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
`, serverName, hostPort)

	path := filepath.Join(nginxConfDir, "thalesops-"+sanitizeName(appSlug)+".conf")
	if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
		sh.Write("stderr", "could not write nginx config: "+err.Error())
		return
	}
	// Validate before reloading so we never break the user's existing nginx.
	if err := exec.CommandContext(ctx, "nginx", "-t").Run(); err != nil {
		os.Remove(path) // back out our snippet
		sh.Write("stderr", "nginx config test failed — leaving existing nginx untouched.")
		return
	}
	if err := exec.CommandContext(ctx, "systemctl", "reload", "nginx").Run(); err != nil {
		sh.Write("stderr", "nginx reload failed: "+err.Error())
		return
	}
	sh.System("Routing live via nginx: " + serverName)

	// Provision HTTPS via certbot (best-effort — app is already reachable on http).
	if _, err := exec.LookPath("certbot"); err != nil {
		sh.System("certbot not installed — app served over HTTP. Install certbot for HTTPS.")
		return
	}
	args := []string{"--nginx", "--non-interactive", "--agree-tos",
		"--register-unsafely-without-email", "--redirect"}
	for _, d := range domains {
		args = append(args, "-d", d)
	}
	if err := exec.CommandContext(ctx, "certbot", args...).Run(); err != nil {
		sh.Write("stderr", "certbot could not issue a certificate (app still reachable on http). "+
			"Check that the domain's DNS points here and port 80 is open.")
		return
	}
	sh.System("HTTPS provisioned via certbot for " + serverName)
}
