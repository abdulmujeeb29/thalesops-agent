package executor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thalesops/agent/internal/models"
	"github.com/thalesops/agent/internal/system"
)

// ExecuteDeploy runs a full deployment on this server:
//   1. git clone the repo
//   2. nixpacks build → Docker image
//   3. docker run the container
//
// Output from every step is streamed back to the backend via `flush` as it is
// produced. Secrets (clone token, env values) are redacted before leaving the host.
func ExecuteDeploy(rawPayload map[string]interface{}, timeout time.Duration, flush FlushFunc) models.CommandResultRequest {
	p := parseDeployPayload(rawPayload)

	sh := NewLogShipper(flush, collectSecrets(p))
	defer sh.Close()

	fail := func(code int, msg string) models.CommandResultRequest {
		sh.Write("stderr", msg)
		return models.CommandResultRequest{ExitCode: code, Stderr: msg}
	}

	sh.System(fmt.Sprintf("Deploy started for %s (branch %s)", p.RepoFullName, p.Branch))

	// ── Guards ────────────────────────────────────────────────────────────────
	if err := system.EnsureDeployPrerequisites(); err != nil {
		return fail(1, err.Error())
	}
	if p.CloneURL == "" {
		return fail(1, "deploy payload missing clone_url")
	}
	if p.BuildMethod != "" && p.BuildMethod != "NIXPACKS" {
		return fail(1, fmt.Sprintf("build method %q is not supported yet (NIXPACKS only)", p.BuildMethod))
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// ── Workspace ───────────────────────────────────────────────────────────────
	workdir, err := os.MkdirTemp("", "thalesops-"+sanitizeName(p.AppSlug)+"-")
	if err != nil {
		return fail(1, "could not create work directory: "+err.Error())
	}
	defer os.RemoveAll(workdir)

	image := imageName(p.AppSlug)

	// ── 1. Clone ──────────────────────────────────────────────────────────────
	sh.System("Cloning repository…")
	if code, err := runStreaming(ctx, sh, "git", "clone", "--depth", "1",
		"--branch", p.Branch, p.CloneURL, workdir); err != nil {
		return fail(code, "git clone failed: "+err.Error())
	}

	// ── 2. Build ──────────────────────────────────────────────────────────────
	sh.System("Building image with Nixpacks…")
	if code, err := runStreaming(ctx, sh, "nixpacks", "build", workdir, "--name", image); err != nil {
		return fail(code, "nixpacks build failed: "+err.Error())
	}

	// ── 3. Run ────────────────────────────────────────────────────────────────
	sh.System("Starting container…")
	if code, err := runContainer(ctx, sh, p.AppSlug, p.Port, p.Env); err != nil {
		return fail(code, "docker run failed: "+err.Error())
	}

	sh.System("Deployment successful — container is running.")
	return models.CommandResultRequest{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("Deployed %s on port %d", p.RepoFullName, p.Port),
	}
}

// imageName / containerName derive the stable Docker names for an app slug.
// Stable across deploys so a build overwrites the same tag and a restart reuses it.
func imageName(appSlug string) string     { return "thalesops/" + sanitizeName(appSlug) }
func containerName(appSlug string) string { return "thalesops-" + sanitizeName(appSlug) }

// runContainer replaces the app's container with a fresh one from its existing
// image, injecting env via a temp --env-file. Shared by deploy (after build) and
// restart (reuse build). Returns the exit code and any error.
func runContainer(ctx context.Context, sh *LogShipper, appSlug string, port int, env map[string]string) (int, error) {
	image := imageName(appSlug)
	container := containerName(appSlug)

	// Replace any previous container with the same name (ignore errors if absent).
	_, _ = runStreaming(ctx, sh, "docker", "rm", "-f", container)

	envFile, err := writeEnvFile(env)
	if err != nil {
		return 1, fmt.Errorf("could not write env file: %w", err)
	}
	if envFile != "" {
		defer os.Remove(envFile)
	}

	runArgs := []string{"run", "-d", "--name", container, "--restart", "unless-stopped"}
	if envFile != "" {
		runArgs = append(runArgs, "--env-file", envFile)
	}
	if port > 0 {
		runArgs = append(runArgs, "-p", fmt.Sprintf("%d:%d", port, port))
	}
	runArgs = append(runArgs, image)

	return runStreaming(ctx, sh, "docker", runArgs...)
}

// runStreaming runs a command, streaming stdout and stderr to the shipper line
// by line as they are produced. Returns the exit code and an error (if any).
func runStreaming(ctx context.Context, sh *LogShipper, name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 1, err
	}

	if err := cmd.Start(); err != nil {
		return 1, err
	}

	// Read both pipes concurrently. wg completes only when both are fully drained,
	// which must happen before cmd.Wait() per os/exec semantics.
	var wg sync.WaitGroup
	wg.Add(2)
	go scanInto(stdout, "stdout", sh, &wg)
	go scanInto(stderr, "stderr", sh, &wg)
	wg.Wait()

	waitErr := cmd.Wait()

	if ctx.Err() == context.DeadlineExceeded {
		return 1, fmt.Errorf("step timed out")
	}
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				return ws.ExitStatus(), waitErr
			}
		}
		return 1, waitErr
	}
	return 0, nil
}

func scanInto(r io.Reader, stream string, sh *LogShipper, wg *sync.WaitGroup) {
	defer wg.Done()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate lines up to 1MB
	for scanner.Scan() {
		sh.Write(stream, scanner.Text())
	}
}

// ── payload parsing helpers ────────────────────────────────────────────────────

func parseDeployPayload(m map[string]interface{}) models.DeployPayload {
	p := models.DeployPayload{Env: map[string]string{}}
	p.DeploymentID = asString(m["deployment_id"])
	p.AppSlug = asString(m["app_slug"])
	p.RepoFullName = asString(m["repo_full_name"])
	p.CloneURL = asString(m["clone_url"])
	p.Branch = asString(m["branch"])
	if p.Branch == "" {
		p.Branch = "main"
	}
	p.BuildMethod = asString(m["build_method"])
	p.Port = asInt(m["port"])
	if env, ok := m["env"].(map[string]interface{}); ok {
		for k, v := range env {
			p.Env[k] = asString(v)
		}
	}
	return p
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt(v interface{}) int {
	switch n := v.(type) {
	case float64: // JSON numbers decode to float64
		return int(n)
	case int:
		return n
	}
	return 0
}

// collectSecrets gathers values that must never appear in streamed logs.
func collectSecrets(p models.DeployPayload) []string {
	var secrets []string
	if tok := extractCloneToken(p.CloneURL); tok != "" {
		secrets = append(secrets, tok)
	}
	for _, v := range p.Env {
		if v != "" {
			secrets = append(secrets, v)
		}
	}
	return secrets
}

// extractCloneToken pulls the token out of https://x-access-token:TOKEN@github.com/...
func extractCloneToken(cloneURL string) string {
	const marker = "x-access-token:"
	i := strings.Index(cloneURL, marker)
	if i == -1 {
		return ""
	}
	rest := cloneURL[i+len(marker):]
	if at := strings.Index(rest, "@"); at != -1 {
		return rest[:at]
	}
	return ""
}

// sanitizeName makes a string safe for use as a Docker image/container name.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "app"
	}
	return out
}

// writeEnvFile writes env vars to a 0600 temp file in docker --env-file format.
// Returns "" (no file) when there are no env vars.
func writeEnvFile(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	f, err := os.CreateTemp("", "thalesops-env-")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	for k, v := range env {
		// docker --env-file: one KEY=VALUE per line; value is literal to EOL.
		// Strip newlines from the value to keep the file well-formed.
		v = strings.ReplaceAll(v, "\n", " ")
		if _, err := fmt.Fprintf(f, "%s=%s\n", k, v); err != nil {
			return "", err
		}
	}
	return f.Name(), nil
}
