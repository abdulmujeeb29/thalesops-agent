package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"
	"github.com/thalesops/agent/internal/api"
	"github.com/thalesops/agent/internal/config"
	"github.com/thalesops/agent/internal/executor"
	"github.com/thalesops/agent/internal/models"
	"github.com/thalesops/agent/internal/system"
	"github.com/thalesops/agent/internal/ws"
)

// Version is injected at build time via:
//   go build -ldflags "-X main.Version=<git-sha>" main.go
// It defaults to "dev" when built without the flag.
var Version = "dev"

// inFlight tracks command IDs that are currently executing.
// Prevents the same command from being run twice even when it is delivered by
// both paths (WebSocket push AND a racing heartbeat poll).
var (
	inFlightMu sync.Mutex
	inFlight   = make(map[string]bool)
)

// commandSink is where results and log batches get submitted. Both the HTTP
// client and the WS-with-fallback transport implement it.
type commandSink interface {
	SubmitResult(commandID string, result models.CommandResultRequest) error
	SubmitLogs(commandID string, lines []models.LogLine) error
	SubmitAppLogs(applicationID string, lines []models.LogLine) error
}

// transport prefers the WebSocket (instant, one connection) and transparently
// falls back to the HTTP endpoints whenever the socket is down or a write
// fails mid-flight — a dropped socket never loses results or logs.
type transport struct {
	ws   *ws.Client
	http *api.Client
}

func (t *transport) SubmitResult(id string, r models.CommandResultRequest) error {
	if t.ws != nil && t.ws.Connected() {
		if err := t.ws.SubmitResult(id, r); err == nil {
			return nil
		}
	}
	return t.http.SubmitResult(id, r)
}

func (t *transport) SubmitLogs(id string, lines []models.LogLine) error {
	if t.ws != nil && t.ws.Connected() {
		if err := t.ws.SubmitLogs(id, lines); err == nil {
			return nil
		}
	}
	return t.http.SubmitLogs(id, lines)
}

func (t *transport) SubmitAppLogs(appID string, lines []models.LogLine) error {
	if t.ws != nil && t.ws.Connected() {
		if err := t.ws.SubmitAppLogs(appID, lines); err == nil {
			return nil
		}
	}
	return t.http.SubmitAppLogs(appID, lines)
}

func main() {
	fmt.Printf("ThalesOps Agent starting... (version: %s)\n", Version)

	cfg := config.LoadConfig()
	if cfg.ServerID == "" || cfg.AgentToken == "" {
		log.Fatal("SERVER_ID and AGENT_TOKEN must be set")
	}

	client := api.NewClient(cfg)

	registerWithRetry(client, models.RegisterRequest{
		OSInfo: map[string]string{
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
			"version": runtime.Version(),
		},
		// Real, detected capabilities (docker/nixpacks presence + versions).
		Capabilities: system.Capabilities(),
		AgentVersion: Version,
	})

	// ── Real-time channel (WebSocket) ─────────────────────────────────────────
	// Commands are pushed down this socket the instant they're queued; results
	// and logs ride back up it. The heartbeat loop below keeps running as the
	// metrics reporter, liveness lease, and guaranteed delivery fallback.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr := &transport{http: client}
	if cfg.WSEnabled {
		wsClient := ws.New(cfg.BackendURL, cfg.ServerID, cfg.AgentToken, Version,
			system.Capabilities(), func(cmd models.AgentCommand) {
				dispatchCommand(tr, cmd, cfg)
			})
		tr.ws = wsClient
		go wsClient.Run(ctx)
	}

	currentInterval := time.Duration(cfg.Interval) * time.Second
	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf("Starting heartbeat loop (interval: %v, command timeout: %ds)...\n",
		currentInterval, cfg.CommandTimeout)

	for {
		select {
		case <-sigs:
			fmt.Println("Shutting down...")
			return
		case <-ticker.C:
			metrics := collectMetrics()
			newInterval := processHeartbeat(client, tr, metrics, cfg)
			if newInterval > 0 && newInterval != currentInterval {
				ticker.Stop()
				currentInterval = newInterval
				ticker = time.NewTicker(currentInterval)
				fmt.Printf("Heartbeat interval updated to %v\n", currentInterval)
			}
		}
	}
}

// registerWithRetry keeps trying to register until the backend accepts.
// Uses quadratic backoff capped at 60s. This handles the case where the agent
// starts before the network is fully up (e.g. on server reboot).
func registerWithRetry(client *api.Client, req models.RegisterRequest) {
	maxWait := 60 * time.Second
	for attempt := 1; ; attempt++ {
		fmt.Printf("Registering agent (attempt %d)...\n", attempt)
		if err := client.Register(req); err == nil {
			fmt.Println("Agent registered successfully.")
			return
		} else {
			wait := time.Duration(attempt*attempt) * time.Second
			if wait > maxWait {
				wait = maxWait
			}
			log.Printf("Registration failed: %v. Retrying in %v...", err, wait)
			time.Sleep(wait)
		}
	}
}

// collectMetrics gathers comprehensive system metrics from the host OS:
// CPU, RAM, swap, disk, network I/O, load average, uptime, and process count.
func collectMetrics() map[string]interface{} {
	metrics := make(map[string]interface{})

	// ── CPU ─────────────────────────────────────────────────────────────────────
	percent, err := cpu.Percent(time.Second, false)
	if err == nil && len(percent) > 0 {
		metrics["cpu_usage"] = fmt.Sprintf("%.1f%%", percent[0])
	}
	if counts, err := cpu.Counts(true); err == nil {
		metrics["cpu_cores"] = counts
	}

	// ── RAM ──────────────────────────────────────────────────────────────────────
	if v, err := mem.VirtualMemory(); err == nil {
		metrics["ram_usage"]    = fmt.Sprintf("%.1f%%", v.UsedPercent)
		metrics["ram_total_mb"] = v.Total / 1024 / 1024
		metrics["ram_used_mb"]  = v.Used / 1024 / 1024
		metrics["ram_free_mb"]  = v.Available / 1024 / 1024
	}

	// ── Swap ─────────────────────────────────────────────────────────────────────
	if s, err := mem.SwapMemory(); err == nil {
		metrics["swap_total_mb"] = s.Total / 1024 / 1024
		metrics["swap_used_mb"]  = s.Used / 1024 / 1024
		metrics["swap_usage"]    = fmt.Sprintf("%.1f%%", s.UsedPercent)
	}

	// ── Disk ─────────────────────────────────────────────────────────────────────
	if d, err := disk.Usage("/"); err == nil {
		metrics["disk_usage"]    = fmt.Sprintf("%.1f%%", d.UsedPercent)
		metrics["disk_total_gb"] = d.Total / 1024 / 1024 / 1024
		metrics["disk_used_gb"]  = d.Used / 1024 / 1024 / 1024
		metrics["disk_free_gb"]  = d.Free / 1024 / 1024 / 1024
	}

	// ── Network I/O ───────────────────────────────────────────────────────────────
	if netStats, err := net.IOCounters(false); err == nil && len(netStats) > 0 {
		metrics["net_bytes_sent"]   = netStats[0].BytesSent
		metrics["net_bytes_recv"]   = netStats[0].BytesRecv
		metrics["net_packets_sent"] = netStats[0].PacketsSent
		metrics["net_packets_recv"] = netStats[0].PacketsRecv
	}

	// ── Load Average ─────────────────────────────────────────────────────────────
	if l, err := load.Avg(); err == nil {
		metrics["load_1"]  = fmt.Sprintf("%.2f", l.Load1)
		metrics["load_5"]  = fmt.Sprintf("%.2f", l.Load5)
		metrics["load_15"] = fmt.Sprintf("%.2f", l.Load15)
	}

	// ── Uptime & Host Info ────────────────────────────────────────────────────────
	if info, err := host.Info(); err == nil {
		metrics["uptime_seconds"] = info.Uptime
		metrics["hostname"]       = info.Hostname
		metrics["os_platform"]    = info.Platform
		metrics["os_version"]     = info.PlatformVersion
		metrics["kernel_version"] = info.KernelVersion
	}

	// ── Process Count ────────────────────────────────────────────────────────────
	if procs, err := process.Pids(); err == nil {
		metrics["process_count"] = len(procs)
	}

	// ── Open Connections ─────────────────────────────────────────────────────────
	if conns, err := net.Connections("all"); err == nil {
		metrics["open_connections"] = len(conns)
	}

	return metrics
}


// processHeartbeat sends metrics to the backend and dispatches any returned commands.
// Returns the backend-requested heartbeat interval (0 if unchanged).
// When the WebSocket is up this normally returns zero commands (the socket got
// them first) — but it remains the guaranteed delivery path when the socket is down.
func processHeartbeat(client *api.Client, sink commandSink, metrics map[string]interface{}, cfg *config.Config) time.Duration {
	fmt.Printf("Heartbeat: %v\n", metrics)

	resp, err := client.Heartbeat(models.HeartbeatRequest{
		Metrics:      metrics,
		AgentVersion: Version,
	})
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
		return 0
	}

	if len(resp.Data.Commands) > 0 {
		fmt.Printf("Received %d command(s)\n", len(resp.Data.Commands))
		for _, cmd := range resp.Data.Commands {
			dispatchCommand(sink, cmd, cfg)
		}
	}

	if resp.Data.HeartbeatInterval > 0 {
		return time.Duration(resp.Data.HeartbeatInterval) * time.Second
	}
	return 0
}

// dispatchCommand runs a command in its own goroutine so neither the heartbeat
// loop nor the WS read loop is ever blocked by long-running operations.
// The inFlight map prevents the same command from executing twice if it is
// delivered by both paths (WS push + heartbeat) before the first run completes.
func dispatchCommand(client commandSink, cmd models.AgentCommand, cfg *config.Config) {
	inFlightMu.Lock()
	if inFlight[cmd.ID] {
		inFlightMu.Unlock()
		fmt.Printf("Command %s already in flight, skipping\n", cmd.ID)
		return
	}
	inFlight[cmd.ID] = true
	inFlightMu.Unlock()

	go func() {
		defer func() {
			inFlightMu.Lock()
			delete(inFlight, cmd.ID)
			inFlightMu.Unlock()
		}()

		fmt.Printf("Executing command: %s (%s)\n", cmd.ID, cmd.CommandType)
		timeout := time.Duration(cfg.CommandTimeout) * time.Second

		var result models.CommandResultRequest
		switch cmd.CommandType {
		case "SHELL":
			result = executor.ExecuteShell(cmd.Payload, timeout)
		case "DEPLOY":
			// Deploys can run for minutes (clone + build + run), so they use a
			// longer timeout and stream their logs back as they run.
			deployTimeout := time.Duration(cfg.DeployTimeout) * time.Second
			result = executor.ExecuteDeploy(cmd.Payload, deployTimeout, func(lines []models.LogLine) error {
				return client.SubmitLogs(cmd.ID, lines)
			})
		case "RESTART":
			// Restart reuses the existing image (no build), so the normal command
			// timeout is plenty. Streams its logs the same way.
			result = executor.ExecuteRestart(cmd.Payload, timeout, func(lines []models.LogLine) error {
				return client.SubmitLogs(cmd.ID, lines)
			})
		case "STREAM_LOGS":
			// Tail the running app's own logs to the app-log buffer for a bounded
			// session. Logs go to the app-log endpoint, keyed by application_id.
			appID, _ := cmd.Payload["application_id"].(string)
			result = executor.ExecuteStreamLogs(cmd.Payload, func(lines []models.LogLine) error {
				return client.SubmitAppLogs(appID, lines)
			})
		default:
			result = models.CommandResultRequest{
				ExitCode: 1,
				Stderr:   fmt.Sprintf("Unsupported command type: %s", cmd.CommandType),
			}
		}

		fmt.Printf("Command %s finished (exit code: %d)\n", cmd.ID, result.ExitCode)
		if err := client.SubmitResult(cmd.ID, result); err != nil {
			log.Printf("Failed to submit result for command %s: %v", cmd.ID, err)
		}
	}()
}
