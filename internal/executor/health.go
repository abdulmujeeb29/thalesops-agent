package executor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// HealthCheckTimeout bounds how long we wait for a new container to come up
// during the pre-swap smoke test before declaring it unhealthy.
const HealthCheckTimeout = 60 * time.Second

// deployContainer brings a new image live without downtime.
//
// When a proxy fronts the app (Caddy/nginx + domains) it does a BLUE-GREEN
// swap: the new container starts on a fresh random localhost port NEXT TO the
// old one, is health-checked in place, and traffic moves via a graceful proxy
// reload — at no instant is nothing listening. Only then is the old container
// retired. Zero blip.
//
// Without a proxy (no domains, port-less worker, or an unmanaged :443) it falls
// back to verify-then-swap on the fixed port: smoke-test a throwaway container,
// then rm old → run new. Safe (a broken image never goes live) but with a brief
// boot-time blip — there's nothing to flip without a proxy in front.
func deployContainer(ctx context.Context, sh *LogShipper, appSlug, image string, port, hostPort int, env map[string]string, domains, networks []string, healthPath string) (int, error) {
	backend := detectProxyBackend()
	if backend != "none" && port > 0 && len(domains) > 0 {
		return blueGreenSwap(ctx, sh, backend, appSlug, image, port, env, domains, networks, healthPath)
	}

	// ── Fallback path (no proxy to flip) ──────────────────────────────────────
	if err := smokeTest(ctx, sh, appSlug, image, port, env, healthPath); err != nil {
		// Old container untouched — no downtime from a bad deploy.
		return 1, err
	}
	sh.System("Health check passed — swapping to the new version…")
	code, err := runContainer(ctx, sh, appSlug, image, port, hostPort, env, false, networks)
	if err != nil {
		return code, err
	}
	if len(domains) > 0 && hostPort > 0 {
		// User wanted a domain but we have no managed proxy on this server — explain.
		owner := portOwner(443)
		reason := "no reverse proxy is set up"
		if owner != "" && owner != "caddy" && owner != "nginx" {
			reason = fmt.Sprintf("port 443 is held by %q (not managed by ThalesOps)", owner)
		}
		sh.System(fmt.Sprintf(
			"⚠ Domain routing skipped (%s). The app is live on its host port; "+
				"re-run the ThalesOps installer to set up a proxy, or free ports 80/443.",
			reason,
		))
	}
	return 0, nil
}

// blueGreenSwap runs the zero-downtime path:
//
//	1. start the NEW container on a fresh random localhost port (old untouched)
//	2. health-gate it in place (TCP + optional HTTP path) — this IS the runtime
//	   container, so the image boots exactly once
//	3. flip the proxy upstream to the new port (graceful reload: in-flight
//	   requests finish on the old container, new requests hit the new one)
//	4. only after a successful flip: retire the old container(s)
//
// A failure at any step before 4 removes the new container and leaves the old
// one serving — same guarantee as always, now with zero downtime on success.
func blueGreenSwap(ctx context.Context, sh *LogShipper, backend, appSlug, image string, port int, env map[string]string, domains, networks []string, healthPath string) (int, error) {
	newName := versionedContainerName(appSlug)

	envFile, err := writeEnvFile(env)
	if err != nil {
		return 1, fmt.Errorf("could not write env file: %w", err)
	}
	if envFile != "" {
		defer os.Remove(envFile)
	}

	// discard removes the not-yet-live new container after a failure.
	discard := func() {
		_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", newName).Run()
	}

	sh.System("Starting the new version alongside the current one…")
	args := []string{"run", "-d", "--name", newName,
		"--label", appLabel(appSlug), "--restart", "unless-stopped"}
	args = append(args, hostGatewayArgs()...)
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	// 127.0.0.1:: → Docker picks a free localhost port; the proxy is the public entry.
	args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", port), image)
	if code, err := runStreaming(ctx, sh, "docker", args...); err != nil {
		discard()
		return code, fmt.Errorf("could not start the new container (exit %d): %w", code, err)
	}

	// Join attached-database networks BEFORE the health check, so the app can
	// reach its DB during startup/migrations.
	if err := connectNetworks(ctx, sh, newName, networks); err != nil {
		discard()
		return 1, err
	}

	hostPort, err := assignedHostPort(ctx, newName, port)
	if err != nil {
		discard()
		return 1, fmt.Errorf("could not determine the new container's port: %w", err)
	}
	if healthPath != "" {
		sh.System(fmt.Sprintf("Waiting for a healthy response on %s…", healthPath))
	}
	if err := waitForPort(ctx, sh, newName, hostPort, HealthCheckTimeout, healthPath); err != nil {
		discard()
		return 1, err // "keeping the current version running" is in the message
	}

	sh.System("New version healthy — switching traffic (zero downtime)…")
	hp, _ := strconv.Atoi(hostPort)
	if err := configureProxy(ctx, sh, backend, appSlug, hp, domains); err != nil {
		// The proxy still points at the OLD container, which is untouched and
		// serving. Drop the new one and fail the deploy rather than leave two
		// versions running with stale routing.
		discard()
		return 1, fmt.Errorf("could not switch traffic to the new version — keeping the current one: %w", err)
	}

	retireOldContainers(ctx, sh, appSlug, newName)
	sh.System("Traffic switched — deploy completed with zero downtime.")
	return 0, nil
}

// versionedContainerName gives each deploy its own container name so old and
// new can run side by side during the swap.
func versionedContainerName(appSlug string) string {
	return fmt.Sprintf("%s-%d", containerName(appSlug), time.Now().UnixMilli())
}

// appLabel tags every blue-green container so old versions (and strays from
// interrupted deploys) can be found and retired reliably.
func appLabel(appSlug string) string {
	return "thalesops.app=" + sanitizeName(appSlug)
}

// findLiveContainer returns the name of the app's RUNNING container: the
// labeled blue-green container if one is up, else the legacy fixed name.
func findLiveContainer(appSlug string) string {
	out, err := exec.Command("docker", "ps",
		"--filter", "label="+appLabel(appSlug), "--format", "{{.Names}}").Output()
	if err == nil {
		if name := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]); name != "" {
			return name
		}
	}
	return containerName(appSlug)
}

// retireOldContainers removes every container belonging to this app EXCEPT the
// one that just went live — previous versions, strays from crashed deploys, and
// the legacy fixed-name container from pre-blue-green agent versions.
func retireOldContainers(ctx context.Context, sh *LogShipper, appSlug, keepName string) {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label="+appLabel(appSlug), "--format", "{{.Names}}").Output()
	if err == nil {
		for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name = strings.TrimSpace(name)
			if name == "" || name == keepName {
				continue
			}
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
		}
	}
	// Legacy fixed-name container (pre-blue-green agents) has no label.
	if legacy := containerName(appSlug); legacy != keepName {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", legacy).Run()
	}
}

// smokeTest starts a throwaway container from the given image on a random
// localhost port and verifies it boots, without touching the live container.
func smokeTest(ctx context.Context, sh *LogShipper, appSlug, image string, port int, env map[string]string, healthPath string) error {
	checkName := containerName(appSlug) + "-check"

	// Clean any leftover check container from a previous (interrupted) run.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", checkName).Run()

	envFile, err := writeEnvFile(env)
	if err != nil {
		return fmt.Errorf("could not write env file: %w", err)
	}
	if envFile != "" {
		defer os.Remove(envFile)
	}

	args := []string{"run", "-d", "--name", checkName}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	if port > 0 {
		// Random host port bound to localhost — not publicly exposed during the check.
		args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", port))
	}
	args = append(args, image)

	sh.System("Verifying the new build is healthy (on a temporary port)…")
	if code, err := runStreaming(ctx, sh, "docker", args...); err != nil {
		_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", checkName).Run()
		return fmt.Errorf("could not start verification container (exit %d): %w", code, err)
	}
	// Always tear down the check container, even on error/timeout.
	defer func() { _ = exec.CommandContext(context.Background(), "docker", "rm", "-f", checkName).Run() }()

	if port > 0 {
		hostPort, err := assignedHostPort(ctx, checkName, port)
		if err != nil {
			return fmt.Errorf("could not determine health-check port: %w", err)
		}
		if healthPath != "" {
			sh.System(fmt.Sprintf("Waiting for a healthy response on %s…", healthPath))
		}
		return waitForPort(ctx, sh, checkName, hostPort, HealthCheckTimeout, healthPath)
	}
	// No published port (e.g. a worker): just confirm it doesn't crash on boot.
	return waitStaysRunning(ctx, sh, checkName, 5*time.Second)
}

// assignedHostPort reads the random host port Docker assigned to the container.
func assignedHostPort(ctx context.Context, name string, containerPort int) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "port", name, fmt.Sprintf("%d/tcp", containerPort)).Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]) // e.g. "127.0.0.1:49153"
	i := strings.LastIndex(line, ":")
	if i == -1 {
		return "", fmt.Errorf("unexpected `docker port` output: %q", line)
	}
	return line[i+1:], nil
}

// waitForPort polls until the container is healthy on hostPort, or times out.
// Fails fast (with the container's logs) if it exits during startup.
//
// If healthPath is empty, "healthy" means the app accepts a TCP connection
// (it's listening). If healthPath is set, listening is not enough — we require
// an HTTP GET on that path to return a 2xx/3xx, which catches an app that binds
// the port but then errors on every request (e.g. a bad env var → 500 on boot).
func waitForPort(ctx context.Context, sh *LogShipper, name, hostPort string, timeout time.Duration, healthPath string) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", hostPort)
	for time.Now().Before(deadline) {
		if !isRunning(ctx, name) {
			dumpCheckLogs(ctx, sh, name)
			return fmt.Errorf("the new container exited during startup — keeping the current version running")
		}
		if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
			conn.Close()
			if healthPath == "" {
				return nil // TCP-only: listening is enough
			}
			if httpHealthy(ctx, addr, healthPath) {
				return nil // listening AND answering 2xx/3xx on the health path
			}
			// Listening but not healthy yet (still booting / returning 5xx) — keep polling.
		}
		time.Sleep(2 * time.Second)
	}
	dumpCheckLogs(ctx, sh, name)
	if healthPath != "" {
		return fmt.Errorf("the new build did not return a healthy response on %s within %s — keeping the current version running", healthPath, timeout)
	}
	return fmt.Errorf("the new build did not become healthy within %s — keeping the current version running", timeout)
}

// httpHealthy does one GET on http://<addr><path> and reports whether it
// answered with a 2xx/3xx. Any transport error (still booting, connection reset)
// counts as not-yet-healthy so the caller keeps polling until the deadline.
func httpHealthy(ctx context.Context, addr, path string) bool {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// waitStaysRunning confirms a port-less container doesn't immediately crash.
func waitStaysRunning(ctx context.Context, sh *LogShipper, name string, dur time.Duration) error {
	time.Sleep(dur)
	if !isRunning(ctx, name) {
		dumpCheckLogs(ctx, sh, name)
		return fmt.Errorf("the new container exited on startup — keeping the current version running")
	}
	return nil
}

func isRunning(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// dumpCheckLogs streams the failing check container's recent output so the user
// sees WHY the new build didn't come up.
func dumpCheckLogs(ctx context.Context, sh *LogShipper, name string) {
	out, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "30", name).CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			sh.Write("stderr", line)
		}
	}
}
