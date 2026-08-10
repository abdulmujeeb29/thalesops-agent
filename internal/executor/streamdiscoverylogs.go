package executor

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// discoveryLogCommand builds the right tail command for whatever the deployment
// is running under. Each process manager keeps its logs somewhere different, so
// this is the one place that knows how to reach each of them.
func discoveryLogCommand(ctx context.Context, payload models.StreamDiscoveryLogsPayload) (*exec.Cmd, error) {
	tail := payload.Tail

	switch payload.Source {
	case "DOCKER", "PODMAN":
		name := payload.ContainerName
		if name == "" {
			name = payload.DiscoveryName
		}
		if name == "" {
			return nil, fmt.Errorf("container_name is required for %s source", payload.Source)
		}
		bin := "docker"
		if payload.Source == "PODMAN" {
			bin = "podman"
		}
		args := []string{"logs", "--follow"}
		if tail > 0 {
			args = append(args, "--tail", strconv.Itoa(tail))
		}
		return exec.CommandContext(ctx, bin, append(args, name)...), nil

	case "SYSTEMD":
		unit := payload.UnitName
		if unit == "" {
			unit = payload.DiscoveryName
		}
		if unit == "" {
			return nil, fmt.Errorf("unit_name is required for SYSTEMD source")
		}
		args := []string{"-u", unit, "--follow", "--no-pager"}
		if tail > 0 {
			args = append(args, "-n", strconv.Itoa(tail))
		}
		return exec.CommandContext(ctx, "journalctl", args...), nil

	case "PM2":
		// Identifiers for another user's apps are namespaced "<user>/<app>";
		// pm2 itself only knows the app name, and must run as that user.
		name, owner := payload.DiscoveryName, ""
		if idx := strings.Index(name, "/"); idx > 0 {
			owner, name = name[:idx], name[idx+1:]
		}
		if name == "" {
			return nil, fmt.Errorf("discovery name is required for PM2 source")
		}
		if tail <= 0 {
			tail = 200
		}
		pm2Cmd := fmt.Sprintf("pm2 logs %s --lines %d --raw", shellQuote(name), tail)
		if owner != "" {
			return exec.CommandContext(ctx, "su", "-", owner, "-c", pm2Cmd), nil
		}
		return exec.CommandContext(ctx, "sh", "-c", pm2Cmd), nil

	case "SUPERVISOR":
		if payload.DiscoveryName == "" {
			return nil, fmt.Errorf("discovery name is required for SUPERVISOR source")
		}
		return exec.CommandContext(ctx, "supervisorctl", "tail", "-f", payload.DiscoveryName), nil

	case "PROCESS":
		// A bare process writes wherever its author chose (or nowhere). There is
		// no log source we can follow without guessing, so we say so plainly.
		return nil, fmt.Errorf("live logs aren't available for a standalone process — " +
			"it isn't managed by Docker, systemd, PM2 or supervisor")

	default:
		return nil, fmt.Errorf("unknown discovery source: %s", payload.Source)
	}
}

// shellQuote makes a value safe to embed in the `sh -c` strings above.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// StreamDiscoveryLogs starts tailing logs for a discovered deployment.
func StreamDiscoveryLogs(ctx context.Context, payload models.StreamDiscoveryLogsPayload, flush FlushFunc) error {
	cmd, err := discoveryLogCommand(ctx, payload)
	if err != nil {
		return err
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
