package executor

import (
	"bytes"
	"os/exec"
	"syscall"

	"github.com/thalesops/agent/internal/models"
)

func ExecuteShell(payload map[string]interface{}) models.CommandResultRequest {
	cmdStr, ok := payload["command"].(string)
	if !ok {
		return models.CommandResultRequest{
			ExitCode: 1,
			Stderr:   "Invalid payload: 'command' field missing or not a string",
		}
	}

	cmd := exec.Command("sh", "-c", cmdStr)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	
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
