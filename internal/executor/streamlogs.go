package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// ExecuteStreamLogs tails the running app container's logs (`docker logs --follow`)
// for a bounded session, shipping lines to the backend's app-log buffer as they
// appear. The session ends when DurationSeconds elapses (the frontend renews it
// while the user keeps the Logs tab open).
//
// `flush` ships a batch to the app-log endpoint (wired in main.go to SubmitAppLogs).
func ExecuteStreamLogs(rawPayload map[string]interface{}, flush FlushFunc) models.CommandResultRequest {
	p := parseStreamLogsPayload(rawPayload)

	if p.AppSlug == "" {
		return models.CommandResultRequest{ExitCode: 1, Stderr: "stream-logs: missing app_slug"}
	}

	duration := time.Duration(p.DurationSeconds) * time.Second
	if duration <= 0 {
		duration = 5 * time.Minute
	}

	// Runtime logs aren't secrets we control, but redact nothing special here —
	// the shipper just batches and flushes.
	sh := NewLogShipper(flush, nil)
	defer sh.Close()

	container := containerName(p.AppSlug)

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	// `docker logs --follow` streams existing + new output until we cancel the
	// context (at the session deadline), which kills the process.
	args := []string{"logs", "--follow", "--tail", fmt.Sprintf("%d", p.Tail), container}
	code, err := runStreaming(ctx, sh, "docker", args...)

	// A session that ends because the deadline elapsed is a normal, successful end
	// (the context was cancelled on purpose) — not a failure.
	if ctx.Err() == context.DeadlineExceeded {
		return models.CommandResultRequest{ExitCode: 0, Stdout: "log stream session ended"}
	}
	if err != nil {
		return models.CommandResultRequest{
			ExitCode: code,
			Stderr:   "could not stream container logs (is the app running?): " + err.Error(),
		}
	}
	return models.CommandResultRequest{ExitCode: 0, Stdout: "log stream ended"}
}

func parseStreamLogsPayload(m map[string]interface{}) models.StreamLogsPayload {
	return models.StreamLogsPayload{
		ApplicationID:   asString(m["application_id"]),
		AppSlug:         asString(m["app_slug"]),
		Tail:            asInt(m["tail"]),
		DurationSeconds: asInt(m["duration_seconds"]),
	}
}
