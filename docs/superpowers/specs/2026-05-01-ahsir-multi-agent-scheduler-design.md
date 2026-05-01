# AHSIR: Multi-Agent Scheduler based on A2A Protocol

**Status:** Draft
**Date:** 2026-05-01
**Version:** 1.0.0

## 1. Overview

AHSIR is a multi-agent scheduler that enables multiple Claude Code instances (agents) to communicate and collaborate using the [A2A (Agent2Agent) Protocol](https://github.com/a2aproject/A2A). Each agent runs in an isolated workspace directory and communicates via A2A JSON-RPC 2.0 over HTTP. The user interacts with agents through their local Claude Code session via MCP (Model Context Protocol).

### Goals

- Isolate each agent in its own workspace with its own Claude Code instance
- Enable user-to-agent communication via local Claude Code + MCP
- Enable agent-to-agent collaboration via A2A protocol (structured JSON call format)
- V1 runs entirely on localhost; V2 supports multi-machine deployment

## 2. Architecture

```
+------------------------------------------------------------+
|                   User's Local Claude Code                  |
|                  (MCP Client --> Scheduler MCP)             |
+----------------------------------+-------------------------+
                                   | MCP (agent_list / agent_chat)
                                   v
+------------------------------------------------------------+
|                     Scheduler (ahsir)                       |
|  +--------------+  +----------------+  +-----------------+  |
|  |  Lifecycle   |  |   Registry     |  |   MCP Server    |  |
|  |  Manager     |  |  (AgentCards)  |  |  (stdio)        |  |
|  +------+-------+  +-------+--------+  +-----------------+  |
+---------+------------------+--------------------------------+
          | start/stop       | register AgentCard + lookup
          v                  v
+------------------+  +------------------+  +------------------+
|  Agent Wrapper   |  |  Agent Wrapper   |  |  Agent Wrapper   |
|  (backend/)      |<>|  (frontend/)     |  |  (data/)         |
|                  |A2A                 |A2A                |
|  Claude Code     |  |  Claude Code     |  |  Claude Code     |
|  (persistent)    |  |  (persistent)    |  |  (persistent)    |
+------------------+  +------------------+  +------------------+
```

### Components

| Component | Binary | Responsibility |
|-----------|--------|----------------|
| Scheduler | `ahsir` | Lifecycle management, Registry, MCP Server |
| Agent Wrapper | `ahsir-agent` | A2A Server + Client, Claude Code session management |

## 3. Agent Wrapper

Each agent workspace runs an `ahsir-agent` process that wraps a Claude Code instance as an A2A-compliant HTTP server.

### 3.1 Internal Structure

```
+----------------------------------------------------------+
|                    Agent Wrapper                          |
|                                                           |
|  +-------------+  +-------------+  +-------------------+  |
|  | A2A Server  |  | A2A Client  |  | Session Manager   |  |
|  | (accept reqs)|  | (call others)|  | (Claude Code proc)| |
|  +------+------+  +------+------+  +---------+---------+  |
|         |                |                   |             |
|         v                v                   v             |
|  +------------------------------------------------------+  |
|  |                 Prompt Construction Layer             |  |
|  |  Inject available agents list into system prompt      |  |
|  |  Parse --A2A_CALL-- from Claude Code output           |  |
|  |  Execute A2A calls, inject results back               |  |
|  +------------------------------------------------------+  |
|                                                           |
|  +-------------+  +-------------------------------------+  |
|  |  Task Store |  |  AgentCard Builder                  |  |
|  |  (in-memory)|  |  (.a2a/agent-card.yaml + runtime)   |  |
|  +-------------+  +-------------------------------------+  |
+----------------------------------------------------------+
```

### 3.2 Claude Code Session Management

- **Persistent Session**: Claude Code runs as a long-lived child process. stdin/stdout pipes are maintained for multi-turn conversations.
- **Request Serialization**: All A2A requests are queued and processed serially (Claude Code does not support concurrent conversations).
- **Crash Recovery**: If the Claude Code process crashes, the wrapper automatically restarts it. In-flight tasks transition to `TASK_STATE_FAILED`.
- **Timeout**: If Claude Code produces no output within a configurable timeout, the task transitions to `TASK_STATE_FAILED`.

### 3.3 A2A <-> Claude Code Translation

**Inbound (A2A -> Claude Code):**

1. Receive JSON-RPC `message/send` request
2. Extract Parts from the Message, concatenate text parts into a prompt
3. New Task: write prompt to Claude Code stdin directly
4. Continuation Task (same contextId): Claude Code session already has context; write follow-up message
5. Map A2A contextId to Claude Code session (one contextId = one conversation turn within the persistent session)

**Outbound (Claude Code -> A2A):**

1. Read Claude Code stdout
2. If output contains `---A2A_CALL---` block: parse the JSON, execute A2A call to target agent, inject result back to Claude Code, loop
3. If no A2A_CALL: wrap output in A2A `Message` (role: agent) or `Artifact`
4. Map Claude Code state to A2A Task state

### 3.4 Task Lifecycle Mapping

```
Claude Code State            A2A Task State
----------------             --------------
Received new prompt          TASK_STATE_SUBMITTED
Processing / generating      TASK_STATE_WORKING
Output contains --A2A_CALL-- TASK_STATE_INPUT_REQUIRED (waiting for sub-agent)
Completed normally           TASK_STATE_COMPLETED
Error / timeout / crash      TASK_STATE_FAILED
External cancel request      TASK_STATE_CANCELED
```

### 3.5 Streaming

Claude Code's token-by-token output is forwarded as SSE events:

- Accumulated tokens -> `TaskArtifactUpdateEvent` with `append: true`
- Final token -> `TaskArtifactUpdateEvent` with `lastChunk: true`

### 3.6 Agent-to-Agent Calling Protocol

The wrapper injects available agents into Claude Code's system prompt:

```
You can call the following agents for help:
- name: "backend", skills: ["api-design"], endpoint: "http://127.0.0.1:9801/"
- name: "data", skills: ["sql"], endpoint: "http://127.0.0.1:9802/"

When you need another agent's help, append to your response:
---A2A_CALL---
{"agent": "<name>", "task": "<description of what you need>"}
---END---
```

**Processing flow:**

1. Parse Claude Code stdout for `---A2A_CALL---` markers
2. Extract JSON payload with target agent name and task description
3. A2A Client sends `message/send` to the target agent via A2A protocol
4. Wait for task completion, collect result
5. Inject result as context: `[Agent X returned: <result>]`
6. Feed back into Claude Code stdin for continued processing

**Safety:** Max chain depth of 5 agent calls. If exceeded, terminate and return partial results.

### 3.7 Workspace Configuration

Each agent workspace contains `.a2a/agent-card.yaml`:

```yaml
name: "Backend Agent"
description: "Go backend development, API design, database operations"
version: "1.0.0"
provider:
  name: "ahsir"
  url: "https://github.com/wu8685/ahsir"

skills:
  - name: "api-design"
    description: "Design and implement RESTful APIs"
  - name: "database-schema"
    description: "Database schema design and migration"
  - name: "code-review"
    description: "Review Go backend code"

claude:
  systemPrompt: |
    You are a Go backend developer. You can review code, design APIs, and write database operations.
    Call the frontend agent when you need frontend support.
  maxAgentCalls: 5

# V1 default (localhost only)
network:
  bind: "127.0.0.1"
  # advertise: ""  # defaults to bind; for multi-machine, set to reachable IP
```

## 4. Scheduler

### 4.1 Configuration

`ahsir.yaml` (default config, can be overridden):

```yaml
agents:
  - name: backend
    workspace: /home/user/projects/backend
    port: 0          # 0 = auto-allocate from port range
  - name: frontend
    workspace: /home/user/projects/frontend
    port: 0
  # V2: remote agent
  # - name: remote-data
  #   remote: "http://192.168.1.10:9801/"

registry:
  host: "127.0.0.1"
  port: 9800
  heartbeat_interval: 10s
  heartbeat_timeout: 30s

mcp:
  # MCP uses stdio transport, configured in Claude Code settings

port_range:
  start: 9801
  end: 9900
```

### 4.2 Lifecycle Management

1. Parse config, allocate ports for each agent
2. For each local agent: `ahsir agent start --workspace=<path> --port=<port> --registry=<registry_url>`
3. Monitor agent health via heartbeat
4. On `ahsir stop`: send SIGTERM to all agent processes, wait for graceful shutdown

### 4.3 Registry API

```
POST   /agents              Register agent (AgentCard in body)
GET    /agents               List all registered agents (AgentCard summaries)
GET    /agents/:name         Get specific agent's full AgentCard
DELETE /agents/:name         Unregister agent
```

### 4.4 MCP Server

The scheduler exposes an MCP server over stdio transport for the user's local Claude Code.

**Tools:**

| Tool | Parameters | Description |
|------|-----------|-------------|
| `agent_list` | — | List all registered agents with name, description, skills, status |
| `agent_chat` | `agent_name`, `message` | Send a message to a specific agent, return response |
| `agent_task_status` | `agent_name`, `task_id` | Query a task's status on a specific agent |

### 4.5 Agent Discovery Flow (Agent A -> Agent B)

1. Agent A queries Registry: `GET /agents`
2. Agent A gets Agent B's AgentCard with endpoint URL
3. Agent A sends A2A request directly to Agent B's endpoint
4. No scheduler involvement after discovery

## 5. User Interaction Flow

1. User starts scheduler: `ahsir start`
2. User opens Claude Code in any project
3. User says: "Ask the backend agent to design a user API"
4. Claude Code (via MCP) calls `agent_chat("backend", "design a user API")`
5. Scheduler routes to Backend Agent Wrapper via A2A `message/send`
6. Backend Agent's Claude Code processes the request
7. Response flows back through A2A -> MCP -> user's Claude Code session

## 6. V2 Multi-Machine Extension Points

Design decisions that enable multi-machine deployment:

1. **`network.bind` vs `network.advertise`**: Bind is the local listen address (default `127.0.0.1`); advertise is the address registered in Registry (for other machines to reach). V1 both are `127.0.0.1`; V2 advertise can be a routable IP.

2. **Registry as standalone service**: V1 Registry is embedded in scheduler. Its HTTP API is already defined. V2 can deploy Registry as an independent service that all agents (across machines) register with.

3. **Remote agent config**: Config supports `remote` field for agents that the scheduler does not manage (already running elsewhere). Scheduler only monitors their health.

4. **No hardcoded localhost**: All URLs are constructed from config, not hardcoded. The registry URL is passed to agents at startup.

## 7. Error Handling

| Layer | Error | Handling |
|-------|-------|----------|
| Claude Code process | Crash / hang | Auto-restart; in-flight tasks -> FAILED |
| Claude Code session | Output timeout | Configurable timeout; task -> FAILED |
| A2A Client call | Target agent unreachable | Retry 3 times; on failure, inject error into Claude Code context |
| Agent chain calls | Exceed maxAgentCalls | Terminate chain; return partial results |
| Registry heartbeat | Timeout | Mark agent offline; notify subscribers |
| MCP | Scheduler not running | Local Claude Code prompts user to start ahsir |

## 8. Security

- **V1 (localhost)**: All HTTP listeners bind to `127.0.0.1`. No authentication required. No data leaves the machine.
- **V2 (multi-machine)**: Authentication via A2A `securitySchemes` in AgentCard (TLS + API key / OAuth). TBD when needed.

## 9. Project Structure

```
ahsir/
├── cmd/
│   ├── ahsir/                # Scheduler binary
│   │   └── main.go
│   └── ahsir-agent/          # Agent Wrapper binary
│       └── main.go
├── internal/
│   ├── scheduler/            # Scheduler logic
│   │   ├── config.go
│   │   ├── scheduler.go
│   │   └── process.go
│   ├── registry/             # Agent registry
│   │   └── registry.go
│   ├── mcp/                  # MCP Server (stdio transport)
│   │   └── server.go
│   ├── wrapper/              # Agent Wrapper
│   │   ├── server.go         #   A2A JSON-RPC Server
│   │   ├── client.go         #   A2A Client (call other agents)
│   │   ├── session.go        #   Claude Code subprocess management
│   │   ├── prompt.go         #   Prompt construction & --A2A_CALL-- parsing
│   │   └── card.go           #   AgentCard reading & building
│   ├── a2a/                  # A2A protocol types
│   │   ├── types.go          #   Task, Message, Part, AgentCard, etc.
│   │   ├── jsonrpc.go        #   JSON-RPC 2.0 codec
│   │   └── errors.go         #   A2A standard error codes
│   └── transport/            # HTTP transport
│       └── http.go           #   HTTP Server + Client
├── go.mod
├── go.sum
└── ahsir.yaml                # Default config
```

## 10. Open Questions / Future Work

- Claude Code stdout parsing reliability: how to reliably detect the end of a response vs waiting for more tokens
- Session persisting across Agent Wrapper restarts: should conversation history survive a crash?
- Agent skill negotiation: should agents advertise "what they can do" dynamically based on workspace content?
- Human-in-the-loop: support for `TASK_STATE_INPUT_REQUIRED` and `TASK_STATE_AUTH_REQUIRED`
