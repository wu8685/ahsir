# Example: Student-Teacher Multi-Agent Setup

A minimal multi-agent example using AHSIR. Two agents — **Student** and **Teacher** — collaborate via the [A2A protocol](https://google.github.io/A2A/):

- **Teacher**: Answers questions, summarizes articles, explains concepts. Has filesystem read access to `/Users/wuke/workspace/brain-spark` (adjust to your own path).
- **Student**: Receives user requests. When it needs help, delegates to the Teacher via `---A2A_CALL---`.

## Prerequisites

- **Go** >= 1.23 (any toolchain that can build the project; 1.23.1 is the minimum verified)
- **Claude Code CLI** (`claude`) — backs each agent at runtime; both agents are configured to drive `claude -p`.
- Both `claude` and `go` must be in your `PATH`.
- **`MODEL_API_KEY`** environment variable. The agents are wired to Zhipu/智谱's Anthropic-compatible endpoint (`https://open.bigmodel.cn/api/anthropic`, model `glm-5.1`). Get a key from <https://open.bigmodel.cn/> and export it before starting:

  ```bash
  export MODEL_API_KEY=<your-zhipu-api-key>
  ```

  If you'd rather use a different provider, edit `example/workspaces/{teacher,student}/.a2a/agent-card.yaml` — the `runtime` block accepts `provider: anthropic` plus `baseURL` / `apiKey` / `model`. Startup will fail fast if a referenced env var is unset.

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

The scheduler launches each agent as a subprocess, passing `--registry` so the agent can list and call other agents.

**Port allocation:** ports are assigned from `port_range.start` (default 9801) in declaration order from `ahsir.yaml`. With the default config:

| Agent | Port |
|-------|------|
| teacher | 9801 |
| student | 9802 |

(The scheduler also prints the assignment to stdout: `Agent <name> listening on port <port>`.)

### 3. Interact with agents

#### Option A: Direct HTTP (A2A JSON-RPC)

Each agent exposes an A2A JSON-RPC endpoint.

```bash
# Ask Teacher directly
curl -s -X POST http://127.0.0.1:9801/ \
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

# Ask Student → it delegates to Teacher
curl -s -X POST http://127.0.0.1:9802/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-2",
        "role": "user",
        "parts": [{"kind": "text", "text": "Please summarize <some directory>; ask the teacher if you need help."}]
      }
    },
    "id": 2
  }'
```

The response is an A2A `Task` — `result.history` contains the user message, the student's `---A2A_CALL---` block, and the teacher's reply.

#### Option B: Scheduler HTTP API (registry + gateway)

The scheduler exposes two groups of endpoints on port 9800. Registry endpoints serve agent CRUD; gateway endpoints forward chat / task-status into the running agents over A2A. Both share the same listener — the same paths the MCP shim hits.

```bash
# Registry: list / get
curl -s http://127.0.0.1:9800/agents
curl -s http://127.0.0.1:9800/agents/student

# Gateway: chat (forwards to the agent over A2A)
curl -s -X POST http://127.0.0.1:9800/agents/teacher/chat \
  -H 'Content-Type: application/json' \
  -d '{"message":"What is a goroutine?"}'

# Gateway: task status (taskID comes from a previous chat / message-send response)
curl -s http://127.0.0.1:9800/agents/teacher/tasks/<task-id>
```

This is also the quickest way to sanity-check the gateway after restarting the scheduler — if `POST /agents/<name>/chat` works here, the MCP shim path will work too.

### 4. Filesystem access

The teacher needs to read real files to summarize them, so its `agent-card.yaml` declares an allow-list:

```yaml
filesystem:
  enabled: true
  allowed_paths:
    - "."
    - "/tmp"
    - "/Users/wuke/workspace/brain-spark"   # <-- change this
```

At startup the wrapper translates each entry into a `--add-dir=<abs-path>` argument for `claude -p`, plus `--allowedTools=Read,LS,Glob,Grep` so the model can use the built-in filesystem tools but cannot write or shell out. No custom MCP server is involved.

> The `--flag=value` form is deliberate: `--add-dir` and `--allowedTools` are variadic in the Claude Code CLI, and the space-separated form would greedily consume neighbouring tokens. The prompt is fed via stdin (`SessionManager.Send`) so it is not at risk, but other flag values still are. Stick to `=` form when adding new flags in `cmd/ahsir-agent/main.go`.

To verify end-to-end:

```bash
# Adjust the path to a directory under one of teacher's allowed_paths
curl -s -X POST http://127.0.0.1:9802/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-summary",
        "role": "user",
        "parts": [{"kind": "text", "text": "请总结 /Users/wuke/workspace/brain-spark/<some-subdir> 下的内容；如果不擅长可以委托 teacher。"}]
      }
    },
    "id": 1
  }'
```

You should see the student emit `---A2A_CALL---` to delegate, and the teacher's reply containing concrete details from the actual files (proof it really read them, didn't make them up).

To grant write or bash access, edit `cmd/ahsir-agent/main.go` and swap `--allowedTools=Read,LS,Glob,Grep` for `--permission-mode=bypassPermissions` (or expand the allow-list). Be aware this also unlocks `Edit`, `Write`, `Bash` for the model.

### 5. Tune timeouts

There are three deadlines in the chain; the invariant is **outer ≥ inner**:

| Where | Default | Configured in |
|---|---|---|
| MCP shim `http.Client.Timeout` | `chat + 1m` | fetched from scheduler at startup |
| Gateway forwarding (`POST /agents/{n}/chat`) | 10m | `timeouts.chat` in `example/ahsir.yaml` |
| Per-agent LLM subprocess deadline | 300s | `runtime.timeout` in each `agent-card.yaml` |

Bump `timeouts.chat` if any agent's `runtime.timeout` is increased — the MCP shim will pick up the new value the next time it starts. The agent's own `runtime.timeout` is the authoritative deadline for a single LLM round-trip; the outer two are just upper bounds to prevent indefinite hangs.

### 6. Reading the logs

Each LLM call (`session.Send`) emits a correlated start/end pair to the agent process stdout — which the scheduler tees into its own terminal:

```
session.Send: claude starting (id=a3f9c1, agent=teacher, prompt=1366B, timeout=5m0s)
session.Send: claude ok in 2m17.8s (id=a3f9c1, agent=teacher, prompt=1366B, stdout=7979B, stderr=0B)
```

Common patterns:

- **Latency breakdown across an agent chain**: a single user request typically produces 2–3 lines (e.g. student dispatch → teacher work → student rephrase). Sum the elapsed times to see where the time goes.
- **Concurrent calls**: grep `id=<6-hex>` to pair start/end across interleaved log output.
- **Per-agent filtering**: grep `agent=teacher` to isolate one agent.
- **Hook / warning noise**: grep `stderr-on-success` to catch calls that succeeded but had stderr output (e.g. SessionEnd hook errors). The next log line shows a 200-byte preview.
- **Failures**: grep `FAILED` — the line carries `signal: killed` (cmdCtx hit `runtime.timeout`) or `exit status N` (claude itself errored).

### 7. Shut down

Press `Ctrl+C` in the scheduler terminal. The scheduler stops the registry and kills all agent subprocesses.

## Use ahsir from your local Claude Code (via MCP)

Instead of curling the JSON-RPC endpoint, you can drive the running scheduler from your own Claude Code session. The repo ships a `.mcp.json` that registers `ahsir` as an MCP server backed by a thin stdio shim:

```json
{
  "mcpServers": {
    "ahsir": {
      "command": "/Users/wuke/workspace/go/src/github.com/wu8685/ahsir/bin/ahsir",
      "args": ["mcp", "--scheduler", "http://127.0.0.1:9800"]
    }
  }
}
```

The shim is just a protocol adapter — it spawns no agents, loads no `ahsir.yaml`, and holds no state. Every tool call becomes an HTTP request to the running scheduler:

- `agent_list` → `GET /agents`
- `agent_chat` → `POST /agents/{name}/chat`
- `agent_task_status` → `GET /agents/{name}/tasks/{taskID}`

So the flow is **Claude Code → `ahsir mcp` shim → scheduler gateway → A2A → agent**. Nothing works unless the scheduler is already running (`./bin/ahsir start example/ahsir.yaml` from step 2).

To use it:

1. Make sure `bin/ahsir` is built (step 1) and the scheduler is running (step 2).
2. Open Claude Code in this repo (it auto-discovers `.mcp.json`); approve the `ahsir` MCP server.
3. Ask Claude Code things like *"list agents via ahsir"* or *"have the student summarize \<some path under teacher's allowed roots\> using ahsir"*. It will use `agent_list` / `agent_chat` to talk to the scheduler.

Notes:

- `command` is an **absolute path** to the `ahsir` binary built in step 1. If you cloned this repo somewhere else (or someone else is using the example), update `command` to match — Claude Code will not search `PATH` here.
- If your scheduler runs on a different host or port, update the `--scheduler` arg accordingly.
- The shim auto-aligns its own `http.Client.Timeout` with the scheduler's `timeouts.chat` (plus a 1-minute buffer) on startup; you should see a line like `ahsir mcp shim: client timeout aligned to 11m0s` on stderr. No need to set timeouts in `.mcp.json`.

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

The integration test uses a mock session — it does **not** require `MODEL_API_KEY` or a real `claude` CLI. To exercise the full path (real LLM, real filesystem reads), use the manual `curl` flow in step 3-4.

Generated with [Claude Code](https://claude.com/claude-code)
