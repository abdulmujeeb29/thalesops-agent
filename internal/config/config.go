package config

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	BackendURL     string
	ServerID       string
	AgentToken     string
	Interval       int
	CommandTimeout int
	DeployTimeout  int
	// WSEnabled turns the persistent WebSocket channel on (default). With it
	// off — or whenever the socket is down — the agent behaves exactly as
	// before: heartbeat polling alone.
	WSEnabled bool
}

func LoadConfig() *Config {
	loadDotEnv()

	backendURL := getEnv("BACKEND_URL", "http://localhost:8000")
	backendURL = strings.TrimSuffix(backendURL, "/")

	interval := 60
	if v := getEnv("HEARTBEAT_INTERVAL", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			interval = n
		}
	}

	commandTimeout := 300
	if v := getEnv("COMMAND_TIMEOUT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			commandTimeout = n
		}
	}

	// Deploys (clone + build + run) can legitimately take several minutes.
	// Kept under the backend's 30-min RUNNING-timeout so the agent fails first.
	deployTimeout := 1200
	if v := getEnv("DEPLOY_TIMEOUT", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			deployTimeout = n
		}
	}

	// Opt-out flag: WS_ENABLED=false disables the real-time channel entirely.
	wsEnabled := !strings.EqualFold(getEnv("WS_ENABLED", "true"), "false")

	return &Config{
		BackendURL:     backendURL,
		ServerID:       getEnv("SERVER_ID", ""),
		AgentToken:     getEnv("AGENT_TOKEN", ""),
		Interval:       interval,
		CommandTimeout: commandTimeout,
		DeployTimeout:  deployTimeout,
		WSEnabled:      wsEnabled,
	}
}

// loadDotEnv reads a .env file in the current directory and sets any env vars
// that are not already set. Existing OS env vars always take precedence.
// Silently does nothing if .env does not exist (production uses systemd env).
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
