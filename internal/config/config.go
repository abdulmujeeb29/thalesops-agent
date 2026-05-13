package config

import (
	"os"
	"strings"
)

type Config struct {
	BackendURL  string
	ServerID    string
	AgentToken  string
	Interval    int
}

func LoadConfig() *Config {
	backendURL := getEnv("BACKEND_URL", "http://localhost:8000")
	// Ensure no trailing slash
	backendURL = strings.TrimSuffix(backendURL, "/")

	return &Config{
		BackendURL: backendURL,
		ServerID:   getEnv("SERVER_ID", ""),
		AgentToken: getEnv("AGENT_TOKEN", ""),
		Interval:   60, // Default heartbeat interval
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
