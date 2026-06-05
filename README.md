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
                   │   ↓ SessionPool     │           │   ↓ SessionPool     │
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
| `internal/wrapper/` | A2A server, executor, session pool (long-running `claude` subprocesses with persistence + HA), agent client |
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

## Install as a Claude Code plugin

ahsir ships as a Claude Code plugin so you can install it once and use it from inside any Claude Code session — without remembering `--scheduler` URLs or absolute binary paths.

The plugin bundles:

- The `ahsir` and `ahsir-agent` CLI binaries (pre-built per platform under `plugin/bin/<os>-<arch>/`).
- A small wrapper at `plugin/bin/ahsir` that auto-detects platform.
- A skill at `plugin/skills/ahsir/SKILL.md` that teaches Claude **when** to use ahsir (parallel sub-tasks, specialist agents, multi-turn with a specific agent) and **how** to invoke it (`ahsir list`, `ahsir chat`, etc).

### Install (recommended: via marketplace)

Claude Code's plugin system uses a git-based marketplace model — no central registry, no upload step. The repo's root holds a `.claude-plugin/marketplace.json` catalog, and Claude Code clones the repo on `marketplace add`.

From inside any Claude Code session, run the two slash commands below:

```
/plugin marketplace add wu8685/ahsir
/plugin install ahsir@ahsir
```

That's it — the binaries for your OS/arch are already bundled under `plugin/bin/<os>-<arch>/`, so the install resolves to a working `ahsir` and `ahsir-agent` immediately. The first `ahsir` is the plugin name; `@ahsir` is the marketplace name (both happen to be "ahsir" here because this repo is single-plugin).

Then add the wrappers to your shell PATH so the same `ahsir` binary works from a normal terminal too — not just Claude Code's Bash tool. Claude Code installs marketplaces under `~/.claude/plugins/<marketplace>/`:

```bash
echo 'export PATH="$HOME/.claude/plugins/ahsir/plugin/bin:$PATH"' >> ~/.zshrc
exec zsh
```

Supported platforms: **darwin-arm64**, **darwin-amd64**, **linux-amd64**, **linux-arm64**. If you're on a different OS/arch, fall back to the local-clone option below.

### Install (alternative: local clone, for development)

If you're hacking on ahsir itself, clone the repo and point Claude Code at the working tree directly:

```bash
# 1. Clone the repo (or `git pull` to update an existing clone).
git clone https://github.com/wu8685/ahsir.git
cd ahsir

# 2. Build the bundled binaries for your platform.
make plugin-current     # builds plugin/bin/<os>-<arch>/{ahsir,ahsir-agent}

# 3. Point your Claude Code at the plugin directory.
#    Either start Claude Code with --plugin-dir:
claude --plugin-dir "$(pwd)/plugin"

#    Or install via the /plugin slash command from inside an existing
#    Claude Code session and point it at this repo's plugin/ subdirectory.

# 4. (Optional) Add the wrapper to your shell PATH so `ahsir` works from
#    a normal terminal too — not just from Claude Code's Bash tool.
echo 'export PATH="$HOME/path/to/ahsir/plugin/bin:$PATH"' >> ~/.zshrc
exec zsh   # reload
```

For multi-platform release builds: `make plugin` cross-compiles darwin-arm64, darwin-amd64, linux-amd64, and linux-arm64 into `plugin/bin/<os>-<arch>/`. Run this and commit before tagging a release so marketplace installers get the new binaries.

### What you get inside Claude Code

Once the plugin is loaded, two things happen automatically:

1. **The skill auto-loads** — Claude Code reads `plugin/skills/ahsir/SKILL.md` and consults its `description` whenever you describe a task. When the description matches (you ask about delegation, multi-agent, parallel sub-tasks, specialist agents, or mention "ahsir" explicitly), Claude proposes using it.

2. **The CLI is on Claude's Bash path** (once you set PATH per step 4). Claude can shell out:

   ```bash
   ahsir ping                                # is the scheduler up?
   ahsir list                                # what agents are available?
   ahsir chat teacher "<task>" --context T1  # send a task, get reply
   ```

### Explicit invocation

Talk to Claude naturally — the skill teaches it the patterns. Examples:

> "Use ahsir to have the teacher summarize this article."
>
> "Spin up three reviewers via ahsir, each critiquing the code from a different angle (security, performance, maintainability)."
>
> "Talk to the researcher agent across the next few messages — keep using contextId `design-experiment-1`."

### Automatic invocation

Even without saying "ahsir", Claude will reach for it when the task shape matches. For example, if you ask "I need three independent code reviews from different perspectives" and Claude knows you have an ahsir scheduler running with reviewer agents, the skill will guide it to fan out via `ahsir chat`.

### Running the scheduler

The plugin does NOT auto-start a scheduler — that's left to you (or to Claude, if you ask). Start one in a separate terminal:

```bash
export MODEL_API_KEY=<your-deepseek-or-anthropic-key>
ahsir start path/to/your/ahsir.yaml
```

Claude can detect the scheduler isn't running (via `ahsir ping` returning exit 2) and ask whether to start it.

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

For Codex CLI-backed agents, use `provider: codex` and `command: codex`.
`apiKey` maps to `CODEX_API_KEY`; `model` maps to `codex exec --model`.

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

The agent runs a long-lived `claude` subprocess per A2A `contextId`
(`ClaudeSession` pooled by `SessionPool`). Each session-lifecycle event emits
one line on the agent's stdout, which the scheduler tees into its own
terminal:

```
claude session: started pid=59108 cmd=claude args=[-p --input-format=stream-json --output-format=stream-json --verbose]
```

When the pool resumes an evicted (or unhealthy) session, the same line
carries the `--resume=<id>` flag:

```
claude session: started pid=67914 cmd=claude args=[... --resume=4a038c6b-f0cb-4ea6-ad1c-05eb7741511c]
```

Inter-agent traffic and per-request receive markers:

```
[teacher] receive: contextID=demo msgID=... text="..."
[student → teacher] A2A_CALL: task="..."
[student ← teacher] reply: took=12.3s bytes=... preview="..."
```

Useful greps:

| Grep | What it tells you |
|---|---|
| `pid=` | Every new `claude` subprocess (one per contextId, per agent, until idle eviction) |
| `--resume=` | Pool eviction recovery, cross-restart resume, or self-healing on SIGKILL |
| `[teacher]` / `[student]` | Per-agent request/log filtering |
| `[X → Y] A2A_CALL` | Cross-agent delegations |

If you suspect the time is being spent outside the LLM (in scheduler / MCP
shim / serialization), compare the elapsed sum across all agent log lines
against your end-to-end latency. A large gap means the overhead is in the
chain, not in the model.

## Run the tests

### Default suite (mocks, no API key required)

```bash
go test ./...
```

Includes:

- Unit tests for registry, wrapper, scheduler, MCP server.
- An end-to-end gateway test (`internal/scheduler/gateway_test.go`) that spins
  up a real A2A server with a mock executor and exercises both the direct A2A
  path and the scheduler-gateway path.

No `MODEL_API_KEY` or live `claude` CLI required — the default suite uses mocks.

### End-to-end with a real LLM (opt-in)

The `e2e/` package holds top-to-bottom integration tests that spawn the real
scheduler subprocess against real `claude` CLIs and a real DeepSeek
(or other Anthropic-compat) endpoint. Build-tagged `e2e` so they never run
in the default pipeline.

```bash
# Build binaries first
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent

# Run the e2e suite
AHSIR_E2E_CLAUDE=1 MODEL_API_KEY=<your-deepseek-key> \
  go test -tags=e2e -timeout=5m -v ./e2e/
```

Tests skip gracefully if `bin/ahsir(-agent)` isn't built, `claude` isn't on
PATH, or `AHSIR_E2E_CLAUDE` / `MODEL_API_KEY` aren't set — so the same
command can be wired into CI conditionally without manual gating.

There's also a lower-level e2e at `internal/wrapper/session_claude_e2e_test.go`
that exercises `ClaudeSession` directly against real claude (no scheduler /
A2A layer). Same env-var + build-tag gates.
