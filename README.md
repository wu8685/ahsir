# ahsir — A Multi-Agent Scheduler over A2A

`ahsir` is a small Go scheduler that runs multiple LLM-backed agents as local
processes, lets them talk to each other over the
[A2A protocol](https://google.github.io/A2A/), and lets Claude Code drive the
fleet through the bundled plugin skill plus the `ahsir` CLI.

Each agent is an `ahsir-agent` process with an A2A JSON-RPC HTTP endpoint and
a provider-backed `Session` implementation. Today the production session
backends are:

- `ClaudeSession`: one long-running `claude -p --input-format=stream-json`
  subprocess per A2A `contextId`, with `--resume` recovery.
- `CodexSession`: one `codex exec --json` subprocess per turn, resuming by
  Codex `thread_id` for the same A2A `contextId`.

The scheduler owns the agent registry and a gateway that forwards chat /
task-status requests. The Claude Code plugin teaches Claude when to use ahsir
and how to call `ahsir list`, `ahsir chat`, and related CLI commands.

## Architecture

```
                   ┌─────────────────────────────────────────────┐
  Claude Code ───► │ ahsir plugin skill                         │
  + Bash tool      │ chooses ahsir CLI commands                  │
                   └─────────────────┬───────────────────────────┘
                                     │ ahsir list/chat/status
  curl / tests ──────────────────────┘
                                     ▼
                    ┌─────────────────────────────────────────────┐
                    │ ahsir start  (scheduler)                    │
                    │                                             │
                    │  registry          gateway                  │
                    │  /agents           /agents/{name}/chat      │
                    │  heartbeats        /agents/{name}/tasks/{id}│
                    │  endpoint lookup   /config/timeouts         │
                    └─────────────────┬───────────────────────────┘
                                      │ A2A JSON-RPC message/send
                                      │ or message/stream
            ┌─────────────────────────┴───────────────────────────┐
            ▼                                                     ▼
 ┌───────────────────────────────┐                    ┌───────────────────────────────┐
 │ ahsir-agent: student          │                    │ ahsir-agent: teacher          │
 │ - A2A server + task store     │                    │ - A2A server + task store     │
 │ - executor handles agent calls│── peer A2A call ──►│ - executor handles request    │
 │ - SessionPool by contextId    │                    │ - SessionPool by contextId    │
 │ - provider session backend:   │                    │ - provider session backend:   │
 │   ClaudeSession / CodexSession│                    │   ClaudeSession / CodexSession│
 └───────────────┬───────────────┘                    └───────────────┬───────────────┘
                 │                                                    │
       ┌─────────┴─────────┐                                ┌─────────┴─────────┐
       │ claude stream-json│  or  codex exec --json         │ claude stream-json│
       │ / --resume        │      / exec resume <thread>    │ / codex exec      │
       └───────────────────┘                                └───────────────────┘
```

## Repo layout

| Path | Purpose |
|---|---|
| `cmd/ahsir/` | Scheduler + user CLI (`ahsir start`, `ahsir list/chat/status/ping`) |
| `cmd/ahsir-agent/` | Per-agent process; loads agent-card, hosts A2A endpoint, drives the LLM CLI |
| `internal/scheduler/` | Config, agent lifecycle, registry, HTTP gateway |
| `internal/registry/` | Agent registration / heartbeat / lookup |
| `internal/wrapper/` | A2A server/client, executor, `SessionPool`, `ClaudeSession`, `CodexSession`, persistence + HA |
| `internal/schedulerclient/` | HTTP client used by the CLI to talk to the scheduler gateway |
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
fleet from Claude Code through the plugin skill and `ahsir chat`. Full hands-on
instructions live in [`example/README.md`](example/README.md).

## Install as a Claude Code plugin

ahsir ships as a Claude Code plugin so you can install it once and use it from inside any Claude Code session — without remembering `--scheduler` URLs or absolute binary paths.

The plugin bundles:

- The `ahsir` and `ahsir-agent` CLI binaries (pre-built per platform under `plugin/bin/<os>-<arch>/`).
- A small wrapper at `plugin/bin/ahsir` that auto-detects platform.
- A skill at `plugin/skills/orchestrator/SKILL.md` that teaches Claude **when** to use ahsir (parallel sub-tasks, specialist agents, multi-turn with a specific agent) and **how** to invoke it (`ahsir list`, `ahsir chat`, etc).

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

1. **The skill auto-loads** — Claude Code reads `plugin/skills/orchestrator/SKILL.md` and consults its `description` whenever you describe a task. When the description matches (you ask about delegation, multi-agent, parallel sub-tasks, specialist agents, or mention "ahsir" explicitly), Claude proposes using it.

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
# The CLI fetches `chat` from the scheduler and uses chat+1m as its own
# http.Client.Timeout, so this is the single knob you tune.
timeouts:
  chat: 10m
  task_status: 30s

port_range:
  start: 9801
  end: 9900
```

### `<workspace>/.a2a/agent-card.yaml` — per-agent config

System prompt, runtime backend (provider / baseURL / apiKey / model),
filesystem allow-list, pool limits, streaming settings, and the per-agent LLM
subprocess timeout.

```yaml
name: teacher
runtime:
  command: claude
  args: []
  timeout: 300s          # claude subprocess deadline
  provider: deepseek
  baseURL: https://api.deepseek.com/anthropic
  apiKey: "${MODEL_API_KEY}"
  model: deepseek-v4-pro
filesystem:
  enabled: true
  allowed_paths:
    - "."
    - "/tmp"
pool:
  max_active: 50          # optional; 0/unset = unlimited
  overload_policy: reject # or evict-lru
streaming:
  partial_messages: true  # ClaudeSession only; enables A2A SSE deltas
```

Runtime provider choices:

| Provider | Runtime backend | Notes |
|---|---|---|
| `anthropic` or empty | `ClaudeSession` via `claude -p --input-format=stream-json` | Uses local Claude auth unless `apiKey` / env are supplied. |
| `deepseek` | `ClaudeSession` against DeepSeek's Anthropic-compatible endpoint | Defaults `baseURL` to `https://api.deepseek.com/anthropic`; `apiKey` maps to `ANTHROPIC_AUTH_TOKEN`. |
| `zhipu` | `ClaudeSession` against Zhipu's Anthropic-compatible endpoint | Defaults `baseURL` to `https://open.bigmodel.cn/api/anthropic`. |
| `codex` | `CodexSession` via `codex exec --json` | `apiKey` maps to `CODEX_API_KEY`; `model` maps to `--model`; resume uses Codex `thread_id`. |

Codex-backed agent example:

```yaml
name: reviewer
runtime:
  command: codex
  args: ["--ignore-user-config", "--ignore-rules"]
  timeout: 300s
  provider: codex
  model: gpt-5.4        # optional; omit to use Codex CLI defaults
filesystem:
  enabled: true
  allowed_paths:
    - "."
```

Agents can mix providers freely. The e2e suite includes a real mixed run where
a Claude/DeepSeek-backed student delegates to a Codex-backed teacher over A2A.

## Timeout topology

There are three layers of deadlines; the invariant is **outer ≥ inner**.

```
CLI http.Client.Timeout  =  chat + 1m   ← fetched from /config/timeouts
gateway ctx              =  chat        ← timeouts.chat in ahsir.yaml
agent runtime.timeout    =  300s        ← per agent-card.yaml
```

Tune the outer two via `timeouts:` in `ahsir.yaml`. The per-agent subprocess
deadline stays per-agent because it is intrinsic to that agent's expected
response latency (a fast classifier vs. a deep researcher legitimately differ).

## Diagnostics: reading the logs

Every agent has a `SessionPool` keyed by A2A `contextId`. The pool returns the
same provider session for repeated turns in the same conversation and persists
`contextId → runtime session id` under `<workspace>/.a2a/sessions.json`.

For Claude-backed agents, the pool owns one long-running `claude` subprocess
per active context. Session starts look like this:

```
claude session: started pid=59108 cmd=claude args=[-p --input-format=stream-json --output-format=stream-json --verbose]
```

When the pool resumes an evicted, restarted, or unhealthy Claude session, the
line carries `--resume=<id>`:

```
claude session: started pid=67914 cmd=claude args=[... --resume=4a038c6b-f0cb-4ea6-ad1c-05eb7741511c]
```

For Codex-backed agents, each turn starts a `codex exec --json` process. The
first turn records Codex's `thread_id`; later turns for the same context use
`codex exec resume <thread_id>`:

```
codex session: started pid=70022 cmd=codex args=[exec --json --sandbox=read-only --skip-git-repo-check ...]
codex session: started pid=70548 cmd=codex args=[exec --json --sandbox=read-only --skip-git-repo-check resume 019e99a5-... ...]
```

Inter-agent traffic and per-request receive markers:

```
[teacher] receive: contextID=demo msgID=... mode=send text="..."
[student → teacher] A2A_CALL: contextID=demo depth=0 source=legacy_text task="..."
[student ← teacher] reply: contextID=demo depth=0 took=12.3s bytes=... preview="..."
```

Agent-to-agent dispatch prefers structured runtime tool-use events named
`a2a_call` / `call_agent` with JSON input `{"agent":"...","task":"..."}`.
The legacy `---A2A_CALL---` text block is still supported as a fallback for
providers or prompts that cannot emit structured tool calls.

Performance timing logs are emitted for every major phase in the path:

```
[student] send done contextID=demo msgID=... state=completed history=3 took=24.7s
[student] executor open_session done contextID=demo msgID=... took=12.4ms
[student] executor prompt_ready contextID=demo msgID=... agents=2 user_bytes=91 prompt_bytes=812 took=1.1ms
[student] executor turn done contextID=demo depth=0 took=7.2s stream_open=1.3ms events=4 response_bytes=120 input_tokens=... output_tokens=... provider_duration_ms=...
session pool: lookup contextID=demo outcome=hit state=active sessionID=... took=35µs
```

Read them as a waterfall:

- `send done` is the whole inbound A2A handler time for one request.
- `executor open_session` plus `session pool: lookup` shows pool overhead and
  whether this was `hit`, `create`, `resume`, or `capacity_reject`.
- `executor prompt_ready` is prompt construction and registry agent listing.
- `executor turn done` is the provider turn. `took` is wrapper-observed wall
  time; `provider_duration_ms` is what the provider reported, when available.
- `[X → Y] A2A_CALL` / `[X ← Y] reply` wraps the full child-agent call,
  including that child agent's own provider work.
- `executor injection_ready` is result-injection prompt construction before
  the parent agent's follow-up turn.

Useful greps:

| Grep | What it tells you |
|---|---|
| `claude session: started` | Every new Claude runtime process (one per active contextId, per agent) |
| `codex session: started` | Every Codex non-interactive turn |
| `--resume=` | Claude pool eviction recovery, cross-restart resume, or self-healing on SIGKILL |
| ` exec resume ` | Codex turn resumed from a prior `thread_id` |
| `[teacher]` / `[student]` | Per-agent request/log filtering |
| `[X → Y] A2A_CALL` | Cross-agent delegations |
| `contextID=<id>` | Full waterfall for one conversation |
| `executor turn done` | Provider turn timings and token/cost stats |
| `session pool: lookup` | Session reuse / create / resume / capacity behavior |

If you suspect the time is being spent outside the LLM (in scheduler /
serialization), compare the elapsed sum across all agent log lines
against your end-to-end latency. A large gap means the overhead is in the
chain, not in the model.

## Run the tests

### Default suite (mocks, no API key required)

```bash
go test ./...
```

Includes:

- Unit tests for registry, wrapper, scheduler, and CLI command wiring.
- An end-to-end gateway test (`internal/scheduler/gateway_test.go`) that spins
  up a real A2A server with a mock executor and exercises both the direct A2A
  path and the scheduler-gateway path.

No `MODEL_API_KEY` or live `claude` CLI required — the default suite uses mocks.

### End-to-end with real LLMs (opt-in)

The `e2e/` package holds top-to-bottom integration tests that spawn the real
scheduler subprocess against real provider CLIs. Build-tagged `e2e` so they
never run in the default pipeline.

```bash
# Build binaries first
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent

# ClaudeSession / Anthropic-compatible provider path
AHSIR_E2E_CLAUDE=1 MODEL_API_KEY=<your-deepseek-key> \
  go test -tags=e2e -timeout=5m -v ./e2e/

# CodexSession path
AHSIR_E2E_CODEX=1 \
  go test -tags=e2e -timeout=8m -v ./e2e/ -run TestCodexProvider_E2E

# Mixed provider path: Claude/DeepSeek student delegates to Codex teacher
AHSIR_E2E_MIXED=1 MODEL_API_KEY=<your-deepseek-key> \
  go test -tags=e2e -timeout=8m -v ./e2e/ -run TestMixedClaudeAndCodexCollaborate_E2E
```

Tests skip gracefully if `bin/ahsir(-agent)` isn't built, the required provider
CLI is not on PATH, or the matching gate/env vars are missing — so the same
commands can be wired into CI conditionally without manual gating.

There's also a lower-level e2e at `internal/wrapper/session_claude_e2e_test.go`
that exercises `ClaudeSession` directly against real claude (no scheduler /
A2A layer). Same env-var + build-tag gates.
