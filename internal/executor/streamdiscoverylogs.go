package executor

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// StreamDiscoveryLogs starts tailing logs for a discovered deployment (Docker or Systemd).
func StreamDiscoveryLogs(ctx context.Context, payload models.StreamDiscoveryLogsPayload, flush FlushFunc) error {
	var cmd *exec.Cmd

	if payload.Source == "DOCKER" {
		if payload.ContainerName == "" {
			return fmt.Errorf("container_name is required for DOCKER source")
		}
		args := []string{"logs", "--follow"}
		if payload.Tail > 0 {
			args = append(args, "--tail", strconv.Itoa(payload.Tail))
		}
		args = append(args, payload.ContainerName)
		cmd = exec.CommandContext(ctx, "docker", args...)
	} else if payload.Source == "SYSTEMD" {
		if payload.UnitName == "" {
			return fmt.Errorf("unit_name is required for SYSTEMD source")
		}
		args := []string{"-u", payload.UnitName, "--follow", "--no-pager"}
		if payload.Tail > 0 {
			args = append(args, "-n", strconv.Itoa(payload.Tail))
		}
		cmd = exec.CommandContext(ctx, "journalctl", args...)
	} else {
		return fmt.Errorf("unknown discovery source: %s", payload.Source)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to attach stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to attach stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start log stream: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			_ = flush([]models.LogLine{{Stream: "stdout", Content: scanner.Text()}})
		}
	}()

	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			_ = flush([]models.LogLine{{Stream: "stderr", Content: scanner.Text()}})
		}
	}()

	// If DurationSeconds is specified, stop after that duration
	if payload.DurationSeconds > 0 {
		go func() {
			select {
			case <-time.After(time.Duration(payload.DurationSeconds) * time.Second):
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			case <-ctx.Done():
			}
		}()
	}

	wg.Wait()
	_ = cmd.Wait()

	return nil
}
