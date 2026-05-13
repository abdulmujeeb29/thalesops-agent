# ThalesOps Agent

A lightweight, high-performance Go agent designed to manage remote servers, collect metrics, and execute commands from the ThalesOps platform.

## Architecture & File Structure

The agent is built using a modular "internal" package structure, which is a Go best practice to keep code organized and maintainable.

```text
thalesops-agent/
├── main.go               # Entry point and the core Heartbeat loop
├── go.mod                # Dependency management (like requirements.txt)
├── internal/
│   ├── api/              # HTTP Client: Handles talking to the Django API
│   ├── config/           # Configuration: Reads .env and environment variables
│   ├── executor/         # Shell Executor: Runs commands on the server
│   ├── models/           # Data Models: Go structs that match the API JSON
│   └── metrics/          # (Planned) Dedicated metrics collection logic
```

## How It Works

### 1. Registration
When you run `go run main.go`, the agent first calls the `Register()` function. It collects basic information about the server:
- Operating System (e.g., Linux, Darwin)
- CPU Architecture (e.g., amd64, arm64)
- Agent Capabilities (currently `shell` and `docker`)

This information is sent to `/api/v1/agent/register/` to let the backend know the server is ready.

### 2. The Heartbeat Loop
Once registered, the agent enters a loop. Every 60 seconds (by default), it performs a "Heartbeat":
- **Metrics Collection**: It uses the `gopsutil` library to get real-time CPU and RAM usage.
- **Reporting**: It sends these metrics to `/api/v1/agent/heartbeat/`.
- **Command Retrieval**: The backend responds with any pending commands queued for this server.

### 3. Command Execution
If the heartbeat response contains commands (e.g., a `SHELL` command):
1. The agent passes the command to the `executor` package.
2. The executor runs the command (e.g., `sh -c "uptime"`).
3. The executor captures the `exit_code`, `stdout` (normal output), and `stderr` (error output).
4. The agent immediately calls `/api/v1/agent/commands/{id}/result/` to send the results back to your dashboard.

### 4. Authentication & Security
The agent uses two custom headers for every request:
- `X-Agent-Token`: A secret key unique to this server.
- `X-Server-ID`: The UUID of this server in the ThalesOps database.

This ensures that only authorized agents can communicate with your platform.

## Development

### Prerequisites
- Go 1.21 or higher
- Access to the ThalesCore Django backend

### Local Setup
1. Copy `.env.example` to `.env`.
2. Fill in your `SERVER_ID` and `AGENT_TOKEN` from the ThalesOps dashboard.
3. Run the agent:
   ```bash
   export $(cat .env | xargs) && go run main.go
   ```

### Adding New Command Types
To support a new type of command (e.g., `DOCKER_DEPLOY`):
1. Update the `switch` statement in `main.go` to recognize the new type.
2. Add the corresponding logic in a new file within the `internal/executor/` package.
