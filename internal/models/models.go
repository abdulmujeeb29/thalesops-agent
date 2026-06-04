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
