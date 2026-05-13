package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/mem"
	"github.com/thalesops/agent/internal/api"
	"github.com/thalesops/agent/internal/config"
	"github.com/thalesops/agent/internal/executor"
	"github.com/thalesops/agent/internal/models"
)

func main() {
	fmt.Println("ThalesOps Agent starting...")

	cfg := config.LoadConfig()
	if cfg.ServerID == "" || cfg.AgentToken == "" {
		log.Fatal("SERVER_ID and AGENT_TOKEN must be set")
	}

	client := api.NewClient(cfg)

	// 1. Register
	fmt.Println("Registering agent...")
	err := client.Register(models.RegisterRequest{
		OSInfo: map[string]string{
			"os":      runtime.GOOS,
			"arch":    runtime.GOARCH,
			"version": runtime.Version(),
		},
		Capabilities: map[string]interface{}{
			"shell":  true,
			"docker": true,
		},
	})
	if err != nil {
		log.Fatalf("Registration failed: %v", err)
	}
	fmt.Println("Agent registered successfully.")

	// 2. Heartbeat loop
	ticker := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	defer ticker.Stop()

	// Signal handling for graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("Starting heartbeat loop...")
	for {
		select {
		case <-sigs:
			fmt.Println("Shutting down...")
			return
		case <-ticker.C:
			metrics := collectMetrics()
			processHeartbeat(client, metrics)
		}
	}
}

func collectMetrics() map[string]interface{} {
	metrics := make(map[string]interface{})

	// CPU Usage
	percent, err := cpu.Percent(time.Second, false)
	if err == nil && len(percent) > 0 {
		metrics["cpu_usage"] = fmt.Sprintf("%.1f%%", percent[0])
	}

	// Memory Usage
	v, err := mem.VirtualMemory()
	if err == nil {
		metrics["ram_usage"] = fmt.Sprintf("%.1f%%", v.UsedPercent)
		metrics["ram_total_mb"] = v.Total / 1024 / 1024
		metrics["ram_free_mb"] = v.Available / 1024 / 1024
	}

	return metrics
}

func processHeartbeat(client *api.Client, metrics map[string]interface{}) {
	fmt.Printf("Heartbeat: Sending metrics: %v\n", metrics)
	resp, err := client.Heartbeat(models.HeartbeatRequest{
		Metrics: metrics,
	})
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
		return
	}

	if len(resp.Data.Commands) > 0 {
		fmt.Printf("Received %d commands\n", len(resp.Data.Commands))
		for _, cmd := range resp.Data.Commands {
			fmt.Printf("Executing command: %s (%s)\n", cmd.ID, cmd.CommandType)
			
			var result models.CommandResultRequest
			switch cmd.CommandType {
			case "SHELL":
				result = executor.ExecuteShell(cmd.Payload)
			default:
				result = models.CommandResultRequest{
					ExitCode: 1,
					Stderr:   fmt.Sprintf("Unsupported command type: %s", cmd.CommandType),
				}
			}

			fmt.Printf("Command %s finished with exit code %d. Submitting result...\n", cmd.ID, result.ExitCode)
			err := client.SubmitResult(cmd.ID, result)
			if err != nil {
				log.Printf("Failed to submit result for command %s: %v", cmd.ID, err)
			}
		}
	}
}
