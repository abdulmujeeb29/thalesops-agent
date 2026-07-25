package executor

import (
	"context"
	"fmt"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// ExecuteStreamDBLogs tails a managed database container's logs
// (`docker logs --follow`) for a bounded session, shipping lines to the
// backend's database-log buffer. Same shape as ExecuteStreamLogs, but the
// container name is explicit (databases have stable, known container names).
func ExecuteStreamDBLogs(rawPayload map[string]interface{}, flush FlushFunc) models.CommandResultRequest {
	p := parseStreamDbLogsPayload(rawPayload)
	if p.ContainerName == "" {
		return models.CommandResultRequest{ExitCode: 1, Stderr: "stream-db-logs: missing container_name"}
	}

	duration := time.Duration(p.DurationSeconds) * time.Second
	if duration <= 0 {
		duration = 5 * time.Minute
	}

	sh := NewLogShipper(flush, nil)
	defer sh.Close()

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	args := []string{"logs", "--follow", "--tail", fmt.Sprintf("%d", p.Tail), p.ContainerName}
	code, err := runStreaming(ctx, sh, "docker", args...)

	if ctx.Err() == context.DeadlineExceeded {
		return models.CommandResultRequest{ExitCode: 0, Stdout: "db log stream session ended"}
	}
	if err != nil {
		return models.CommandResultRequest{
			ExitCode: code,
			Stderr:   "could not stream database logs (is it running?): " + err.Error(),
		}
	}
	return models.CommandResultRequest{ExitCode: 0, Stdout: "db log stream ended"}
}

func parseStreamDbLogsPayload(m map[string]interface{}) models.StreamDbLogsPayload {
	return models.StreamDbLogsPayload{
		DatabaseID:      asString(m["database_id"]),
		ContainerName:   asString(m["container_name"]),
		Tail:            asInt(m["tail"]),
		DurationSeconds: asInt(m["duration_seconds"]),
	}
}
