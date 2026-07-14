// Package ws maintains the agent's persistent WebSocket to the backend — the
// real-time channel that removes the heartbeat pickup lag.
//
// The agent DIALS OUT (so it traverses NAT/firewalls) and holds the socket
// open. Down the socket come commands, pushed the instant they're queued; up
// the socket go results and log batches. The HTTP heartbeat keeps running
// underneath as the guaranteed fallback: when this socket is down, everything
// still works at heartbeat speed.
package ws

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thalesops/agent/internal/models"
)

const (
	writeWait  = 10 * time.Second // deadline for a single write
	pongWait   = 90 * time.Second // read deadline; refreshed by pongs/messages
	pingPeriod = 30 * time.Second // client ping cadence (must be < pongWait)
	maxBackoff = 60 * time.Second
)

// envelope is the one JSON frame shape used in both directions.
type envelope struct {
	Type          string                       `json:"type"`
	Command       *models.AgentCommand         `json:"command,omitempty"`
	CommandID     string                       `json:"command_id,omitempty"`
	ApplicationID string                       `json:"application_id,omitempty"`
	Result        *models.CommandResultRequest `json:"result,omitempty"`
	Lines         []models.LogLine             `json:"lines,omitempty"`
	AgentVersion  string                       `json:"agent_version,omitempty"`
	Capabilities  map[string]interface{}       `json:"capabilities,omitempty"`
}

// Client dials and maintains the socket, reconnecting with backoff forever.
type Client struct {
	url          string
	header       http.Header
	agentVersion string
	capabilities map[string]interface{}
	onCommand    func(models.AgentCommand)

	mu   sync.Mutex // guards conn (gorilla allows one concurrent writer)
	conn *websocket.Conn
}

// New builds a client. backendURL is the normal http(s) API base; the WS URL
// is derived from it. onCommand is invoked for every pushed command (it must
// be non-blocking — main's dispatchCommand already runs work in goroutines).
func New(backendURL, serverID, agentToken, agentVersion string,
	capabilities map[string]interface{}, onCommand func(models.AgentCommand)) *Client {

	wsURL := strings.Replace(backendURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)

	h := http.Header{}
	h.Set("X-Server-ID", serverID)
	h.Set("X-Agent-Token", agentToken)

	return &Client{
		url:          wsURL + "/ws/agent/",
		header:       h,
		agentVersion: agentVersion,
		capabilities: capabilities,
		onCommand:    onCommand,
	}
}

// Run keeps the socket alive until ctx is cancelled: dial → serve → backoff →
// redial. Never returns an error — a dead socket just means fallback mode.
func (c *Client) Run(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.url, c.header)
		if err != nil {
			attempt++
			wait := backoff(attempt)
			log.Printf("ws: connect failed (%v) — retrying in %v (heartbeat fallback active)", err, wait)
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return
			}
		}

		attempt = 0
		log.Printf("ws: connected — commands now arrive instantly")
		c.setConn(conn)
		_ = c.Send(envelope{Type: "hello", AgentVersion: c.agentVersion, Capabilities: c.capabilities})

		c.serve(ctx, conn) // blocks until the connection dies
		c.setConn(nil)
		log.Printf("ws: disconnected — falling back to heartbeat delivery")
	}
}

// serve runs the read loop + keepalive pings until the connection errors.
func (c *Client) serve(ctx context.Context, conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	done := make(chan struct{})
	go func() { // keepalive pings; also notices ctx cancellation
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.mu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait))
				c.mu.Unlock()
				if err != nil {
					_ = conn.Close()
					return
				}
			case <-ctx.Done():
				_ = conn.Close()
				return
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	for {
		var env envelope
		if err := conn.ReadJSON(&env); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		if env.Type == "command" && env.Command != nil && c.onCommand != nil {
			c.onCommand(*env.Command)
		}
	}
}

func (c *Client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

// Connected reports whether the socket is currently up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Send writes one frame. Returns an error when disconnected (callers fall back
// to HTTP). Serialised by the mutex — gorilla allows one writer at a time.
func (c *Client) Send(v interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("ws: not connected")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.conn.WriteJSON(v); err != nil {
		// A failed write means the conn is dying; close so the read loop exits
		// promptly and Run() reconnects.
		_ = c.conn.Close()
		return err
	}
	return nil
}

// SubmitResult / SubmitLogs / SubmitAppLogs mirror the HTTP client's API so a
// transport wrapper can swap between them freely.

func (c *Client) SubmitResult(commandID string, result models.CommandResultRequest) error {
	return c.Send(envelope{Type: "result", CommandID: commandID, Result: &result})
}

func (c *Client) SubmitLogs(commandID string, lines []models.LogLine) error {
	return c.Send(envelope{Type: "logs", CommandID: commandID, Lines: lines})
}

func (c *Client) SubmitAppLogs(applicationID string, lines []models.LogLine) error {
	return c.Send(envelope{Type: "app_logs", ApplicationID: applicationID, Lines: lines})
}

// backoff: exponential with jitter, capped. attempt 1 → ~1s, 2 → ~2s, 3 → ~4s…
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(min(attempt-1, 6))) * time.Second
	if d > maxBackoff {
		d = maxBackoff
	}
	// ±25% jitter so a fleet doesn't reconnect in lockstep after a backend restart.
	jitter := time.Duration(rand.Int63n(int64(d) / 2))
	return d/4*3 + jitter
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
