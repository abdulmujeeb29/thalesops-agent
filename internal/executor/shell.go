package executor

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// ExecuteShell runs a shell command with a hard timeout.
// If the command exceeds the timeout, it is killed and a timeout error is returned.
func ExecuteShell(payload map[string]interface{}, timeout time.Duration) models.CommandResultRequest {
	cmdStr, ok := payload["command"].(string)
	if !ok {
		return models.CommandResultRequest{
			ExitCode: 1,
			Stderr:   "Invalid payload: 'command' field missing or not a string",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return models.CommandResultRequest{
			ExitCode: 1,
			Stdout:   stdout.String(),
			Stderr:   fmt.Sprintf("Command timed out after %v\n%s", timeout, stderr.String()),
		}
	}

	exitCode := 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			exitCode = ws.ExitStatus()
		} else {
			exitCode = 1
		}
	}

	return models.CommandResultRequest{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
}
