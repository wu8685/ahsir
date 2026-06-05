# AHSIR: Multi-Agent Scheduler based on A2A Protocol

**Status:** Superseded by the current plugin skill + `ahsir` CLI entrypoint. This
document preserves the original MCP-based design for historical context only.
See `README.md` for the current architecture.
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
+----------------------------------+-------------------------+
                                   | spawns + JSON-RPC over stdio
                                   v
+------------------------------------------------------------+
|              ahsir mcp  (thin stdio MCP shim)               |
|              tools: agent_list / agent_chat /               |
|                     agent_task_status                       |
+----------------------------------+-------------------------+
                                   | HTTP (gateway endpoints)
                                   v
+------------------------------------------------------------+
|              Scheduler (ahsir start)                        |
|  +--------------+  +----------------+  +-----------------+  |
|  |  Lifecycle   |  |   Registry     |  |    Gateway      |  |
|  |  Manager     |  |  (AgentCards)  |  |  (chat / tasks) |  |
|  +------+-------+  +-------+--------+  +--------+--------+  |
+---------+------------------+--------------------+----------+
          | start/stop       | register + lookup  | A2A forward
          v                  v                    v
+------------------+  +------------------+  +------------------+
|  Agent Wrapper   |  |  Agent Wrapper   |  |  Agent Wrapper   |
|  (backend/)      |<>|  (frontend/)     |  |  (data/)         |
|                  |A2A                 |A2A                |
|  Claude Code     |  |  Claude Code     |  |  Claude Code     |
|  (persistent)    |  |  (persistent)    |  |  (persistent)    |
+------------------+  +------------------+  +------------------+
```

The user-facing path is **Claude Code → MCP shim → scheduler gateway → agent**. The MCP shim is a tiny per-invocation process that owns no state; the scheduler is the single chokepoint that holds the registry and forwards every chat/task call into A2A. Agent-to-agent calls are still peer-to-peer over A2A (they discover endpoints from the registry but bypass the gateway).

### Components

| Component | Binary | Responsibility |
|-----------|--------|----------------|
| Scheduler | `ahsir start` | Lifecycle management, Registry, Gateway HTTP API |
| MCP shim | `ahsir mcp` | stdio JSON-RPC adapter; forwards each tool call to the scheduler over HTTP |
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

### 4.3 Registry + Gateway HTTP API

The scheduler's HTTP server exposes both registry routes and gateway routes on the same listener (default `127.0.0.1:9800`). Go 1.22+ `http.ServeMux` pattern routing prefers the more specific gateway patterns, so registry handlers continue to own the bare `/agents` and `/agents/{name}` paths:

```
POST   /agents                          Register agent (AgentCard in body)
GET    /agents                          List all registered agents
GET    /agents/{name}                   Get specific agent's full AgentCard
DELETE /agents/{name}                   Unregister agent

POST   /agents/{name}/chat              Gateway: forward a message; body {"message":"..."} -> {"response":"..."}
GET    /agents/{name}/tasks/{taskID}    Gateway: forward task-status query; returns A2A Task JSON
```

The gateway routes are the public seam used by the MCP shim (and any future CLI / web UI). They never reach the registry handler.

### 4.4 MCP shim (`ahsir mcp`)

The MCP shim is a separate, short-lived process spawned by the user's local Claude Code per `.mcp.json` configuration. It does **not** spawn agents, does not load `ahsir.yaml`, does not embed a scheduler — it is purely a protocol adapter:

- reads JSON-RPC messages line-by-line from stdin,
- forwards each `tools/call` to the scheduler over HTTP (registry list + gateway chat/tasks),
- writes responses to stdout; logs go to stderr to keep the MCP wire clean.

It accepts a single flag, `--scheduler <url>` (default `http://127.0.0.1:9800`), so the same shim works against any reachable scheduler.

**Tools:**

| Tool | Parameters | Routes through scheduler |
|------|-----------|-------------------------|
| `agent_list` | — | `GET /agents` |
| `agent_chat` | `agent_name`, `message` | `POST /agents/{name}/chat` |
| `agent_task_status` | `agent_name`, `task_id` | `GET /agents/{name}/tasks/{taskID}` |

### 4.5 Agent Discovery Flow (Agent A -> Agent B)

1. Agent A queries Registry: `GET /agents`
2. Agent A gets Agent B's AgentCard with endpoint URL
3. Agent A sends A2A request directly to Agent B's endpoint
4. No scheduler involvement after discovery

## 5. User Interaction Flow

1. User starts scheduler: `ahsir start`
2. User opens Claude Code in any project; Claude Code spawns `ahsir mcp --scheduler http://127.0.0.1:9800` per `.mcp.json`
3. User says: "Ask the backend agent to design a user API"
4. Claude Code (via MCP) calls `agent_chat("backend", "design a user API")`
5. The MCP shim forwards the call as `POST /agents/backend/chat` to the scheduler
6. Scheduler gateway looks up the agent in its registry and routes to the Backend Agent Wrapper via A2A `message/send`
7. Backend Agent's Claude Code processes the request
8. Response flows back: A2A → scheduler → MCP shim → user's Claude Code session

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
| MCP shim | Scheduler not running | HTTP call fails; shim returns the connection error to Claude Code |

## 8. Security

- **V1 (localhost)**: All HTTP listeners bind to `127.0.0.1`. No authentication required. No data leaves the machine.
- **V2 (multi-machine)**: Authentication via A2A `securitySchemes` in AgentCard (TLS + API key / OAuth). TBD when needed.

## 9. Project Structure

```
ahsir/
├── cmd/
│   ├── ahsir/                # Scheduler + MCP shim binary (subcommands: start, mcp, stop)
│   │   └── main.go
│   └── ahsir-agent/          # Agent Wrapper binary
│       └── main.go
├── internal/
│   ├── scheduler/            # Scheduler logic
│   │   ├── config.go
│   │   ├── scheduler.go      #   Lifecycle + HTTP server (registry + gateway routes)
│   │   └── gateway.go        #   POST /agents/{name}/chat, GET /agents/{name}/tasks/{taskID}
│   ├── registry/             # Agent registry (HTTP CRUD on AgentCards)
│   │   └── registry.go
│   ├── mcp/                  # MCP server (stdio JSON-RPC) + scheduler HTTP client
│   │   ├── server.go
│   │   └── scheduler_client.go  # SchedulerHTTPClient implementing AgentRouter
│   ├── wrapper/              # Agent Wrapper
│   │   ├── server.go         #   A2A JSON-RPC Server
│   │   ├── client.go         #   A2A Client (call other agents)
│   │   ├── runtime.go        #   Provider env resolution + session config
│   │   ├── session.go        #   Claude Code subprocess management
│   │   ├── prompt.go         #   Prompt construction & --A2A_CALL-- parsing
│   │   └── card.go           #   AgentCard reading & building
│   └── ...
├── go.mod
├── go.sum
├── .mcp.json                 # Local Claude Code → spawns `ahsir mcp --scheduler http://127.0.0.1:9800`
└── ahsir.yaml                # Default scheduler config
```

## 10. Open Questions / Future Work

- Claude Code stdout parsing reliability: how to reliably detect the end of a response vs waiting for more tokens
- Session persisting across Agent Wrapper restarts: should conversation history survive a crash?
- Agent skill negotiation: should agents advertise "what they can do" dynamically based on workspace content?
- Human-in-the-loop: support for `TASK_STATE_INPUT_REQUIRED` and `TASK_STATE_AUTH_REQUIRED`
