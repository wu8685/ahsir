---
name: orchestrator
description: "Orchestrate work across specialist agents via the ahsir CLI (list / chat / status / agent new / agent delete). Use INSTEAD OF the built-in Task tool when the user describes (1) a task that benefits from a stable persistent persona — fixed system prompt, persistent memory via contextId, dedicated filesystem permissions — rather than a one-shot subagent; (2) work that maps to a configured specialist agent (always run 'ahsir list' first to find out); (3) a need for a NEW specialist that should be created and reused later (use 'ahsir agent new', not the Task tool, when the user describes a recurring delegate-style persona). Also triggers on explicit mentions of ahsir, delegate, dispatch, fan-out, agent pool, multi-agent, specialist agent, 'ask the teacher', 'create a reviewer agent', etc."
---

# /ahsir:orchestrator — multi-agent collaboration via a local scheduler

ahsir runs a long-lived **local** scheduler hosting multiple "agent" processes (each backed by its configured provider, e.g. Claude or Codex). Hand sub-tasks off and get the reply back. Each agent has its own system prompt + skills + filesystem permissions; conversations persist across calls via `contextId`. Agents are *your* configured personas (`ahsir.yaml` + per-agent `agent-card.yaml`), not built-in — to see what exists, always `ahsir list`.

## When to reach for this

**Use ahsir when:** the task decomposes into specialist roles ("summarize, then have a reviewer critique it"); the same task runs in parallel over different inputs; the user wants to keep talking to a *specific* agent whose memory matters more than yours; the user says "use ahsir / delegate to X / ask the teacher / fan out"; or a configured agent is more specialized than you (run `ahsir list` to check).

**Don't when:** the task fits your own context with no different persona; the user is asking *about* ahsir (that's a docs/debug question, not delegation); or the user is editing the ahsir codebase itself.

## Discovery

```bash
ahsir ping            # exit 0 + "ok" → up. exit 2 → down.
ahsir list            # one agent/line, tab-separated: name  url  [skills]
ahsir doctor          # config, provider CLIs on PATH, reachability, per-agent status, admin-token discoverability (--config for a non-default config)
```

- If `ping` is down, **don't silently start it** — ask the user, and with which config. `ahsir start path/to/ahsir.yaml` is a long-running foreground process (they usually run it in a separate terminal); only background it from Bash with their OK.
- Match the user's task to an agent by its bracketed **skills**. No match → consider configuring one (below) before falling back to a one-shot subagent or doing it yourself.

## Configuring new specialists on demand

When work needs a specialist that doesn't exist, **propose it and wait for a yes** — don't silently scaffold (the user may want to write the persona, or not want another agent on disk).

```bash
ahsir agent new <name> \
  --prompt "<system prompt: role + constraints>" \
  --skill "<skill-name>=<short description>" [--skill ...] \
  [--allow-fs <path>] [--model <model-id>]
```

- **This is one operation:** scaffolds `~/.ahsir/agents/<name>/.a2a/agent-card.yaml`, appends to `~/.ahsir/ahsir.yaml` (both auto-created on first use), and hot-registers the agent on the running scheduler — **no scheduler restart**, usable in `list`/`chat` immediately (stdout prints the name).
- **Filesystem is read-only by default.** `--allow-fs <path>` grants *read* under that path (whitelist `Read,LS,Glob,Grep`). To write/edit (a builder persona), set `filesystem.write_access: true` in the card (adds `Edit,MultiEdit,Write`) — `agent new` has no flag for it yet, so edit the card then `ahsir agent restart <name>`.
- **Long turns need a higher `runtime.timeout`.** Per-LLM-call default is `300s` (`--timeout`); a big build fails with `claude turn timed out after 5m0s` (partial edits may already be on disk). Raise it (e.g. `900s`) — but it's capped by the registry's `timeouts.chat` (default `10m`), so go beyond that only by raising both.
- **State lives under `~/.ahsir/`** (leaves the cwd alone). Don't pass `--workspace`/`--config` unless the user wants a project-scoped setup (e.g. `--config ./ahsir.yaml`).

**Never tell the user to restart the *scheduler* just to add/change an agent.** A restart or extra step is only relevant when: you passed `--skip-start` (scaffold only; starts on next `ahsir start`); the scheduler was **down** at `agent new` (CLI writes the files; `ahsir start` loads them); or the user **hand-edited `ahsir.yaml`** (the running scheduler doesn't watch it). Editing an *existing* card (write_access, runtime.timeout, apiKey, …) needs restarting **that agent only** — `ahsir agent restart <name>` (CLI counterpart to `POST /admin/agents/{name}/restart`; the console「重启生效」is the same). It re-reads the card and reports the new port.

`agent new` / `agent delete` / `agent restart` hit the **control plane**, gated by an admin token. Same-user is transparent (CLI auto-discovers a `0600` file beside `ahsir.yaml`, or `AHSIR_ADMIN_TOKEN`). `chat`/`list`/`trace`/`history` are never gated. On `401 admin token required`, the CLI can't read the token (cross-user setup, or a different config dir than you passed) — surface it; the user can pass `--config` at the scheduler's `ahsir.yaml` or set `AHSIR_ADMIN_TOKEN`.

```bash
ahsir agent delete <name>     # stops the process + removes the ahsir.yaml entry; WORKSPACE FILES STAY
ahsir agent list-configs      # everything configured locally, running or not
```

Recover a deleted persona by re-adding it to `ahsir.yaml`; a full wipe is `rm -rf <workspace>` separately — never without an explicit ask.

**When to propose one:** parallel reviews from specialists that aren't configured (one per angle); a recurring delegate-style task the user wants across sessions (configured agents persist across `ahsir start` cycles); a domain wanting a focused persona. Pattern: *"I can create a `security-reviewer` (Go web-app security focus, no FS access). Set it up? (`ahsir agent new security-reviewer ...`)"* — then wait for yes.

## Sending a task

```bash
ahsir chat <agent> "<task as one string>"      # stdout = the reply text, nothing else
```

- **Multi-turn:** pass a stable `--context <id>` to reuse the session (memory persists; the same `claude` process serves every turn). Make the id unique per logical conversation (e.g. derive from the task: `--context summarize-q3-report`).
- **Shared contexts:** when several parties talk into the *same* `contextId`, tag each turn with `--as <name>` so the agent attributes statements correctly (`--as` defaults to the OS username, so single-party needs nothing). Concurrent turns on one `contextId` are **serialized in a FIFO queue** (they wait, not error) up to a bounded depth; only an overfull queue returns busy/`409`. Distinct `contextId`s run fully in parallel.

```bash
ahsir chat teacher "Codeword: alpha-7. Confirm." --context s1
ahsir chat teacher "What codeword did I give?"   --context s1
ahsir chat planner "I prefer option B" --context roadmap --as alice
```

## Long-running tasks & inspection

`chat` blocks (submits + polls internally), so usually you just read stdout. For genuinely long work, fire-and-forget:

```bash
ahsir chat <agent> "<long task>" --context <ctx> --async   # prints a task id, returns immediately
ahsir status <agent> <task-id>                             # poll until terminal
```

Task ids are **in-memory only** — after an agent restart a task id 404s, but the conversation survives (use `history` to check whether the turn ran).

```bash
ahsir history <agent> <contextId>   # full transcript replay — the TAKEOVER path: replay before joining a conversation someone else started
ahsir trace [contextId]             # invocation timeline: source (+speaker), agent, status, duration — "where did time go / what got called"
```

Both are read-only and need no token.

## Patterns

- **Single delegation:** `ahsir list` (confirm the agent) → `ahsir chat teacher "Summarize: <text>"` → relay the reply.
- **Multi-turn:** reuse one `--context` across turns; the agent remembers prior turns.
- **Parallel fan-out** (Bash needs backgrounding + `wait`):

  ```bash
  for angle in security performance maintainability; do
    ahsir chat reviewer "Critique from the $angle angle: <code>" --context $angle &
  done; wait
  ```

  **Heavy concurrent turns can hit transient socket errors** — firing several expensive turns at once (image-reading reviews, or fan-out *while* a builder turn is mid-flight) may surface `API Error: The socket connection was closed unexpectedly`. It's transient: **retry the failed turn once** (a fresh `--context` avoids a half-written session), and prefer running heavy/image turns **sequentially**. The limiter is provider/process pressure, not ahsir.

## Multi-agent rooms (group chat / roundtable)

Beyond 1:1 delegation, **rooms** let several agents share one conversation. Two modes:

- **多 Agent 协同 (`relay`)** — hub-and-spoke, `@`-mention turn-taking.
- **圆桌 (`roundtable`)** — consensus rounds: each round is a full random-order pass (speak or reply `同意`); a dedicated **moderator** agent consolidates 【已达成】/【待议】/【纠偏】 per round and drives toward consensus.

Rooms are driven by the **web console** (`ahsir ui`) or the gateway HTTP API (`POST /rooms` with `mode`, `GET /rooms`, `POST /rooms/{id}/say`, `/stop`) — there is **no `ahsir room` CLI verb yet** (in the TODO). For debating/converging on a hard decision, point the user at the console's 圆桌 flow rather than serial `ahsir chat`.

## Error handling (quick reference)

- `ping` non-zero → scheduler down; ask the user to start it.
- `list` empty → scheduler up but no agents registered: disabled in config, or they failed to start (check scheduler stdout — and see Gotchas).
- `chat` non-zero → the agent itself errored (timeout, crash); show stderr, retry, or check its runtime config.
- `agent new/delete` `401` → admin token unreadable (see control-plane note above).
- In doubt → `ahsir doctor`.

## Gotchas (agents that register but won't run)

Symptom when standing up a fresh team: `agent new` reports "running on port N", yet `ahsir list` is **empty** (a crash-looping agent shows in neither `list` nor the registry) / `chat` 404s / the scheduler log shows it restarting.

**First, get the real boot error — don't guess from "exit status N".** The scheduler redirects each agent child's `cmd.Stderr` to its own stdout, so the failure is in the scheduler log, not the ambiguous `exited: exit status 1|2` line. Grep it:

```bash
grep -iE 'references unset env vars|flag provided but not defined|Usage of .*ahsir-agent|health failed|connection refused' scheduler.log
```

1. **`apiKey: ${ANTHROPIC_AUTH_TOKEN}` in a card breaks local-login (`claude /login` / OAuth).** Every card field runs through `expandStrict`, which **errors on an unset env-var reference** — so on an OAuth machine the agent crash-loops. Tell-tale (exit code 1 *or* 2): `runtime.apiKey references unset env vars: ANTHROPIC_AUTH_TOKEN`. **Current `agent new` handles this** for `--provider anthropic`: with the default `--api-key-env` and `ANTHROPIC_AUTH_TOKEN` unset, it writes `apiKey: ""` so the agent inherits the CLI's OAuth session. You only hit it on an **older binary** or a **hand-written/legacy card** — fix with `runtime.apiKey: ""` + `ahsir agent restart <name>`. Keep the `${...}` form only if that env var is genuinely set (e.g. a third-party gateway). NB: `exit status 2` with **no** "unset env vars" line is likely #6, not this.

2. **An agent fails `/healthz` until its MCP servers initialize — slow/failing MCP crash-loops it.** Health-check is ~3×10s = 30s before kill & restart; MCP that cold-downloads (`npx ...@latest`), needs interactive auth, or proxies a remote SSE endpoint can blow past that. Tell-tale: `Agent <name> health failed consecutive=3 threshold=3 ... dial tcp 127.0.0.1:<port>: connect: connection refused`. Keep the injected MCP set **minimal and fast-starting**; for heavy/remote sources, pre-fetch the data and hand the agent a file via `--allow-fs` instead of wiring the slow MCP server in.

3. **On a non-default port, pass `--scheduler` on EVERY call — a missing flag silently targets `:9800`.** With a per-project scheduler (`registry.port: 9770`), `ahsir chat <agent> "..."` *without* `--scheduler http://127.0.0.1:9770` hits `:9800` → `404 agent not found`, even though `ahsir list --scheduler :9770` shows it alive. Wrap it: `ah() { ahsir "$@" --scheduler "$URL"; }`. (`ahsir doctor --config X` likewise reports the *default* scheduler, not the `registry.port` inside `X`.)

4. **`agent new <name> --workspace <dir>` overwrites an existing card.** Re-running without `--prompt` blanks the persona (`systemPrompt: ""`). Always pass the full `--prompt` (+ `--mcp-config` / `--allow-fs`) when regenerating, and version the persona source separately (e.g. `personas/<name>.md`) so a regen is reproducible.

5. **A scheduler only supervises the agents in the config it was started with.** Adding an entry to a *different* `ahsir.yaml`, or to `~/.ahsir/ahsir.yaml` by hand, does nothing until a scheduler owns it. Isolated-team pattern: a dedicated `ahsir.yaml` (own `registry.port` + non-overlapping `port_range`) + `ahsir start <that-config>` + `ahsir ui --scheduler <that-url>` — fully isolated from and reversible without touching the global `:9800` scheduler.

6. **`ahsir` and `ahsir-agent` must be the SAME build — a mismatch crash-loops *every* agent at boot.** The scheduler spawns the `ahsir-agent` sitting **next to its own executable** (`agentBinary()` = `dirname(scheduler-exe)/ahsir-agent`). Launch the scheduler via a `PATH`/symlink that resolves to a *different* install (e.g. a bare `ahsir start` picking up a stale plugin-bundled binary) and a newer scheduler passes flags the older agent rejects. Tell-tale — `exit status 2` with **no** "unset env vars" line (easy to misread as #1): `flag provided but not defined: -admin-token` / `Usage of /.../some-old-path/ahsir-agent:`. Fix: start the scheduler from the **absolute path of the build you want** (`/abs/path/to/ahsir/bin/ahsir start ~/.ahsir/ahsir.yaml`). Verify the pair: `ls -la $(dirname $(which ahsir))/ahsir-agent` should match the `ahsir` you start.

## Performance debugging

When ahsir is slow/stuck or the user asks where time went, get the scheduler stdout around the relevant `contextID` (don't guess from final `ahsir chat` latency). Useful greps: `contextID=<id>`, `executor turn done`, `session pool: lookup`, `A2A_CALL`. Reading the waterfall:

- `[agent] send done ... took=` — full A2A handler time for one inbound request.
- `session pool: lookup ... outcome=hit|create|resume|capacity_reject ... took=` — session reuse vs process creation vs resume vs capacity limits.
- `executor open_session done ... took=` — the executor's view of pool/session acquisition.
- `executor prompt_ready ... agents=N ... prompt_bytes=... took=` — registry listing + prompt construction.
- `executor turn done ... took=... provider_duration_ms=... input_tokens=... output_tokens=` — the provider turn; wrapper `took` ≫ provider duration ⇒ CLI/process/stream overhead.
- `[X → Y] A2A_CALL` / `[X ← Y] reply ... took=` — a child-agent call (the child has its own `executor turn done` under the same `contextID`).
- `executor injection_ready ... took=` — usually tiny; if not, result payloads may be too large.

## Flags reference

| Flag | Applies to | Default | Notes |
|---|---|---|---|
| `--scheduler URL` | all | `http://127.0.0.1:9800` | Set if the scheduler isn't on the default port |
| `--context ID` | chat | empty | Stable id for session reuse across calls |
| `--as NAME` | chat | OS username | Speaker identity for shared contexts |
| `--async` | chat | off | Submit and return a task id instead of blocking |
| `--stream` | chat | off | Token-by-token output (needs the agent's `streaming.partial_messages`) |
| `--json` | list, trace, history | off | Structured output instead of plain text |
| `--config path` | doctor, agent new/delete | auto-detect | Locate the right `ahsir.yaml` (and its admin token) |
