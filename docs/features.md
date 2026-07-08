# ahsir — Feature Guide

A detailed tour of what ahsir does, feature by feature. For a quick start see the
[root README](../README.md); for runnable walkthroughs see [`example/`](../example/).

ahsir is a local **multi-agent scheduler** over the [A2A protocol](https://google.github.io/A2A/).
It hosts several long-lived "agent" processes — each backed by a provider CLI
(`claude` or `codex`) with its own system prompt, skills, filesystem permissions,
and MCP configuration — and lets you (or other agents) hand work to them and get
replies back.

---

## 1. The scheduler and its gateway

`ahsir start <config>` runs a long-lived scheduler that owns a single HTTP
gateway (default `127.0.0.1:9800`). One port serves both the agent registry and
the user-facing chat/task/history/trace endpoints:

| Endpoint | Purpose |
|---|---|
| `GET  /agents` | List registered agents + online status |
| `GET  /agents/{name}` | One agent's public card |
| `POST /agents/{name}/chat` | Send a message; `{message, contextId?, speaker?, async?}` |
| `GET  /agents/{name}/tasks/{id}` | Poll an async task |
| `GET  /agents/{name}/history/{contextId}` | Replay an agent's transcript for a context |
| `POST /a2a/{name}` | Native A2A JSON-RPC, proxied through the scheduler |
| `GET  /invocations` | The invocation ledger (backs `trace`) |
| `POST /rooms …` | Roundtable group chat (see §6) |
| `POST /admin/agents …` | Control plane: start/stop agents (token-gated) |

Everything the CLI, the console, and plugins do is built on these.

---

## 2. CLI

The `ahsir` binary is both the scheduler (`ahsir start`) and a thin client for a
running one. The client commands are designed for human eyes *and* for Claude
Code / Codex driving them via a shell:

```bash
ahsir list                                  # registered agents
ahsir chat <agent> "<msg>" [--context ID] [--as NAME] [--async] [--stream]
ahsir status <agent> <task-id>              # poll a task
ahsir history <agent> <contextId> [--json]  # replay a transcript
ahsir trace [contextId] [--json]            # invocation timeline
ahsir ping                                  # liveness (exit 0/2)
ahsir doctor                                # config + provider + auth checklist
ahsir agent new|delete|list-configs         # manage personas (see §4)
ahsir ui                                    # web console (see §5)
ahsir version                               # print the build version (also --version / -v)
```

`chat` submits asynchronously and polls internally, so the UX is "reply on
stdout" while arbitrarily long queue waits survive proxies and timeouts.

---

## 3. Sessions, contextId, and conversation memory

Each agent holds conversation history in its provider process (`claude`/`codex`),
keyed by the A2A **`contextId`**:

- **Pass a stable `--context`** and the agent's `SessionPool` reuses the same
  live process across calls — memory persists.
- **Omit it** and the call is isolated.
- A `contextId` deterministically maps to one session per agent. The scheduler
  mints one when you don't supply it, and uses the *same* id for the reply
  handle, the ledger record, and the on-disk transcript — so `history`/`trace`
  always resolve. (This coherence is enforced end-to-end; see the
  [session-reuse example](../example/session-reuse/).)

Mappings survive restarts via `<workspace>/.a2a/sessions.json`, bounded by
`pool.evicted_ttl` / `pool.max_evicted`, so a restarted agent can `--resume`.

**Retention & deletion.** There is no manual "delete conversation" action; records
are pruned by time, automatically, at process **startup** (compaction):

| Store | What it holds | Retention | Compacted at |
| --- | --- | --- | --- |
| Invocation ledger (scheduler) | drives the console's 会话 list + 轨迹 | completed **7 days**, incomplete **30 days** | scheduler start |
| Transcript (per agent) | each context's message history (`history`) | most recent turn older than **30 days** → file + `index.json` entry removed | agent start |
| Room logs (scheduler) | roundtable transcripts | last activity older than **30 days** | scheduler start |

Separately, `pool.evicted_ttl` governs how long the **session mapping** is
remembered — i.e. whether an old conversation can still `--resume` — independent
of whether its transcript/ledger records still exist.

### Shared-context collaboration

Several parties can work in one `contextId`. `--as <name>` attributes each turn
to a speaker (the executor tags every message `[speaker: <name>]` so the model
knows who said what). Concurrent turns on one context wait in a per-context FIFO
queue (`pool.queue_depth`, default 4; `0` restores fail-fast 409). This is the
substrate the roundtable (§6) is built on.

---

## 4. Personas: agents on demand

An agent is defined by a `<workspace>/.a2a/agent-card.yaml` plus an entry in
`ahsir.yaml`. Create one without hand-editing files:

```bash
ahsir agent new security-reviewer \
  --prompt "You review Go web apps for security issues; cite file:line." \
  --skill "security-review=find authn/authz/injection bugs" \
  --allow-fs /path/to/repo \
  --model claude-opus-4-8 \
  --mcp-config ./servers.json     # optional, see §7
```

This scaffolds the card, appends to `ahsir.yaml`, and asks the running scheduler
to spin the agent up immediately. `ahsir agent delete <name>` stops it and removes
the config entry (workspace files are preserved). The control plane is token-gated
(see §8); for the normal same-user case the CLI discovers the token automatically.

**Adding an agent needs no scheduler restart.** `ahsir agent new` hot-registers
and starts the agent on the live scheduler via the admin API; it's usable in
`list` / `chat` right away. A restart (or extra step) is only needed when:

- you pass `--skip-start` (scaffold only — the agent starts on the next `ahsir start`);
- the scheduler was **down** when you ran `agent new` (files are written; `ahsir start` loads it);
- you **hand-edited `ahsir.yaml`** instead of using `agent new` (the running scheduler doesn't watch that file — restart it, or `POST /admin/agents`).

Editing an **existing** agent's card is different: restart *that agent*, not the
whole scheduler — console「重启生效」or `POST /admin/agents/{name}/restart`.

Per-agent knobs in the card: `claude.systemPrompt`, `skills`, `runtime`
(provider/model/timeout), `filesystem.allowed_paths`, `pool` (concurrency caps),
`streaming.partial_messages`, and `mcp.servers`.

**Workspace vs working directory.** Each agent's `workspace` (set in `ahsir.yaml`)
holds its private `.a2a/` state — card, sessions, transcripts — and **must be
unique** per agent. By default it is also the LLM's cwd (and the base for relative
`filesystem.allowed_paths`). To let several agents operate in the **same** project
directory while keeping their own private workspaces, give each an optional
`workdir` (defaults to `workspace`); point them all at one shared path. CLI:
`ahsir agent new <name> --workdir /path/to/shared`.

---

## 5. Web console (`ahsir ui`)

An optional, **separate process** that serves a single-page browser console and
proxies the scheduler's gateway:

```bash
ahsir ui --addr 127.0.0.1:9801 --scheduler http://127.0.0.1:9800
```

It holds no LLM and does no orchestration — it's a thin proxy + static SPA. What
it gives you:

- **Pick an agent and chat** 1:1; replies render as **Markdown** (headings,
  bold/italic, code blocks, lists, blockquotes, links, **GFM tables**). Each
  bubble shows the **speaker** and a **timestamp**.
- A **conversation list** on the left, derived from the invocation ledger and
  keyed on `contextId` (one context can chain several agents' sessions).
- A right rail with the selected agent's **card details** and the **轨迹 (trace)**
  timeline for the current context.
- **Rooms** — both **多 Agent 协同 (relay)** and **圆桌 (roundtable)** create
  flows (§6); the room list and header tag the mode. A roundtable thread shows a
  **第 X 轮** divider per round and renders the moderator's consolidation as a
  distinct 小结 card; the trace gets per-round dividers too.
- **Per-message actions** (revealed on hover): **复制** copies the raw message
  text; **直接回复** (roundtable agent bubbles only) drops `@<speaker>` into the
  composer so you address that agent without typing the mention. The right-rail
  participant rows and the agent-card header are also **draggable onto the
  composer** to insert `@<agent>` at the caret.
- **Mentions + alerts**: in a room, an `@<participant>` renders as blue text and
  **`@operator` (you)** as a deep-blue pill. A new agent `@operator` always shows
  an in-page toast; when the tab is **not active** (hidden or unfocused) it also
  fires a **sound + browser popup + flashing tab title/favicon**, which stops the
  moment you return to (or are already on) the page. Audio/popup unlock on first
  interaction.
- **Navigation**: click a **轨迹 (trace)** node to scroll the thread to that
  bubble (with a flash), and a floating **jump-to-bottom** button when scrolled up.
- **Drop or paste a file onto the composer** to attach its local path (paste a
  clipboard image with `⌘V` too). Browsers hide a dragged/pasted file's real path,
  so the console uploads the bytes to `POST /api/upload` and copies them into a
  local **upload dir**, then inserts that absolute path at the caret (claude reads
  the file itself, e.g. `看下 /tmp/.../shot.png 报的什么错`). Any file type,
  multiple files at once.

The upload dir defaults to `$TMPDIR/ahsir-uploads` (override with
`ahsir ui --upload-dir <path>`, or set `AHSIR_UPLOAD_DIR` to align the console
*and* the agents in one place). For an agent to actually read a dropped file it
must run with `filesystem.enabled: true` — such agents **auto-allow-list the
upload dir** (`--add-dir`) at startup, so this works with zero per-agent config.
Local-only by design: the path is valid because the console, the upload dir, and
the agent share one host.

The console is the spine for the contextId model: the left rail lists contexts,
and per-agent history is a straight proxy to `/agents/{name}/history/{contextId}`.
Markdown rendering is XSS-safe (escape first, then tokenize to a fixed tag set;
links limited to http(s)/mailto).

---

## 6. Rooms: 多 Agent 协同 (relay) and 圆桌 (roundtable)

A **room** hosts several agents in one shared conversation — they see each
other's turns. The room id *is* the shared `contextId`, so it reuses everything
in §3 with no wrapper changes. A room has one of two **floor-control modes**
(`mode` on `POST /rooms`; default `relay`):

### 6a. 多 Agent 协同 — relay (`mode: relay`)

Hub-and-spoke, `@`-mention driven. Good for delegation-flavored collaboration.
Walkthrough: [`example/roundtable/`](../example/roundtable/).

- **@-mention turn-taking**, usable by the operator *and* the agents: the first
  `@<participant>` in a message becomes the next speaker, so an agent can hand a
  question to a peer. Mentions inside code spans are quoted tokens, not directives.
- **Organizer fallback**: a message addressing no one returns control to the
  room's organizer — `operator` (parks for your next message) or a designated
  **moderator agent** (it takes the floor and picks who continues).
- **Incremental relay**: each agent's turn is fed only the transcript it hasn't
  seen since it last spoke (a first-timer gets the full history). Bounded cost,
  no broadcast storm, no context loss.
- **Runaway guard**: an autonomous agent↔agent chain is bounded by `maxChain`
  turns between operator messages (default 8); each operator message resets it.

### 6b. 圆桌 — roundtable (`mode: roundtable`)

The real round-table: **consensus rounds** (Texas Hold'em). Flat, no chair —
for a hard, ambiguous decision (e.g. an OKR review).

- **A round = one full pass** over the participants in a **fresh random order**;
  each is asked in turn (a later speaker sees earlier same-round turns — shared
  broadcast). Each either contributes or, with no objection, replies `同意`. The
  convention is injected per-turn, so **no agent system-prompt change is needed**.
- **Rolling consolidation.** A dedicated **moderator agent** (not a participant)
  runs under its own `<roomId>#mod` contextId — never polluting the shared
  transcript or any participant's session, and with **no** say in who speaks —
  and each round produces a visible 小结 with three columns that **anchors the
  next round**: **【已达成】** (locked, carried forward, not relitigated unless an
  assumption breaks), **【待议】** (open, shrinking), and **【纠偏】** (steering —
  it names `@<agent>` whose reasoning drifted off the proposition / over-diverged
  and tells them to reconverge next round). When 待议 empties, that's consensus
  and its 已达成 is the decision.
- **Budget** (default 12 rounds) without consensus → park for the operator. A
  new operator message (`/say`) re-opens a fresh cycle on a new question.
- Create with `moderator` + optional `budget`:
  `POST /rooms {"mode":"roundtable","participants":[...],"moderator":"<agent>","budget":12,"message":"<question>"}`.
- Design: [`docs/superpowers/specs/2026-06-10-roundtable-mode.md`](./superpowers/specs/2026-06-10-roundtable-mode.md).

### Shared by both modes

HTTP surface: `POST /rooms` (create), `GET /rooms`, `GET /rooms/{id}`,
`POST /rooms/{id}/say`, `POST /rooms/{id}/stop`. The console drives both via its
**多 Agent 协同** and **圆桌** create flows. Turns are recorded in the ledger
(`source=roundtable`) so they show up in `trace`. Rooms are **persisted** (one
`<roomId>.jsonl` per room, carrying the mode) and reconstructed on scheduler
restart; a recovered room comes back **waiting** (a roundtable resumes a fresh
round on the next operator question). Room logs older than 30 days are compacted
at startup (see §3).

---

## 7. MCP isolation

Agents run their `claude` with **`--strict-mcp-config`**, so an agent **never
inherits** the operator's global (`~/.claude.json`) or project (`.mcp.json`) MCP
servers. The default — no `mcp:` block — means the agent runs with **zero MCP
servers**: the isolated, fast, cheap baseline (inherited MCP servers were
measured to balloon a trivial turn's input tokens and add per-call init latency,
and are a privilege-bleed risk).

Opt back in per-agent in the card (claude `mcpServers` shape, passed through
verbatim):

```yaml
mcp:
  servers:
    docs:
      command: npx
      args: ["-y", "@modelcontextprotocol/server-docs"]
```

…or scaffold it via `ahsir agent new --mcp-config <file>` (accepts a
`.mcp.json`-shaped file). Codex configures MCP through its own `CODEX_HOME`
`config.toml`, so `mcp.servers` on a codex agent is an error rather than a silent
no-op.

---

## 8. Recovery, health, and the ledger

- **Process health & restart**: the scheduler monitors agent exits and
  `/healthz` failures, restarts local agents on the same port, and leaves
  intentional stops terminal.
- **Invocation ledger**: mediated calls are recorded in `.ahsir/ledger.jsonl`,
  replayed on startup, and compacted over time. Backs `ahsir trace` and the
  console 轨迹 panel.
- **Continuation prompt**: after a supervised restart, recoverable work with a
  `contextId` gets a continuation prompt so the conversation resumes.

See the [recovery-continuation example](../example/recovery-continuation/) for
the mechanics.

### Scale-to-zero (idle-stopped agents)

An agent that no one is talking to shouldn't keep a process, a port, and a live
LLM subprocess pinned in RAM. Scale-to-zero (issue #6) reaps idle agents and
transparently wakes them again on demand:

- **`running ⇄ idle-stopped`.** When an agent has had **no turn in flight** for
  `runtime.agent_idle_timeout` (default **10m**), it self-exits with a distinct
  status code. The scheduler recognises this controlled exit and marks the agent
  **idle-stopped** — it is *not* treated as a crash and *not* restarted eagerly.
- **Activator wake.** The next request routed to an idle-stopped agent triggers
  the scheduler's **activator**: it relaunches the process on the same port,
  replays any recoverable in-flight work, and forwards the call. From the
  caller's side this is just a slightly slower first turn (cold start), not an
  error. A turn that arrives in the instant the reaper commits gets a retriable
  `agent idle-stopping` signal, which the activator resolves by waking + retrying.
- **Pinning resident.** Set `runtime.agent_idle_timeout: 0` to opt a hot agent
  out of reaping entirely — byte-for-byte the historical always-on behaviour.
  The default and any positive duration enable reaping.

**Idle-stopped is not archived/deleted.** These are three different things:

| State | What it means | Comes back by |
| --- | --- | --- |
| **idle-stopped** | live agent, temporarily not running to save resources | any request (activator wakes it automatically) |
| **archived** | intentionally stopped; kept in the registry but off | an explicit start |
| **deleted** | removed from the registry | re-registering / scaffolding it again |

Only idle-stopped is automatic and self-healing; archived/deleted are deliberate
operator actions.

**Two idle knobs, two granularities.** `agent_idle_timeout` reaps the whole
process; `session_idle_ttl` only recycles a single conversation's live
subprocess. They are independent and both default sensibly:

| Knob | Block | Granularity | On idle |
| --- | --- | --- | --- |
| `session_idle_ttl` | `pool` | one **session** (a `contextId` → one live subprocess) | that session closes → EVICTED (sessionId retained so it can `--resume`); the **agent process keeps running** |
| `agent_idle_timeout` | `runtime` | the whole **agent process** | the process self-exits → idle-stopped → woken on next access |

> **Renamed in a breaking change (issue #11).** These were formerly
> `pool.idle_ttl` and `runtime.idle_timeout`. The near-synonym names were too
> easy to confuse for two different granularities. A card still carrying an old
> key is **rejected at load** with an error naming the replacement — it is never
> silently ignored (a dropped `idle_timeout: 0` would have quietly turned a
> resident agent into a 10m-reaped one).

---

## 9. Authentication

An auto-generated **admin token** (a `0600` file beside `ahsir.yaml`, or
`AHSIR_ADMIN_TOKEN`) gates the control plane: agent start/stop and registry
writes (`POST /admin/agents`, `DELETE /admin/agents/{name}`, registry
mutations). Read/chat/trace/history/rooms are **not** gated. For the normal
same-user case the CLI and console discover the token automatically; a `401`
means the token isn't readable (cross-user setup or a different config dir).

---

## 10. Providers

Each agent picks its own model backend through the **`runtime`** block of its
`agent-card.yaml`. Two CLIs back it: **`claude`** (Anthropic + any
Anthropic-compatible endpoint such as DeepSeek/Zhipu) and **`codex`** (runs in an
isolated `CODEX_HOME` per agent).

The high-level fields map to env the CLI understands (see
`internal/wrapper/runtime.go`):

| Field | Meaning | Maps to (anthropic family) |
| --- | --- | --- |
| `provider` | `anthropic` \| `zhipu` \| `deepseek` \| `codex` (default `anthropic`) | picks the env wiring + default base URL |
| `baseURL` | endpoint override | `ANTHROPIC_BASE_URL` |
| `apiKey` | key/token (literal or `${ENV_VAR}`) | `ANTHROPIC_AUTH_TOKEN` |
| `model` | model id | `ANTHROPIC_MODEL` |

**Key rule:** the wrapper only injects the env vars you actually set. If
`baseURL` **and** `apiKey` are both empty, *no* `ANTHROPIC_*` env is exported and
the `claude` CLI falls back to **its own stored login**. `${VAR}` references are
expanded strictly (a missing var fails at agent startup, not at first call). A
non-empty `baseURL` with an empty `apiKey` is rejected (it would 401 silently).

### Recipes

**A. Claude Code local login (no API key).** Reuse the subscription/OAuth login
already stored by `claude` (`claude` then `/login`). Leave `baseURL`/`apiKey`
empty; leave `model` empty to use Claude Code's default, or pin one.

```yaml
runtime:
  command: claude
  provider: anthropic
  baseURL: ""        # empty → no ANTHROPIC_BASE_URL
  apiKey: ""         # empty → no token → claude uses its own login
  model: ""          # empty → Claude Code default; or e.g. claude-opus-4-8
  timeout: 600s
```

Requirements: the scheduler runs as the **same user / `$HOME`** that holds the
`~/.claude` login, and **no `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`/
`ANTHROPIC_BASE_URL` is exported** in the env that launches `ahsir start` (those
would override the login and bill the API instead).

**B. Anthropic API key.**

```yaml
runtime:
  command: claude
  provider: anthropic
  apiKey: ${ANTHROPIC_API_KEY}
  model: claude-sonnet-4-6      # optional
```

**C. DeepSeek / Zhipu (Anthropic-compatible).** `baseURL` defaults per provider,
so only `apiKey` (and usually `model`) are needed.

```yaml
runtime:
  command: claude
  provider: deepseek            # or: zhipu
  apiKey: ${MODEL_API_KEY}
  model: deepseek-v4-pro
```

**D. Codex.** Uses an isolated per-agent `CODEX_HOME` and Codex's own
config/login; `baseURL` is not supported here.

```yaml
runtime:
  command: codex
  provider: codex
  model: ""                     # optional; Codex picks its default otherwise
```

`ahsir agent new` scaffolds Anthropic defaults — recipe **B** (`provider: anthropic`,
no `baseURL` → official `api.anthropic.com`, `apiKey: ${ANTHROPIC_AUTH_TOKEN}`,
`model: claude-opus-4-8`). For recipe **A** (local login) pass `--api-key-env ""`
(and optionally `--model ""`); for **C/D** pass `--provider deepseek|zhipu|codex`.
After editing a card, **restart that agent** (console「重启生效」or
`POST /admin/agents/{name}/restart`) to apply.
