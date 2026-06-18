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

	// Versioned, kept image — enables rollback to this exact build later.
	image := imageRef(p.AppSlug, p.ImageTag)

	// ── 1. Clone ──────────────────────────────────────────────────────────────
	sh.System("Cloning repository…")
	if code, err := runStreaming(ctx, sh, "git", "clone", "--depth", "1",
		"--branch", p.Branch, p.CloneURL, workdir); err != nil {
		return fail(code, "git clone failed: "+err.Error())
	}

	// Optionally check out a SPECIFIC commit (deploy-a-commit). Needs a fetch
	// because the shallow clone above only has the branch tip.
	if p.Commit != "" {
		sh.System("Checking out commit " + shortSHA(p.Commit) + "…")
		if code, err := runStreaming(ctx, sh, "git", "-C", workdir, "fetch", "--depth", "1", "origin", p.Commit); err != nil {
			return fail(code, "could not fetch commit "+shortSHA(p.Commit)+": "+err.Error())
		}
		if code, err := runStreaming(ctx, sh, "git", "-C", workdir, "checkout", p.Commit); err != nil {
			return fail(code, "could not check out commit "+shortSHA(p.Commit)+": "+err.Error())
		}
	}

	// ── 2. Build ──────────────────────────────────────────────────────────────
	sh.System("Building image with Nixpacks…")
	if code, err := runStreaming(ctx, sh, "nixpacks", "build", workdir, "--name", image); err != nil {
		return fail(code, "nixpacks build failed: "+err.Error())
	}

	// ── 3. Run (health-gated swap: verify new container before retiring old) ──
	sh.System("Starting container…")
	if code, err := deployContainer(ctx, sh, p.AppSlug, image, p.Port, p.HostPort, p.Env, p.Domains); err != nil {
		return fail(code, err.Error())
	}

	// Keep the last few image versions for rollback; prune older ones.
	pruneOldImages(ctx, sh, p.AppSlug, 5)

	sh.System("Deployment successful — container is running.")
	return models.CommandResultRequest{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("Deployed %s on port %d", p.RepoFullName, p.Port),
	}
}

// imageRepo / containerName derive the stable Docker names for an app slug.
func imageRepo(appSlug string) string     { return "thalesops/" + sanitizeName(appSlug) }
func containerName(appSlug string) string { return "thalesops-" + sanitizeName(appSlug) }

// imageRef is the versioned image reference, e.g. thalesops/<slug>:<tag>.
// Falls back to :latest for older/untagged payloads.
func imageRef(appSlug, tag string) string {
	if tag == "" {
		tag = "latest"
	}
	return imageRepo(appSlug) + ":" + tag
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// pruneOldImages keeps the newest `keep` tagged images for an app and removes
// older ones (so the rollback history is bounded). The in-use image is recent,
// so it's always in the keep set; `docker rmi` on any still-referenced image
// fails harmlessly and is ignored.
func pruneOldImages(ctx context.Context, sh *LogShipper, appSlug string, keep int) {
	repo := imageRepo(appSlug)
	out, err := exec.CommandContext(ctx, "docker", "images", repo,
		"--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, ":<none>") {
			continue
		}
		refs = append(refs, line)
	}
	removed := 0
	for i := keep; i < len(refs); i++ { // docker images lists newest-first
		if err := exec.CommandContext(ctx, "docker", "rmi", refs[i]).Run(); err == nil {
			removed++
		}
	}
	if removed > 0 {
		sh.System(fmt.Sprintf("Pruned %d old image version(s), kept %d for rollback.", removed, keep))
	}
}

// runContainer replaces the app's container with a fresh one from its existing
// image, injecting env via a temp --env-file. Shared by deploy (after build) and
// restart (reuse build). Returns the exit code and any error.
func runContainer(ctx context.Context, sh *LogShipper, appSlug, image string, port, hostPort int, env map[string]string, useProxy bool) (int, error) {
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
	switch {
	case useProxy && hostPort > 0 && port > 0:
		// Proxy fronts it → bind to localhost only; the proxy is the public entry.
		runArgs = append(runArgs, "-p", fmt.Sprintf("127.0.0.1:%d:%d", hostPort, port))
	case hostPort > 0 && port > 0:
		// No proxy → publish on the host port so it's still reachable at ip:host_port.
		runArgs = append(runArgs, "-p", fmt.Sprintf("%d:%d", hostPort, port))
	case port > 0:
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
	p.Commit = asString(m["commit"])
	p.ImageTag = asString(m["image_tag"])
	p.BuildMethod = asString(m["build_method"])
	p.Port = asInt(m["port"])
	p.HostPort = asInt(m["host_port"])
	if env, ok := m["env"].(map[string]interface{}); ok {
		for k, v := range env {
			p.Env[k] = asString(v)
		}
	}
	if domains, ok := m["domains"].([]interface{}); ok {
		for _, d := range domains {
			if s := asString(d); s != "" {
				p.Domains = append(p.Domains, s)
			}
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
