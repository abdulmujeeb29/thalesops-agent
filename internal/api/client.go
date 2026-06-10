package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/thalesops/agent/internal/config"
	"github.com/thalesops/agent/internal/models"
)

type Client struct {
	config *config.Config
	http   *http.Client
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		config: cfg,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) doRequest(method, path string, body interface{}) (*http.Response, error) {
	url := fmt.Sprintf("%s%s", c.config.BackendURL, path)
	
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Token", c.config.AgentToken)
	req.Header.Set("X-Server-ID", c.config.ServerID)

	return c.http.Do(req)
}

func (c *Client) Register(req models.RegisterRequest) error {
	resp, err := c.doRequest("POST", "/api/v1/agent/register/", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registration failed with status: %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) Heartbeat(req models.HeartbeatRequest) (*models.HeartbeatResponse, error) {
	resp, err := c.doRequest("POST", "/api/v1/agent/heartbeat/", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("heartbeat failed with status: %d", resp.StatusCode)
	}

	var hbResp models.HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hbResp); err != nil {
		return nil, err
	}
	return &hbResp, nil
}

func (c *Client) SubmitResult(commandID string, result models.CommandResultRequest) error {
	path := fmt.Sprintf("/api/v1/agent/commands/%s/result/", commandID)
	resp, err := c.doRequest("POST", path, result)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("submitting result failed with status: %d", resp.StatusCode)
	}
	return nil
}

// SubmitLogs streams a batch of log lines for a running command.
// Best-effort: callers should not abort a deploy if this fails.
func (c *Client) SubmitLogs(commandID string, lines []models.LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	path := fmt.Sprintf("/api/v1/agent/commands/%s/logs/", commandID)
	resp, err := c.doRequest("POST", path, models.CommandLogBatch{Logs: lines})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("submitting logs failed with status: %d", resp.StatusCode)
	}
	return nil
}
