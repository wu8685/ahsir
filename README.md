# ahsir — A Multi-Agent Scheduler over A2A

`ahsir` is a small Go scheduler that runs multiple LLM-backed agents as local
subprocesses, lets them talk to each other over the
[A2A protocol](https://google.github.io/A2A/), and exposes the whole fleet to
external tools (e.g. Claude Code) via MCP.

Each agent is a `claude -p` subprocess wrapped in a JSON-RPC HTTP server. The
scheduler owns the agent registry, a gateway that forwards chat / task-status
requests, and an MCP stdio shim that lets your local Claude Code drive the
fleet without speaking A2A directly.

## Architecture

```
                   ┌─────────────────────────────────────────────┐
   Claude Code ─►  │  ahsir mcp (stdio shim)                     │
   (.mcp.json)     │  - tool calls → HTTP                        │
                   └─────────────────┬───────────────────────────┘
                                     │ HTTP
                                     ▼
                   ┌─────────────────────────────────────────────┐
   curl / tests ─► │  ahsir start  (scheduler, port 9800)        │
                   │  ┌──────────────┬───────────────────────┐   │
                   │  │ registry     │ gateway               │   │
                   │  │ /agents      │ /agents/{n}/chat      │   │
                   │  │ /agents/{n}  │ /agents/{n}/tasks/{t} │   │
                   │  │              │ /config/timeouts      │   │
                   │  └──────────────┴───────────┬───────────┘   │
                   └─────────────────────────────┼───────────────┘
                                                 │ A2A JSON-RPC
                              ┌──────────────────┼─────────────────┐
                              ▼                                    ▼
                   ┌─────────────────────┐           ┌─────────────────────┐
                   │ ahsir-agent (9801)  │           │ ahsir-agent (9802)  │
                   │ A2A server  ◄────►  │  A2A      │ A2A server          │
                   │ wrapper / executor  │           │ wrapper / executor  │
                   │   ↓ session.Send    │           │   ↓ session.Send    │
                   │   claude -p (LLM)   │           │   claude -p (LLM)   │
                   └─────────────────────┘           └─────────────────────┘
```

## Repo layout

| Path | Purpose |
|---|---|
| `cmd/ahsir/` | Scheduler + MCP shim CLI (`ahsir start`, `ahsir mcp`) |
| `cmd/ahsir-agent/` | Per-agent process; loads agent-card, hosts A2A endpoint, drives the LLM CLI |
| `internal/scheduler/` | Config, agent lifecycle, registry, HTTP gateway |
| `internal/registry/` | Agent registration / heartbeat / lookup |
| `internal/wrapper/` | A2A server, executor, session manager (claude subprocess), agent client |
| `internal/mcp/` | MCP stdio server + scheduler HTTP client |
| `example/` | Working two-agent setup (student delegates to teacher) |
| `docs/superpowers/` | Specs, plans, and design notes |

## Quick start

```bash
# Build both binaries
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent

# Provide an LLM endpoint (DeepSeek used in the bundled examples)
export MODEL_API_KEY=<your-deepseek-key>

# Start the scheduler with the multi-agent example config
./bin/ahsir start example/multi-agent/ahsir.yaml
```

Then either curl the agents directly, hit the scheduler gateway, or drive the
fleet from your local Claude Code via the bundled `.mcp.json`. Full hands-on
instructions live in [`example/README.md`](example/README.md).

## Configuration

Two YAML files drive everything:

### `ahsir.yaml` — scheduler config

```yaml
agents:
  - name: teacher
    workspace: example/multi-agent/workspaces/teacher
    port: 0          # 0 = auto-allocate from port_range
  - name: student
    workspace: example/multi-agent/workspaces/student
    port: 0

registry:
  host: "127.0.0.1"
  port: 9800
  heartbeat_interval: 10s
  heartbeat_timeout: 30s

# Outer-envelope timeouts. Optional — defaults shown.
# `chat` MUST be >= the largest agent's runtime.timeout (in agent-card.yaml).
# The MCP shim fetches `chat` from the scheduler at startup and uses chat+1m
# as its own http.Client.Timeout, so this is the single knob you tune.
timeouts:
  chat: 10m
  task_status: 30s

port_range:
  start: 9801
  end: 9900
```

### `<workspace>/.a2a/agent-card.yaml` — per-agent config

System prompt, runtime backend (provider / baseURL / apiKey / model),
filesystem allow-list, and the per-agent LLM subprocess timeout.

```yaml
name: teacher
runtime:
  command: claude
  args: ["-p", "--output-format", "text"]
  timeout: 300s          # claude subprocess deadline
  provider: zhipu
  apiKey: "${MODEL_API_KEY}"
  model: glm-5.1
filesystem:
  enabled: true
  allowed_paths:
    - "."
    - "/tmp"
```

## Timeout topology

There are three layers of deadlines; the invariant is **outer ≥ inner**.

```
MCP shim http.Client.Timeout  =  chat + 1m   ← fetched from /config/timeouts
gateway ctx                    =  chat        ← timeouts.chat in ahsir.yaml
agent runtime.timeout          =  300s        ← per agent-card.yaml
```

Tune the outer two via `timeouts:` in `ahsir.yaml`. The per-agent subprocess
deadline stays per-agent because it is intrinsic to that agent's expected
response latency (a fast classifier vs. a deep researcher legitimately differ).

## Diagnostics: reading the logs

Every LLM round-trip emits a correlated start/end pair on the agent process
stdout (which the scheduler tees into its own terminal):

```
session.Send: claude starting (id=a3f9c1, agent=teacher, prompt=1366B, timeout=5m0s)
session.Send: claude ok in 2m17.8s (id=a3f9c1, agent=teacher, prompt=1366B, stdout=7979B, stderr=0B)
```

Useful greps:

| Grep | What it tells you |
|---|---|
| `agent=teacher` | All LLM calls made by a specific agent |
| `id=a3f9c1` | Match a start ↔ end pair under concurrency |
| `stderr-on-success` | Calls that "succeeded" but the CLI wrote to stderr (hook noise, deprecation warnings) |
| `FAILED` | Failed calls; the same line carries `signal: killed` / `exit status N` |

If you suspect the time is being spent outside the LLM (in scheduler / MCP
shim / serialization), compare the elapsed sum across all agent log lines
against your end-to-end latency. A large gap means the overhead is in the
chain, not in the model.

## Run the tests

```bash
go test ./...
```

The suite includes:

- Unit tests for registry, wrapper, scheduler, MCP server.
- An end-to-end gateway test (`internal/scheduler/gateway_test.go`) that spins
  up a real A2A server with a mock executor and exercises both the direct A2A
  path (Option A) and the scheduler-gateway path (Option B).

No `MODEL_API_KEY` or live `claude` CLI is required — the suite uses mocks.
The hands-on example flow in `example/README.md` is what exercises the real
LLM path.
