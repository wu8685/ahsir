# Example: Student-Teacher Multi-Agent Setup

A minimal multi-agent example using AHSIR. Two agents — **Student** and **Teacher** — collaborate via the [A2A protocol](https://google.github.io/A2A/):

- **Teacher**: Answers questions, summarizes articles, explains concepts.
- **Student**: Receives user requests. When it needs help, delegates to the Teacher via `---A2A_CALL---`.

## Prerequisites

- **Go** >= 1.24.4
- **Claude Code CLI** (`claude`) — required for the Student agent to call the Teacher at runtime
- Both `claude` and `go` must be in your `PATH`

## Quick Start

### 1. Build the binaries

```bash
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent
```

### 2. Start the scheduler

The scheduler reads `example/ahsir.yaml`, starts both agents, and runs the registry on port 9800:

```bash
./bin/ahsir start example/ahsir.yaml
```

The scheduler launches each agent as a subprocess, passing `--registry` so the agent knows where to list and call other agents.

### 3. Interact with agents

#### Option A: Direct HTTP (A2A JSON-RPC)

Each agent exposes an A2A JSON-RPC endpoint. Check the scheduler logs for the port each agent is assigned.

```bash
# Send a message to the Student
curl -s -X POST http://127.0.0.1:<student-port> \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "Summarize the article in /tmp/article.md"}]
      }
    },
    "id": 1
  }'

# Send a message directly to the Teacher
curl -s -X POST http://127.0.0.1:<teacher-port> \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "What is a goroutine?"}]
      }
    },
    "id": 1
  }'
```

#### Option B: Registry HTTP API

List agents and check liveness via the registry:

```bash
# List all registered agents
curl -s http://127.0.0.1:9800/agents

# Get a specific agent
curl -s http://127.0.0.1:9800/agents/student
```

#### Option C: MCP stdio (Claude Code)

The project root already contains `.mcp.json` with the ahsir server configured:

```json
{
  "mcpServers": {
    "ahsir": {
      "command": "/path/to/bin/ahsir",
      "args": ["start", "example/ahsir.yaml"]
    }
  }
}
```

When you open this project in Claude Code, it will detect `.mcp.json` and prompt you to approve the ahsir MCP server. Once approved, you can type: "Ask the student to summarize the article in /tmp/article.md"

### 4. Shut down

Press `Ctrl+C` in the scheduler terminal. The scheduler stops the registry and kills all agent subprocesses.

## Run the Tests

```bash
go test ./example/ -v
```

This runs three tests:

| Test | What it verifies |
|------|-----------------|
| `TestLoadAgentCards` | Both `teacher` and `student` agent-card YAMLs are valid |
| `TestLoadSchedulerConfig` | `ahsir.yaml` is valid and contains both agents |
| `TestStudentDelegatesToTeacher` | Student receives a message, delegates to Teacher via A2A_CALL, returns the answer |

The integration test uses mock session handlers — it does not require the real `claude` CLI.

Generated with [Claude Code](https://claude.com/claude-code)
