# ahsir — A Multi-Agent Scheduler over A2A

`ahsir` is a small Go scheduler that runs multiple LLM-backed agents as local
processes, lets them talk to each other over the
[A2A protocol](https://google.github.io/A2A/), and lets Claude Code or Codex
drive the fleet through the bundled plugin skill plus the `ahsir` CLI.

Each agent is an `ahsir-agent` process with an A2A JSON-RPC HTTP endpoint and
a provider-backed `Session` implementation. Today the production session
backends are:

- `ClaudeSession`: one long-running `claude -p --input-format=stream-json`
  subprocess per A2A `contextId`, with `--resume` recovery.
- `CodexSession`: one `codex exec --json` subprocess per turn, resuming by
  Codex `thread_id` for the same A2A `contextId`.

The scheduler owns the agent registry and a gateway that forwards chat /
task-status requests. The bundled Claude Code and Codex plugin manifests expose
the same skill and CLI wrappers, so both hosts learn when to use ahsir and how
to call `ahsir list`, `ahsir chat`, and related commands.

For local agents started by the scheduler, ahsir also acts as a lightweight
supervisor: if an `ahsir-agent` process exits unexpectedly, or if its
`/healthz` endpoint fails repeatedly, the scheduler restarts it on the same
port with exponential backoff. Explicit `ahsir agent delete` / scheduler
shutdown still stop the process without restart.

## Current Feature Set

- **Scheduler-owned A2A entrypoint**: public native A2A traffic goes through
  `POST /a2a/{agent}` so user calls and agent-to-agent calls are visible to the
  scheduler.
- **CLI + plugin workflow**: Claude Code and Codex use the bundled
  `orchestrator` skill and `ahsir` CLI. The old MCP shim design is historical
  only.
- **Multi-provider sessions**: Claude-backed agents resume with
  `claude --resume=<session_id>`; Codex-backed agents resume with
  `codex exec resume <thread_id>`.
- **Persistent session mapping**: each agent writes
  `<workspace>/.a2a/sessions.json`, bounded by `pool.evicted_ttl` and
  `pool.max_evicted`.
- **Process health and restart**: scheduler monitors exits and `/healthz`
  failures, restarts local agents on the same port, and leaves intentional
  stops terminal.
- **Dynamic agent lifecycle (no scheduler restart)**: `ahsir agent new <name>`
  scaffolds the card, registers it in `ahsir.yaml`, and starts it on the
  *running* scheduler via the admin API (`POST /admin/agents`) — the new agent
  hot-registers and is usable in `list` / `chat` immediately. `ahsir agent
  delete` stops it (files preserved). A restart is only needed with
  `--skip-start`, when the scheduler was down, or after hand-editing
  `ahsir.yaml`; editing an existing card restarts *that agent* (`POST
  /admin/agents/{name}/restart`), not the scheduler.
- **Invocation ledger and continuation prompt**: scheduler records
  mediated calls in `.ahsir/ledger.jsonl`, replays it on startup, compacts old
  records, and sends a continuation prompt after a supervised restart for
  recoverable work with a `contextId`.
- **Shared-context collaboration**: multiple clients can work in one
  `contextId` — `ahsir chat --as <name>` attributes each turn to a speaker
  (default: OS username), concurrent turns wait in a per-context FIFO queue
  (`pool.queue_depth`, default 4; `0` restores fail-fast busy/409), and
  `ahsir history <agent> <contextId>` replays the full transcript for whoever
  takes over. `ahsir chat --async` returns a taskId immediately; poll it with
  `ahsir status`.
- **Control-plane auth**: an auto-generated admin token gates the privileged
  endpoints (agent start/stop + registry write). See *Authentication* below.
- **Web console (`ahsir ui`)**: an optional, separate process that serves a
  single-page browser console and proxies the gateway. Pick an agent and chat
  (Markdown-rendered replies, speaker + timestamp per bubble), browse contexts,
  run rooms, and read the trace. Per-bubble 复制 / 直接回复 actions, drag a
  participant to `@`-mention it, drop or paste a file onto the composer to attach
  its local path, click a trace node to jump to its bubble, and get a
  sound + popup + flashing-tab alert when an agent `@`-mentions you. Holds no
  LLM — a thin proxy + SPA.
- **Rooms (multi-agent group conversation)** — two floor-control modes
  (`mode` on `POST /rooms`):
  - **多 Agent 协同 (`relay`)**: hub-and-spoke, `@`-mention turn-taking (operator
    *and* agents), an operator-or-agent moderator, incremental relay, runaway
    guard. See [`example/roundtable/`](./example/roundtable/).
  - **圆桌 (`roundtable`)**: the real round-table — **consensus rounds**. Each
    round is a full pass over participants in random order; they speak or reply
    `同意`; a dedicated moderator agent consolidates 已达成/待议 each round (under
    its own contextId) until consensus, or a budget parks it for the operator.
- **MCP isolation**: agents run `claude --strict-mcp-config`, so they never
  inherit the host's global/project MCP servers. Default is zero MCP (fast,
  cheap, no privilege bleed); opt in per-agent via the card's `mcp.servers` or
  `ahsir agent new --mcp-config`.

📖 **For a detailed, feature-by-feature tour, see [`docs/features.md`](./docs/features.md)** (EN/中文).

## Architecture

```
                   ┌─────────────────────────────────────────────┐
  Claude Code ───► │ ahsir plugin skill                         │
  or Codex         │ chooses ahsir CLI commands                  │
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
                    │  A2A proxy         /a2a/{name}              │
                    │  config            /config/timeouts         │
                    └─────────────────┬───────────────────────────┘
                                     │ A2A JSON-RPC message/send
                                     │ or message/stream
            ┌─────────────────────────┴───────────────────────────┐
            ▼                                                     ▼
 ┌───────────────────────────────┐                    ┌───────────────────────────────┐
 │ ahsir-agent: student          │                    │ ahsir-agent: teacher          │
 │ - internal A2A server         │                    │ - internal A2A server         │
 │ - executor handles agent calls│── via scheduler ──►│ - executor handles request    │
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

Each agent HTTP server exposes a few non-LLM operational endpoints alongside
the internal A2A JSON-RPC endpoint. Public A2A traffic should use the
scheduler-owned URL from the registry, `http://<scheduler>/a2a/{agentName}`;
direct agent ports are reserved for internal forwarding, health checks, and
local debugging. Local agents started by the scheduler require an internal
`X-Ahsir-Internal-Token` header for A2A JSON-RPC, and the scheduler proxy adds
that header automatically.

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness: the `ahsir-agent` process and HTTP server can answer. |
| `GET /readyz` | Readiness: the agent card is loaded and the executor is wired. |
| `GET /.well-known/agent-card.json` | A2A Agent Card discovery using the SDK's standard well-known path. |

The scheduler probes local agents through `/healthz` after a startup grace
period. Non-2xx responses, request timeouts, or connection failures count as
health failures; consecutive failures trigger the same restart supervisor used
for abnormal process exits.

Scheduler-mediated chat and A2A proxy calls are recorded in an append-only
invocation ledger at `<dir-of-ahsir.yaml>/.ahsir/ledger.jsonl`. The scheduler
replays this JSONL file on startup, compacts old records, and uses it after an
agent restart to continue interrupted `contextId` work.

**Privacy note — ledger vs transcript.** The ledger stores only a 512-byte
preview of user text (audit/recovery index, not a conversation store). The
per-context transcript at `<workspace>/.a2a/transcripts/` is the deliberate
exception: it records FULL turn content — speaker, user text, agent reply —
because replay for whoever joins a shared context is its entire purpose. Both
are written with owner-only permissions (`0700` dirs / `0600` files); the
sanctioned read path for transcripts is the internal-token-protected agent
`/history` endpoint, proxied as `GET /agents/{name}/history/{contextId}` and
rendered by `ahsir history`. Task handles from `--async` live in memory only:
after an agent restart a taskId answers 404 — the conversation itself survives
(sessions.json resume + transcript); check `ahsir history` to see whether the
turn ran before resending.

## Authentication

The scheduler's **control plane** — agent lifecycle (`POST/DELETE
/admin/agents`) and registry write (`POST/DELETE /agents`) — is gated by a
single **admin token**. The **data plane** (chat, history, task status, trace,
agent listing) stays open in this baseline.

**Trust boundary:** the OS user who can read the token file. On a shared
machine, a different OS user — unable to read the `0600` file — cannot start,
stop, or hijack agents.

**How the token is resolved (scheduler and CLI agree):**

1. `AHSIR_ADMIN_TOKEN` environment variable, if set (CI / containers — no file
   is touched); else
2. a `0600` file named `admin-token` beside the resolved `ahsir.yaml`
   (e.g. `~/.ahsir/admin-token`). `ahsir start` generates it on first run; the
   CLI reads the same file, so same-user usage needs **zero configuration**.

`ahsir agent new` / `ahsir agent delete` auto-discover the token and attach it.
`ahsir doctor` reports whether the CLI can find a token. The scheduler logs its
auth status (enabled + source) at startup.

**Rotation:** delete the `admin-token` file and restart the scheduler (a new
token is generated); or set/replace `AHSIR_ADMIN_TOKEN`.

**Token-leak hardening:** the scheduler sends its per-agent *internal* token
only to the loopback address it recorded when it spawned the agent — never to
the URL in a registry card (which a write could otherwise overwrite to an
attacker sink). Agents known only by a registry card receive no internal token.

**Out of scope (deferred):** non-loopback / cross-machine access, multiple
principals / RBAC, and data-plane auth. The code is structured so data-plane
gating can be added when a non-loopback bind is introduced.

## Repo layout

| Path | Purpose |
|---|---|
| `cmd/ahsir/` | Scheduler + user CLI (`ahsir start`, `ahsir list/chat/status/ping`) |
| `cmd/ahsir-agent/` | Per-agent process; loads agent-card, hosts A2A endpoint, drives the LLM CLI |
| `internal/scheduler/` | Config, agent lifecycle, registry, HTTP gateway |
| `internal/registry/` | Agent registration / heartbeat / lookup |
| `internal/wrapper/` | A2A server/client, executor, `SessionPool`, `ClaudeSession`, `CodexSession`, persistence + HA |
| `internal/schedulerclient/` | HTTP client used by the CLI to talk to the scheduler gateway |
| `internal/ui/` | Web console server (`ahsir ui`): `/api/*` proxy + aggregation, embedded SPA |
| `example/` | Runnable walkthroughs: simple, session reuse, restart recovery, multi-agent delegation, roundtable |
| `docs/features.md` | Detailed, feature-by-feature guide |
| `docs/superpowers/` | Specs, plans, and design notes |

## Quick start

> **Prerequisite — Go toolchain required.** The plugin no longer ships pre-built
> binaries; it bundles its source and compiles `ahsir` / `ahsir-agent` on your
> machine the first time it runs. You need Go installed (`go version`); the build
> is cached under `~/.cache/ahsir/<version>/`, so it's a one-time cost per
> release. Supported targets: **darwin** / **linux** × **arm64** / **amd64**.

Install the plugin from the antcode marketplace inside a Claude Code session:

```
/plugin marketplace add https://github.com/wu8685/ahsir.git
/plugin install ahsir@ahsir
```

A `SessionStart` hook warms the build cache on the first session, so the first
real `ahsir` call runs straight from the cached binary.

Then start the scheduler with the multi-agent example config (run from a repo
clone, which ships the `example/` walkthroughs):

```bash
# Start the scheduler with the multi-agent example config
ahsir start example/multi-agent/ahsir.yaml

# (Optional) open the web console in a second terminal
ahsir ui --addr 127.0.0.1:9801 --scheduler http://127.0.0.1:9800
# → http://127.0.0.1:9801
```

The bundled examples default to Anthropic / claude-opus-4-8 and reuse your local
Claude Code credentials, so no API-key export is needed. To point an agent at a
different provider, set the API-key env var its agent-card references (e.g.
`MODEL_API_KEY` for DeepSeek/Zhipu).

Then either curl the scheduler A2A endpoint, hit the scheduler chat gateway,
drive the fleet from Claude Code / Codex through the plugin skill and
`ahsir chat`, or use the web console. Hands-on walkthroughs live in
[`example/README.md`](example/README.md): `simple/` for the smallest path,
`session-reuse/` for `contextId` continuity, `recovery-continuation/` for restart
continuation and session retention, `multi-agent/` for delegation, and
`roundtable/` for multi-agent group chat. For a feature-by-feature reference, see
[`docs/features.md`](docs/features.md).

## Install as a Codex plugin

ahsir can be installed into Codex with the same user experience as Claude Code:
the plugin exposes the same orchestrator skill and the same `plugin/bin/ahsir`
wrapper, which compiles and caches the binary for your OS/arch on first use.
**Go must be installed** (see the [Quick start](#quick-start) prerequisite).

### Install (recommended: via marketplace)

```bash
codex plugin marketplace add https://github.com/wu8685/ahsir.git
codex plugin add ahsir@ahsir
```

Then add the installed plugin's wrapper to your shell PATH so `ahsir` also
works from a normal terminal:

```bash
AHSIR_PLUGIN_DIR="$(codex plugin list | awk '/ahsir@ahsir/ {print $NF; exit}')"
echo "export PATH=\"$AHSIR_PLUGIN_DIR/bin:\$PATH\"" >> ~/.zshrc
exec zsh
```

Supported platforms: **darwin-arm64**, **darwin-amd64**, **linux-amd64**,
**linux-arm64** — the binary is compiled locally on first use, so any of these
works as long as Go is installed.

### Install (alternative: local clone, for development)

```bash
git clone https://github.com/wu8685/ahsir.git
cd ahsir
# The plugin/src/ source bundle is already committed; run `make plugin` only to
# refresh it after changing cmd/, internal/, or dependencies.

codex plugin marketplace add "$(pwd)"
codex plugin add ahsir@ahsir

AHSIR_PLUGIN_DIR="$(codex plugin list | awk '/ahsir@ahsir/ {print $NF; exit}')"
echo "export PATH=\"$AHSIR_PLUGIN_DIR/bin:\$PATH\"" >> ~/.zshrc
exec zsh
```

Once installed, Codex sees the `orchestrator` skill and can use `ahsir ping`,
`ahsir list`, `ahsir chat`, `ahsir status`, and `ahsir agent ...` through its
shell tools. The scheduler is still a local service; start it in a separate
terminal before delegating work:

```bash
# Needed when an agent-card references an API-key env var. The bundled examples
# use ${ANTHROPIC_AUTH_TOKEN} (Anthropic); DeepSeek/Zhipu cards use ${MODEL_API_KEY}.
export ANTHROPIC_AUTH_TOKEN=<your-anthropic-key>
ahsir start path/to/your/ahsir.yaml
```

## Install as a Claude Code plugin

ahsir ships as a Claude Code plugin so you can install it once and use it from inside any Claude Code session — without remembering `--scheduler` URLs or absolute binary paths.

> **Prerequisite — Go toolchain required** (`go version`). The plugin ships its
> Go source (vendored) under `plugin/src/` and compiles the binaries on your
> machine on first use, caching them under `~/.cache/ahsir/<version>/`.

The plugin bundles:

- The vendored Go source under `plugin/src/`, compiled on first use into `ahsir` and `ahsir-agent` (cached under `~/.cache/ahsir/<version>/`).
- A small wrapper at `plugin/bin/ahsir` that detects platform, builds-on-demand, and execs the cached binary; a `SessionStart` hook warms that cache.
- A skill at `plugin/skills/orchestrator/SKILL.md` that teaches Claude **when** to use ahsir (parallel sub-tasks, specialist agents, multi-turn with a specific agent) and **how** to invoke it (`ahsir list`, `ahsir chat`, etc).

### Install (recommended: via marketplace)

Claude Code's plugin system uses a git-based marketplace model — no central registry, no upload step. The repo's root holds a `.claude-plugin/marketplace.json` catalog, and Claude Code clones the repo on `marketplace add`.

From inside any Claude Code session, run the two slash commands below:

```
/plugin marketplace add https://github.com/wu8685/ahsir.git
/plugin install ahsir@ahsir
```

The `SessionStart` hook compiles the binaries for your OS/arch on the first session (one-time, cached afterward), so subsequent `ahsir` / `ahsir-agent` calls resolve straight to the cache. The first `ahsir` is the plugin name; `@ahsir` is the marketplace name (both happen to be "ahsir" here because this repo is single-plugin).

Then add the wrappers to your shell PATH so the same `ahsir` binary works from a normal terminal too — not just Claude Code's Bash tool. Claude Code installs marketplaces under `~/.claude/plugins/<marketplace>/`:

```bash
echo 'export PATH="$HOME/.claude/plugins/ahsir/plugin/bin:$PATH"' >> ~/.zshrc
exec zsh
```

Supported platforms: **darwin-arm64**, **darwin-amd64**, **linux-amd64**, **linux-arm64** — compiled locally on first use, so any of these works with Go installed.

### Install (alternative: local clone, for development)

If you're hacking on ahsir itself, clone the repo and point Claude Code at the working tree directly:

```bash
# 1. Clone the repo (or `git pull` to update an existing clone).
git clone https://github.com/wu8685/ahsir.git
cd ahsir

# 2. (Optional) Refresh the vendored source bundle the plugin compiles from.
#    Only needed after changing cmd/, internal/, or dependencies.
make plugin             # syncs plugin/src/ (cmd, internal, go.mod, vendored deps)

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

Before tagging a release: run `make plugin` to refresh the vendored source bundle in `plugin/src/` (cmd, internal, go.mod, deps), then commit it. Marketplace installers compile from that bundle on first use — there are no per-platform binaries to cross-compile or commit anymore.

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
# Needed when an agent-card references an API-key env var. The bundled examples
# use ${ANTHROPIC_AUTH_TOKEN} (Anthropic); DeepSeek/Zhipu cards use ${MODEL_API_KEY}.
export ANTHROPIC_AUTH_TOKEN=<your-anthropic-key>
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
```

Each agent's `workspace` holds its private `.a2a/` state (card, sessions,
transcripts) and **must be unique** per agent. By default it is also the LLM's
working directory (cwd). To have several agents operate in the **same** project
directory while keeping their own private workspaces, set an optional `workdir`
(defaults to `workspace`):

```yaml
agents:
  - name: teacher
    workspace: ~/.ahsir/agents/teacher    # private .a2a state (unique)
    workdir: ~/projects/shared-kb         # shared cwd (can repeat across agents)
  - name: student
    workspace: ~/.ahsir/agents/student
    workdir: ~/projects/shared-kb

registry:
  host: "127.0.0.1"
  port: 9800
  heartbeat_interval: 10s
  heartbeat_timeout: 30s

# Outer-envelope timeouts. Optional — defaults shown.
# `chat` MUST be >= the largest agent's runtime.timeout (in agent-card.yaml).
# Set `chat: 0s` only for intentional long-running work with no scheduler
# gateway deadline; hung providers will then keep the request open.
# The CLI fetches `chat` from the scheduler and uses chat+1m as its own
# http.Client.Timeout for positive values. With `chat: 0s`, the CLI also
# disables its own HTTP timeout.
timeouts:
  chat: 10m
  task_status: 30s

port_range:
  start: 9802
  end: 9900
```

### `<workspace>/.a2a/agent-card.yaml` — per-agent config

System prompt, runtime backend (provider / baseURL / apiKey / model),
filesystem allow-list, pool limits, streaming settings, and the per-agent
provider deadline.

```yaml
name: teacher
runtime:
  command: claude
  args: []
  timeout: 300s          # provider turn deadline; set 0s for no provider deadline
  provider: anthropic
  apiKey: "${ANTHROPIC_AUTH_TOKEN}"  # official api.anthropic.com; omit to use local `claude` login
  model: claude-opus-4-8
filesystem:
  enabled: true
  allowed_paths:
    - "."
    - "/tmp"
pool:
  max_active: 50          # optional; 0/unset = unlimited
  max_evicted: 1000       # inactive resume mappings retained; oldest evicted records are deleted first
  idle_ttl: 30m           # active session idle time before closing the live process
  evicted_ttl: 30d        # inactive mapping TTL; accepts Go durations plus day suffix such as 30d
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

`ahsir agent new` scaffolds `provider: anthropic` with `model: claude-opus-4-8`,
no `baseURL` (official `api.anthropic.com`), and `apiKey: "${ANTHROPIC_AUTH_TOKEN}"`
by default. Override any of these with `--provider` / `--model` / `--base-url` /
`--api-key-env`.

Anthropic local-login example:

```yaml
name: reviewer
runtime:
  command: claude
  args: []
  timeout: 300s
  provider: anthropic
  model: claude-opus-4-8
```

With `provider: anthropic`, leave `baseURL` and `apiKey` unset to use the
local Claude Code auth from `/login`. `runtime.model` is translated into
`ANTHROPIC_MODEL` for the underlying `claude -p --input-format=stream-json`
process, so it can select a local-login model such as `claude-opus-4-8`.
Do not set `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` unless you
intentionally want API-key auth instead of local login.

If you do want API-key auth for Anthropic, keep `provider: anthropic` and set
`apiKey` explicitly (literal or `${ENV_VAR}`). If you set `baseURL`, ahsir
requires `apiKey` as well, because Anthropic-compatible gateways usually return
401s without an auth token.

Codex-backed agent example:

```yaml
name: reviewer
runtime:
  command: codex
  args: ["--ignore-user-config", "--ignore-rules"]
  timeout: 300s          # set 0s for no Codex turn deadline
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
CLI http.Client.Timeout  =  chat + 1m, or 0 when chat=0s
gateway ctx              =  chat, or no deadline when chat=0s
agent runtime.timeout    =  300s, or 0s for no provider deadline
```

Tune the outer two via `timeouts:` in `ahsir.yaml`. The per-agent provider
deadline stays per-agent because it is intrinsic to that agent's expected
response latency (a fast classifier vs. a deep researcher legitimately differ).
For intentional long-running work, set both `timeouts.chat: 0s` and that
agent's `runtime.timeout: 0s`; hung providers will then keep the request open
until manually stopped or the scheduler/agent process exits.

## Diagnostics: reading the logs

Every agent has a `SessionPool` keyed by A2A `contextId`. The pool returns the
same provider session for repeated turns in the same conversation and persists
`contextId → runtime session id` under `<workspace>/.a2a/sessions.json`.
Active sessions idle out after `pool.idle_ttl` and become inactive mappings;
inactive mappings are retained until `pool.evicted_ttl` or until
`pool.max_evicted` is exceeded, in which case the oldest inactive mappings are
deleted first. Active mappings are not deleted by `pool.max_evicted`.

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
| `Agent <name> recovery:` | Scheduler restart continuation prompt attempts and outcomes |

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
  up a real A2A server with a mock executor and exercises the scheduler-owned
  A2A proxy plus the scheduler chat gateway path.

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
