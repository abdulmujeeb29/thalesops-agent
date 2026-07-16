package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// Static sites are served straight from disk by the reverse proxy — no runtime
// container, no port, no idle memory. Layout per app:
//
//	/var/www/thalesops/<slug>/
//	├── releases/<deployment-id>/   ← immutable artifacts, newest N kept
//	└── current → releases/<id>     ← atomic pointer the proxy serves from
//
// Deploy = fill a new release folder, then flip `current`. Rollback = flip it
// back. A failed build never touches `current`, so the live site keeps serving.
const staticSitesRoot = "/var/www/thalesops"

// staticBuildImage is the throwaway environment for `npm run build` etc. Node is
// never installed on the host; the container is removed the moment it exits.
const staticBuildImage = "node:20-alpine"

// staticKeepReleases bounds the rollback history (mirrors pruneOldImages).
const staticKeepReleases = 5

// ExecuteStaticDeploy runs a static-site deployment:
//
//	1. clone the repo (exact commit supported, same as service deploys)
//	2. if a build command is set → run it in a one-shot node container
//	3. verify the publish dir is a plausible site (non-empty, has index.html)
//	4. copy it to releases/<deployment-id>/ and atomically flip `current`
//	5. route the domains via the proxy (static file-server config) + prune
func ExecuteStaticDeploy(rawPayload map[string]interface{}, timeout time.Duration, flush FlushFunc) models.CommandResultRequest {
	p := parseDeployPayload(rawPayload)

	sh := NewLogShipper(flush, collectSecrets(p))
	defer sh.Close()

	fail := func(code int, msg string) models.CommandResultRequest {
		sh.Write("stderr", msg)
		return models.CommandResultRequest{ExitCode: code, Stderr: msg}
	}

	sh.System(fmt.Sprintf("Static deploy started for %s (branch %s)", p.RepoFullName, p.Branch))

	if p.CloneURL == "" {
		return fail(1, "deploy payload missing clone_url")
	}
	if p.ImageTag == "" {
		return fail(1, "deploy payload missing release id")
	}
	// Files must be served by a proxy — there's no container to publish a port.
	backend := detectProxyBackend()
	if backend == "none" {
		return fail(1, "static sites need Caddy or nginx on this server to serve files — "+
			"re-run the ThalesOps installer to set one up (the current version, if any, keeps serving)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// ── 1. Clone ──────────────────────────────────────────────────────────────
	workdir, err := os.MkdirTemp("", "thalesops-"+sanitizeName(p.AppSlug)+"-")
	if err != nil {
		return fail(1, "could not create work directory: "+err.Error())
	}
	defer os.RemoveAll(workdir)

	sh.System("Cloning repository…")
	if code, err := runStreaming(ctx, sh, "git", "clone", "--depth", "1",
		"--branch", p.Branch, p.CloneURL, workdir); err != nil {
		return fail(code, "git clone failed: "+err.Error())
	}
	if p.Commit != "" {
		sh.System("Checking out commit " + shortSHA(p.Commit) + "…")
		if code, err := runStreaming(ctx, sh, "git", "-C", workdir, "fetch", "--depth", "1", "origin", p.Commit); err != nil {
			return fail(code, "could not fetch commit "+shortSHA(p.Commit)+": "+err.Error())
		}
		if code, err := runStreaming(ctx, sh, "git", "-C", workdir, "checkout", p.Commit); err != nil {
			return fail(code, "could not check out commit "+shortSHA(p.Commit)+": "+err.Error())
		}
	}

	// ── 2. Build (optional — plain HTML repos skip straight to publish) ───────
	if p.BuildCommand != "" {
		sh.System(fmt.Sprintf("Building site (%s) in a temporary container…", p.BuildCommand))
		if code, err := runStaticBuild(ctx, sh, workdir, p.BuildCommand, p.Env); err != nil {
			return fail(code, "build failed — the current version keeps serving: "+err.Error())
		}
	} else {
		sh.System("No build step — publishing files as-is.")
	}

	// ── 3. Locate + sanity-check the artifact ─────────────────────────────────
	artifact := filepath.Join(workdir, filepath.Clean("/"+p.PublishDir)) // clean stops ../ escapes
	if err := verifyStaticArtifact(artifact); err != nil {
		return fail(1, err.Error()+" — the current version keeps serving")
	}

	// ── 4. Publish: copy to a release folder, then atomically flip `current` ──
	appDir := filepath.Join(staticSitesRoot, sanitizeName(p.AppSlug))
	releaseDir := filepath.Join(appDir, "releases", p.ImageTag)
	sh.System("Publishing release " + shortSHA(p.ImageTag) + "…")
	if err := copyArtifact(ctx, artifact, releaseDir); err != nil {
		return fail(1, "could not publish release: "+err.Error())
	}
	if err := flipCurrent(appDir, releaseDir); err != nil {
		return fail(1, "could not activate release: "+err.Error())
	}

	// ── 5. Route + prune ──────────────────────────────────────────────────────
	if len(p.Domains) > 0 {
		configureStaticProxy(ctx, sh, backend, p.AppSlug, filepath.Join(appDir, "current"), p.Domains)
	} else {
		sh.System("⚠ No domains configured — the site is published but not routed. Add a domain to serve it.")
	}
	pruneOldReleases(sh, appDir, staticKeepReleases)

	sh.System("Deployment successful — static site is live.")
	return models.CommandResultRequest{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("Published %s (release %s)", p.RepoFullName, shortSHA(p.ImageTag)),
	}
}

// ExecuteStaticRollback re-points `current` at a previous release folder — the
// static equivalent of re-running an old image. Payload: app_slug + image_tag
// (the target release id) + domains.
func ExecuteStaticRollback(rawPayload map[string]interface{}, timeout time.Duration, flush FlushFunc) models.CommandResultRequest {
	p := parseDeployPayload(rawPayload)

	sh := NewLogShipper(flush, collectSecrets(p))
	defer sh.Close()

	fail := func(code int, msg string) models.CommandResultRequest {
		sh.Write("stderr", msg)
		return models.CommandResultRequest{ExitCode: code, Stderr: msg}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	appDir := filepath.Join(staticSitesRoot, sanitizeName(p.AppSlug))
	releaseDir := filepath.Join(appDir, "releases", p.ImageTag)

	sh.System("Rolling back to release " + shortSHA(p.ImageTag) + "…")
	if err := verifyStaticArtifact(releaseDir); err != nil {
		return fail(1, "that release is no longer on this server (pruned?) — redeploy the commit instead")
	}
	if err := flipCurrent(appDir, releaseDir); err != nil {
		return fail(1, "could not activate release: "+err.Error())
	}
	// Re-assert routing (covers domain changes since the release was published).
	if backend := detectProxyBackend(); backend != "none" && len(p.Domains) > 0 {
		configureStaticProxy(ctx, sh, backend, p.AppSlug, filepath.Join(appDir, "current"), p.Domains)
	}

	sh.System("Rollback complete — serving release " + shortSHA(p.ImageTag) + ".")
	return models.CommandResultRequest{ExitCode: 0, Stdout: "rolled back to " + p.ImageTag}
}

// runStaticBuild executes the build command inside a one-shot container with the
// repo mounted at /app. Env vars are passed in because frontend builds bake them
// into the bundle (VITE_*, REACT_APP_*, …).
func runStaticBuild(ctx context.Context, sh *LogShipper, workdir, command string, env map[string]string) (int, error) {
	envFile, err := writeEnvFile(env)
	if err != nil {
		return 1, fmt.Errorf("could not write env file: %w", err)
	}
	if envFile != "" {
		defer os.Remove(envFile)
	}
	args := []string{"run", "--rm", "-v", workdir + ":/app", "-w", "/app"}
	if envFile != "" {
		args = append(args, "--env-file", envFile)
	}
	args = append(args, staticBuildImage, "sh", "-c", command)
	return runStreaming(ctx, sh, "docker", args...)
}

// verifyStaticArtifact is the static equivalent of the health gate: the output
// must exist, be non-empty, and contain an index.html. A build that produced
// nothing (or the wrong publish_dir) never goes live.
func verifyStaticArtifact(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("publish directory %q not found — check the app's publish dir setting", filepath.Base(dir))
	}
	if len(entries) == 0 {
		return fmt.Errorf("publish directory is empty — the build produced no files")
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return fmt.Errorf("no index.html in the publish directory — check the app's publish dir setting")
	}
	return nil
}

// copyArtifact copies the built site into its immutable release folder.
// `cp -a` preserves modes/times and handles dotfiles; the target is replaced
// wholesale if a partial copy from an interrupted run exists.
func copyArtifact(ctx context.Context, src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	// `src/.` copies the CONTENTS of the publish dir (index.html at top level).
	return exec.CommandContext(ctx, "cp", "-a", src+"/.", dst).Run()
}

// flipCurrent atomically points <appDir>/current at the release dir: build the
// new symlink under a temp name, then rename over — readers never see a gap.
func flipCurrent(appDir, releaseDir string) error {
	current := filepath.Join(appDir, "current")
	tmp := current + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(releaseDir, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, current)
}

// pruneOldReleases keeps the newest `keep` release folders (by modification
// time) and deletes older ones — but never the one `current` points at.
func pruneOldReleases(sh *LogShipper, appDir string, keep int) {
	releasesDir := filepath.Join(appDir, "releases")
	entries, err := os.ReadDir(releasesDir)
	if err != nil || len(entries) <= keep {
		return
	}
	live, _ := os.Readlink(filepath.Join(appDir, "current"))

	type rel struct {
		path string
		mod  time.Time
	}
	var rels []rel
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rels = append(rels, rel{filepath.Join(releasesDir, e.Name()), info.ModTime()})
	}
	sort.Slice(rels, func(i, j int) bool { return rels[i].mod.After(rels[j].mod) }) // newest first

	removed := 0
	for i := keep; i < len(rels); i++ {
		if rels[i].path == strings.TrimSuffix(live, "/") {
			continue // never delete what's serving
		}
		if err := os.RemoveAll(rels[i].path); err == nil {
			removed++
		}
	}
	if removed > 0 {
		sh.System(fmt.Sprintf("Pruned %d old release(s), kept %d for rollback.", removed, keep))
	}
}
