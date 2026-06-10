package executor

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/thalesops/agent/internal/models"
	"github.com/thalesops/agent/internal/system"
)

// ExecuteRestart re-runs an app's container with updated env, reusing the image
// already built on this server. No clone, no nixpacks build — a fast config apply
// (used after env changes). Streams its output like a deploy.
func ExecuteRestart(rawPayload map[string]interface{}, timeout time.Duration, flush FlushFunc) models.CommandResultRequest {
	// Reuses the deploy payload parser — only app_slug, port and env are relevant here.
	p := parseDeployPayload(rawPayload)

	sh := NewLogShipper(flush, collectSecrets(p))
	defer sh.Close()

	fail := func(code int, msg string) models.CommandResultRequest {
		sh.Write("stderr", msg)
		return models.CommandResultRequest{ExitCode: code, Stderr: msg}
	}

	sh.System("Restarting with updated configuration…")

	if err := system.EnsureDeployPrerequisites(); err != nil {
		return fail(1, err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Guard: the image must already exist (from a prior build). Restart can't build.
	image := imageName(p.AppSlug)
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err != nil {
		return fail(1, fmt.Sprintf(
			"no built image found for this app (%s) — deploy it once before restarting", image,
		))
	}

	if code, err := runContainer(ctx, sh, p.AppSlug, p.Port, p.Env); err != nil {
		return fail(code, "restart failed: "+err.Error())
	}

	sh.System("Restart successful — container is running with the new configuration.")
	return models.CommandResultRequest{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("Restarted %s on port %d", p.AppSlug, p.Port),
	}
}
