package models

type RegisterRequest struct {
	OSInfo       map[string]string      `json:"os_info"`
	Capabilities map[string]interface{} `json:"capabilities"`
	AgentVersion string                 `json:"agent_version,omitempty"`
}

type HeartbeatRequest struct {
	OSInfo       map[string]string      `json:"os_info,omitempty"`
	Capabilities map[string]interface{} `json:"capabilities,omitempty"`
	Metrics      map[string]interface{} `json:"metrics,omitempty"`
	AgentVersion string                 `json:"agent_version,omitempty"`
}

type AgentCommand struct {
	ID          string                 `json:"id"`
	CommandType string                 `json:"command_type"`
	Payload     map[string]interface{} `json:"payload"`
}

type HeartbeatResponse struct {
	Data struct {
		HeartbeatInterval int            `json:"heartbeat_interval"`
		Commands          []AgentCommand `json:"commands"`
	} `json:"data"`
}

type CommandResultRequest struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type SuccessResponse struct {
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

// LogLine is a single streamed line of deploy output.
type LogLine struct {
	Stream  string `json:"stream"` // "stdout" | "stderr" | "system"
	Content string `json:"content"`
}

// CommandLogBatch is the body sent to the agent log-streaming endpoint.
type CommandLogBatch struct {
	Logs []LogLine `json:"logs"`
}

// AppLogBatch is the body sent to the agent runtime-app-log endpoint.
type AppLogBatch struct {
	ApplicationID string    `json:"application_id"`
	Logs          []LogLine `json:"logs"`
}

// StreamLogsPayload is the typed view of a STREAM_LOGS command's payload.
type StreamLogsPayload struct {
	ApplicationID   string
	AppSlug         string
	Tail            int
	DurationSeconds int
}

// DeployPayload is the typed view of a DEPLOY/RESTART command's payload.
// Parsed defensively from the generic map the backend sends.
type DeployPayload struct {
	DeploymentID string
	AppType      string // "SERVICE" (default, container) or "STATIC" (files via proxy)
	AppSlug      string
	RepoFullName string
	CloneURL     string
	Branch       string
	Commit       string // if set, check out this exact commit instead of branch HEAD
	ImageTag     string // SERVICE: versioned image tag. STATIC: the release-folder id.
	BuildMethod  string

	// STATIC only: optional build step + which folder to publish.
	BuildCommand string // e.g. "npm run build"; empty = no build (plain HTML)
	PublishDir   string // relative to repo root; empty = repo root
	Port         int      // container's internal port
	HostPort     int      // localhost port to publish on (the proxy routes here)
	Domains      []string // hostnames the reverse proxy should route to this app

	// Intelligent deployment (detection-driven, user-confirmed on the backend):
	RunMigrations    bool   // run MigrationCommand before swapping in the new build
	MigrationCommand string // e.g. "python manage.py migrate"
	HealthCheckPath  string // HTTP path polled for a 2xx before go-live ('' = TCP-only)

	Env map[string]string
}
