# Example: Student-Teacher Multi-Agent Setup

The full multi-agent walkthrough: two agents — **Student** and **Teacher** — collaborating via the [A2A protocol](https://google.github.io/A2A/), plus filesystem access, scheduler gateway endpoints, and Claude Code plugin/CLI integration.

- **Teacher**: Answers questions, summarizes articles, explains concepts. Has filesystem read access to a configurable allow-list (defaults include `/Users/wuke/workspace/brain-spark` — adjust to your own path).
- **Student**: Receives user requests. When it needs help, delegates to the Teacher via `---A2A_CALL---`.

If you only want to see the basics, start with [`../simple/`](../simple/) (single agent, single curl). For the session-reuse mechanics in isolation, see [`../session-reuse/`](../session-reuse/).

## Layout

```
multi-agent/
├── ahsir.yaml                                   # scheduler + registry + two agents
├── hello_test.go                                # integration tests
├── workspaces/
│   ├── teacher/.a2a/agent-card.yaml             # teacher card (has fs access)
│   └── student/.a2a/agent-card.yaml             # student card (delegates)
└── README.md
```

## Prerequisites

- **Go** >= 1.23 (1.23.1 is the minimum verified)
- **Claude Code CLI** (`claude`) on PATH — backs each agent at runtime
- **`MODEL_API_KEY`** env var pointing to a DeepSeek API key:

  ```bash
  export MODEL_API_KEY=<your-deepseek-api-key>
  ```

  The agents are wired to DeepSeek's Anthropic-compatible endpoint (`https://api.deepseek.com/anthropic`, model `deepseek-v4-pro`). Get a key from <https://platform.deepseek.com/>.

  To use a different provider, edit `workspaces/{teacher,student}/.a2a/agent-card.yaml` — the `runtime` block accepts `provider: anthropic|zhipu|deepseek|codex` plus `baseURL` / `apiKey` / `model` where supported. Startup fails fast if a referenced env var is unset.

## Quick Start

### 1. Build the binaries (from repo root)

```bash
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent
```

### 2. Start the scheduler

The scheduler reads `example/multi-agent/ahsir.yaml`, starts both agents, and runs the registry on port 9800. Run from the **repo root** (workspace paths in `ahsir.yaml` are relative to the scheduler's cwd):

```bash
./bin/ahsir start example/multi-agent/ahsir.yaml
```

**Internal port allocation:** local agent ports are assigned from
`port_range.start` (default 9801) in declaration order:

| Agent | Port |
|-------|------|
| teacher | 9801 |
| student | 9802 |

The scheduler prints the assignment to stdout: `Agent <name> listening on port
<port>`. These ports are internal execution targets; public A2A requests should
use the scheduler endpoint `/a2a/{agent}`. Scheduler-started local agents reject
direct A2A JSON-RPC on the internal port unless the scheduler-issued internal
token is present.

### 3. Ask student to delegate to teacher

Be **explicit** about delegation. Loose prompts like "summarize X; ask the teacher if you need help" often let the model answer directly without emitting an `---A2A_CALL---` marker — phrase it as a direct instruction to delegate.

```bash
curl -s -X POST http://127.0.0.1:9800/a2a/student \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-delegate",
        "contextId": "demo-multi",
        "role": "user",
        "parts": [{"kind": "text", "text": "Please delegate this to the teacher agent: explain what a goroutine is in one paragraph. Then relay the teacher'\''s answer back to me."}]
      }
    },
    "id": 1
  }'
```

Expected scheduler log sequence:

```
[student] receive: contextID=demo-multi msgID=msg-delegate text="Please delegate this to the teacher agent..."
claude session: started pid=... cmd=claude args=[...]                      # student's claude
[student → teacher] A2A_CALL: task="explain what a goroutine is in one paragraph"
[teacher] receive: contextID=demo-multi msgID=... text="explain what a goroutine is..."   ← contextId propagated
claude session: started pid=... cmd=claude args=[...]                      # teacher's claude
[student ← teacher] reply: took=12.3s bytes=... preview="A goroutine is a lightweight thread..."
```

Two `claude session: started` lines (one per agent's first turn) confirm both agents really fired. The `[X → Y]` / `[X ← Y]` lines are the inter-agent edges; if you only see `[student] receive` and no `→ teacher`, the student model answered directly without delegating — sharpen the prompt.

**Reuse:** A follow-up curl with `contextId: "demo-multi"` reuses BOTH the student's AND teacher's claude processes (`contextId` propagates over the A2A_CALL boundary). No further `claude session: started` lines for either agent.

### 4. Filesystem access

The teacher has filesystem read access via an allow-list in its `agent-card.yaml`:

```yaml
filesystem:
  enabled: true
  allowed_paths:
    - "."
    - "/tmp"
    - "/Users/wuke/workspace/brain-spark"   # <-- change this to your own path
```

At startup the wrapper translates each entry into a `--add-dir=<abs-path>` argument for `claude -p`, plus `--allowedTools=Read,LS,Glob,Grep` so the model can use the built-in filesystem tools but cannot write or shell out. No extra tool server is involved.

> The `--flag=value` form is deliberate: `--add-dir` and `--allowedTools` are variadic in the Claude Code CLI, and the space-separated form would greedily consume neighbouring tokens. The prompt is fed via stdin, so it is not at risk, but other flag values still are. Stick to `=` form when adding new flags in `cmd/ahsir-agent/main.go`.

Verify end-to-end:

```bash
# Adjust the path to a directory under one of teacher's allowed_paths.
curl -s -X POST http://127.0.0.1:9800/a2a/student \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-summary",
        "contextId": "demo-fs",
        "role": "user",
        "parts": [{"kind": "text", "text": "Please delegate this to the teacher agent: summarize the contents under /Users/wuke/workspace/brain-spark/<some-subdir> (only the teacher has access to that path). Relay the teacher'\''s reply back to me."}]
      }
    },
    "id": 1
  }'
```

You should see the student emit `---A2A_CALL---` to delegate, and the teacher's reply containing concrete details from the actual files (proof it really read them, didn't make them up).

To grant write or bash access, edit `cmd/ahsir-agent/main.go` and swap `--allowedTools=Read,LS,Glob,Grep` for `--permission-mode=bypassPermissions` (or expand the allow-list). Be aware this also unlocks `Edit`, `Write`, `Bash` for the model.

### 5. Scheduler HTTP API (registry + gateway)

The scheduler exposes three groups of endpoints on port 9800. Registry
endpoints serve public Agent Cards whose URLs point back to scheduler
`/a2a/{agent}` routes; the A2A proxy forwards native JSON-RPC into the internal
agent ports; chat / task-status gateway endpoints provide CLI-friendly wrappers.
The `ahsir` CLI uses these same gateway paths.

```bash
# Registry: list / get
curl -s http://127.0.0.1:9800/agents
curl -s http://127.0.0.1:9800/agents/student

# Native A2A through scheduler
curl -s -X POST http://127.0.0.1:9800/a2a/teacher \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-a2a","role":"user","parts":[{"kind":"text","text":"What is a goroutine?"}]}},"id":1}'

# Gateway: chat (forwards to the agent over A2A)
curl -s -X POST http://127.0.0.1:9800/agents/teacher/chat \
  -H 'Content-Type: application/json' \
  -d '{"message":"What is a goroutine?"}'

# Gateway: task status (taskID comes from a previous chat / message-send response)
curl -s http://127.0.0.1:9800/agents/teacher/tasks/<task-id>
```

This is also the quickest way to sanity-check the gateway after restarting the scheduler; `ahsir chat` is a small wrapper over this path.

Each local agent also exposes internal operational endpoints. These remain
token-free so the scheduler and operators can check process health:

```bash
curl -s http://127.0.0.1:9801/healthz
curl -s http://127.0.0.1:9801/readyz
curl -s http://127.0.0.1:9801/.well-known/agent-card.json
```

If an agent process exits unexpectedly, or if `/healthz` fails repeatedly, the
scheduler restarts it on the same port with exponential backoff. `ahsir agent
delete <name>` and scheduler shutdown are treated as intentional stops and are
not restarted.

### 6. Tune timeouts

There are three deadlines in the chain; the invariant is **outer ≥ inner**:

| Where | Default | Configured in |
|---|---|---|
| CLI `http.Client.Timeout` | `chat + 1m`, or no timeout when `chat: 0s` | fetched from scheduler at startup |
| Gateway forwarding (`POST /agents/{n}/chat`) | 10m, or no deadline when `chat: 0s` | `timeouts.chat` in `ahsir.yaml` |
| Per-agent provider deadline | 300s, or no provider deadline with `runtime.timeout: 0s` | `runtime.timeout` in each `agent-card.yaml` |

Bump `timeouts.chat` if any agent's `runtime.timeout` is increased — the CLI picks up the new value when it talks to the scheduler. For intentionally long-running work, set both `timeouts.chat: 0s` and the relevant agent's `runtime.timeout: 0s`.

### 7. Reading the logs

The production path is a **long-running `claude` subprocess per A2A `contextId`** (`ClaudeSession`) pooled by `SessionPool`. Each session lifecycle emits a process-start line:

```
claude session: started pid=59108 cmd=claude args=[-p --input-format=stream-json --output-format=stream-json --verbose]
```

When the pool resumes an evicted (or unhealthy) session against a remembered `sessionId`, the same line carries the `--resume=<id>` flag:

```
claude session: started pid=67914 cmd=claude args=[... --resume=4a038c6b-f0cb-4ea6-ad1c-05eb7741511c]
```

If `claude` writes to stderr, the wrapper drains it into the log (auth failures, hook crashes, deprecation notes typically land here):

```
claude session [teacher] stderr: <line>
```

Inter-agent traffic shows up as:

```
[teacher] receive: contextID=... msgID=... mode=send text="..."
[student → teacher] A2A_CALL: contextID=... depth=0 source=legacy_text task="..."
[student ← teacher] reply: contextID=... depth=0 took=12.3s bytes=... preview="..."
```

Common patterns:

- **Per-conversation tracing**: one `pid=` per `contextId` per agent. Reused requests don't emit new start lines.
- **Resume detection**: grep for `--resume=` to spot pool eviction recovery or self-healing on SIGKILL.
- **Per-agent filtering**: agent name appears in `[teacher]` / `[student]` brackets.
- **Performance tracing**: grep `contextID=<id>` for the full waterfall, then compare `send done`, `session pool: lookup`, `executor turn done`, and `[X ← Y] reply` timings.
- **Failures**: missing process-start log means `cmd.Start()` failed (check `MODEL_API_KEY`, claude binary path, or the resolved `args` block from `agent-card.yaml`).

### 8. Shut down

`Ctrl+C` in the scheduler terminal. The scheduler stops the registry and kills all agent subprocesses.

## Use ahsir from your local Claude Code

Install the Claude Code plugin described in the root README, put
`plugin/bin` on PATH, and keep the scheduler running from step 2. The skill
will guide Claude to use CLI commands such as:

```bash
ahsir ping
ahsir list
ahsir chat student "Please delegate this to the teacher agent: explain what a goroutine is in one paragraph." --context demo-multi
```

The flow is **Claude Code skill → Bash tool → `ahsir` CLI → scheduler gateway
→ A2A → agent**. The CLI spawns no agents and loads no `ahsir.yaml`; it talks to
the already-running scheduler.

## Run the Tests

```bash
go test ./example/multi-agent/ -v
```

Three tests:

| Test | What it verifies |
|------|-----------------|
| `TestLoadAgentCards` | Both `teacher` and `student` agent-card YAMLs are valid |
| `TestLoadSchedulerConfig` | `ahsir.yaml` is valid and contains both agents |
| `TestStudentDelegatesToTeacher` | Student receives a message, delegates to Teacher via A2A_CALL, returns the answer |

The integration test uses a mock session — it does **not** require `MODEL_API_KEY` or a real `claude` CLI. To exercise the full path (real LLM, real filesystem reads), use the manual `curl` flow above.
