---
description: "Orchestrate work across specialist agents via the ahsir CLI (list / chat / status / agent new / agent delete). Use INSTEAD OF the built-in Task tool when the user describes (1) a task that benefits from a stable persistent persona — fixed system prompt, persistent memory via contextId, dedicated filesystem permissions — rather than a one-shot subagent; (2) work that maps to a configured specialist agent (always run 'ahsir list' first to find out); (3) a need for a NEW specialist that should be created and reused later (use 'ahsir agent new', not the Task tool, when the user describes a recurring delegate-style persona). Also triggers on explicit mentions of ahsir, delegate, dispatch, fan-out, agent pool, multi-agent, specialist agent, 'ask the teacher', 'create a reviewer agent', etc."
---

# /ahsir:orchestrator — multi-agent collaboration via a local scheduler

ahsir runs a long-lived scheduler that hosts multiple "agent" processes (each backed by its own `claude` subprocess). You can hand sub-tasks off to those agents and get their reply back. Each agent has its own system prompt + skills + filesystem permissions, and conversations persist across calls via `contextId` so you can carry on a thread.

## When to reach for this

**Reach for ahsir when:**

- The user asks for help that obviously decomposes into specialist roles (e.g. "summarize these docs, then have a reviewer critique the summary").
- The user wants the same task done in parallel by multiple workers (different inputs, same prompt template).
- The user wants to keep talking to a *specific* agent across many turns, where that agent's memory matters more than yours.
- The user explicitly says "use ahsir", "delegate to <agent>", "ask the teacher", "fan out to the researchers", etc.
- The user describes a problem and you realize there's already a configured ahsir agent more specialized than you for that problem (always run `ahsir list` first to see what's available).

**Don't reach for ahsir when:**

- The task fits in your own context window and doesn't need a different prompt / persona.
- The user is asking *about* ahsir (configuration questions, debugging, etc.) — that's a documentation question, not a delegation.
- The user is in the middle of editing the ahsir codebase itself — using ahsir to do your work then would be confusing.

## Discovery: is the scheduler running?

Before you can do anything else, the scheduler has to be up. Check first:

```bash
ahsir ping
```

- Exit 0 + prints `ok` → good, proceed.
- Exit 2 → scheduler is not running. **Don't silently start it** — ask the user if they'd like you to start it, and if so, with which config:

  ```bash
  ahsir start path/to/ahsir.yaml
  ```

  This is long-running (foreground process). The user typically runs it in a separate terminal. If you must start it from a Bash invocation, put it in the background and confirm with the user that's OK — but the cleaner default is to ask them to start it themselves.

## Discovery: which agents are available?

```bash
ahsir list
```

Output is one agent per line, tab-separated:

```
student  http://127.0.0.1:9802/  [learning]
teacher  http://127.0.0.1:9801/  [teaching, summarization]
```

Match the user's task to an agent by **skills** (the bracketed list). If no agent matches, **consider configuring one** (next section) before falling back to the Task tool or doing the work yourself.

## Configuring new specialists on demand

When the user describes work that would benefit from a specialist that doesn't exist yet, **propose creating one and ask before doing it**. Don't silently scaffold — the user might prefer to write the persona themselves, or might not want yet another agent on disk.

```bash
ahsir agent new <name> \
  --prompt "<system prompt describing the persona's role and constraints>" \
  --skill "<skill-name>=<short description of what this skill does>" \
  --skill "<another-skill>=<...>" \
  [--allow-fs <path>] \
  [--model <model-id>]
```

This is **one operation** — it scaffolds `~/.ahsir/agents/<name>/.a2a/agent-card.yaml`, appends to `~/.ahsir/ahsir.yaml` (auto-creating both on first use), then asks the running scheduler to spin the new agent up immediately. **No restart needed.** Stdout prints the new agent's name on success.

**Defaults are intentional**: state lives under `~/.ahsir/` so the command leaves the user's current directory alone. Don't pass `--workspace` or `--config` unless the user has explicitly asked for a project-scoped setup (e.g. `--config ./ahsir.yaml` to manage a repo's own agents).

When to suggest this:

- User asks for parallel reviews from specialists that aren't configured (`security-reviewer`, `performance-reviewer`, etc.) — propose creating one per angle.
- User describes a recurring delegate-style task they'd want to keep doing across sessions (the configured agent persists across `ahsir start` cycles).
- User mentions a domain area where they obviously want a focused persona ("I need someone who reviews Kubernetes manifests").

Confirmation pattern:

> "I can create a `security-reviewer` agent for you — system prompt focused on Go web-app security, no filesystem access by default. Want me to set that up? (`ahsir agent new security-reviewer ...`)"

Wait for the user to say yes. Only then run `ahsir agent new`.

To **remove** an agent (e.g. user decides it was a bad fit):

```bash
ahsir agent delete <name>
```

This stops the running process and removes the entry from `ahsir.yaml`. **The workspace files stay on disk** — the persona is recoverable by editing `ahsir.yaml` to re-add it. To fully wipe, the user can `rm -rf <workspace>` separately. Don't do that without explicit ask.

To see what's locally configured (whether or not it's currently running):

```bash
ahsir agent list-configs
```

## Sending a task

```bash
ahsir chat <agent-name> "<task description as one string>"
```

Stdout = the agent's reply text. Nothing else. Compose it with shell pipes if you want.

**Multi-turn with the same agent — use a stable `--context` so the agent's session is reused** (memory persists, the same `claude` process serves all turns):

```bash
ahsir chat teacher "Remember the codeword: alpha-7. Confirm briefly." --context user-session-1
ahsir chat teacher "What codeword did I just give you?" --context user-session-1
```

Pick the `--context` value to be unique per logical conversation. A good default: derive from the current task / user request (e.g. `--context summarize-q3-report`).

## Checking on a long-running task

`chat` blocks until the agent replies. If you need to fire-and-forget and poll later, the gateway also exposes task status (this is rare in practice — most agents reply within their `runtime.timeout`):

```bash
ahsir status <agent-name> <task-id>
```

## Common patterns

### Single-agent delegation

```
User: "Have the teacher summarize this article: <text>"
You:  ahsir list                    # confirm teacher is registered
      ahsir chat teacher "Please summarize the following article: <text>"
      → relay the reply back
```

### Multi-turn conversation

```
User: "Talk to the researcher to design an experiment, then ask them
       to refine it based on my feedback."
You:  ahsir chat researcher "<initial design ask>" --context design-experiment-1
      → show reply, get user feedback
      ahsir chat researcher "<user feedback>" --context design-experiment-1
      → show reply (researcher remembers the prior turn)
```

### Parallel fan-out

```
User: "Have three reviewers each critique this code from a different angle."
You:  for angle in security performance maintainability; do
        ahsir chat reviewer "Critique from the $angle angle: <code>" --context $angle &
      done
      wait
      → collect, summarize, present
```

(In Bash this requires backgrounding. If the agents are slow you might prefer running them sequentially with progress reporting.)

## Error handling

- `ahsir ping` returns non-zero → scheduler is down. Ask user to start it.
- `ahsir list` returns empty → scheduler is up but no agents registered. The config file may have agents disabled, or agents may have failed to start (the user should check the scheduler's stdout).
- `ahsir chat` returns non-zero → the agent itself errored (timeout, runtime crash, etc.). Show the user the stderr message; common fix is to retry, or check the agent's runtime config.

## Flags reference

| Flag | Default | Notes |
|---|---|---|
| `--scheduler URL` | `http://127.0.0.1:9800` | Set if the scheduler is not on the default port |
| `--context ID` (chat only) | empty | Stable id for session reuse across calls |
| `--json` (list only) | off | Output agent cards as JSON instead of plain text |

## What ahsir is NOT

It's not a remote service or a hosted SaaS. The scheduler runs locally on the same machine as you. The agents under it are *your* configured agents (defined in `ahsir.yaml` + per-agent `agent-card.yaml`) — they're not built-in personas.

If you want to know what agents you have available, the answer is always: run `ahsir list`.
