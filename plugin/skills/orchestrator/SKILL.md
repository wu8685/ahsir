---
description: Dispatch tasks to specialist agents running under the ahsir multi-agent scheduler via the CLI (ahsir list / chat / status / ping). Use when the user wants to delegate work to a named external agent, parallelize sub-tasks across independent claude processes, maintain conversation memory with a specific agent across many turns, or explicitly mentions ahsir / agent pool / multi-agent / fan-out / delegate to <agent>.
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

Match the user's task to an agent by **skills** (the bracketed list). If no agent matches, tell the user and either suggest configuring one or just handle the task yourself.

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
