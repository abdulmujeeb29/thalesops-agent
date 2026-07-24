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

	// The specific versioned image to (re-)run. For an env-change restart this is
	// the current image; for a ROLLBACK it's a previous build's image.
	image := imageRef(p.AppSlug, p.ImageTag)
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err != nil {
		return fail(1, fmt.Sprintf(
			"image %s not found on this server — it may have been pruned; redeploy instead", image,
		))
	}

	// Health-gated: verify the image boots with the new env on a temp port before
	// retiring the running container, so a bad env value can't take the app down.
	if code, err := deployContainer(ctx, sh, p.AppSlug, image, p.Port, p.HostPort, p.Env, p.Domains, p.Networks, p.HealthCheckPath); err != nil {
		return fail(code, err.Error())
	}

	sh.System("Restart successful — container is running with the new configuration.")
	return models.CommandResultRequest{
		ExitCode: 0,
		Stdout:   fmt.Sprintf("Restarted %s on port %d", p.AppSlug, p.Port),
	}
}
