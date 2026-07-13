package executor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// HealthCheckTimeout bounds how long we wait for a new container to come up
// during the pre-swap smoke test before declaring it unhealthy.
const HealthCheckTimeout = 60 * time.Second

// deployContainer performs a HEALTH-GATED swap:
//
//	1. start the new image as a throwaway container on a random localhost port
//	2. verify it actually comes up (the currently-running container is untouched)
//	3. only if healthy → swap it in on the real port (brief blip, validated image)
//	4. if unhealthy → leave the old container running and fail the deploy
//
// The key guarantee: a broken build/env can never take down the running app —
// the old container is only removed once the new one is proven to start.
func deployContainer(ctx context.Context, sh *LogShipper, appSlug, image string, port, hostPort int, env map[string]string, domains []string, healthPath string) (int, error) {
	if err := smokeTest(ctx, sh, appSlug, image, port, env, healthPath); err != nil {
		// Old container untouched — no downtime from a bad deploy.
		return 1, err
	}
	sh.System("Health check passed — swapping to the new version…")
	// Pick the proxy backend by what already fronts this server (Caddy on a clean
	// box, nginx if it's already there). Only bind localhost if a backend will
	// actually route to us; otherwise publish the host port so the app stays reachable.
	backend := detectProxyBackend()
	useProxy := backend != "none" && hostPort > 0 && len(domains) > 0
	code, err := runContainer(ctx, sh, appSlug, image, port, hostPort, env, useProxy)
	if err != nil {
		return code, err
	}
	if useProxy {
		configureProxy(ctx, sh, backend, appSlug, hostPort, domains)
	} else if len(domains) > 0 && hostPort > 0 {
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
