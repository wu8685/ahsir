# Example: Session Reuse via `contextId`

Demonstrates AHSIR's three layers of conversation continuity:

| Layer | Trigger | Mechanism |
|---|---|---|
| **In-process reuse** | Two curls with same `contextId`, same scheduler | `SessionPool` returns the cached live `claude` process |
| **Cross-restart resume** | Restart scheduler, curl again with same `contextId` | `sessions.json` persists `contextId → sessionId`; new claude starts with `--resume=<id>` |
| **Self-healing on SIGKILL** | `kill -9` the claude process, curl again | `IsHealthy()` probe detects the zombie, pool transparently recreates with `--resume` |

All three keep the teacher's memory of prior turns intact.

## Layout

```
session-reuse/
├── ahsir.yaml                                   # scheduler + registry config
├── workspaces/
│   └── teacher/
│       └── .a2a/
│           ├── agent-card.yaml                  # teacher's agent card
│           └── sessions.json                    # auto-created at first curl
└── README.md
```

`sessions.json` is created automatically by the agent on first request. It maps `contextId` → `sessionId` so a restart can resume.

## Prerequisites

Same as `../simple/`: `bin/ahsir` + `bin/ahsir-agent` built, `claude` CLI on PATH, `MODEL_API_KEY` exported.

## Start the scheduler

From the **repo root**:

```bash
./bin/ahsir start example/session-reuse/ahsir.yaml
```

## Layer 1 — In-process reuse

Two curls back-to-back with the same `contextId`. The teacher recalls the codeword from turn 1 in turn 2.

```bash
# Turn 1 — give the teacher something to remember
curl -s -X POST http://127.0.0.1:9801/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-1",
        "contextId": "conv-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "Please remember this codeword: jade-tiger-99. Reply briefly to confirm."}]
      }
    },
    "id": 1
  }'

# Turn 2 — same contextId, ask the teacher to recall
curl -s -X POST http://127.0.0.1:9801/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-2",
        "contextId": "conv-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "What codeword did I just tell you? Reply with just the codeword."}]
      }
    },
    "id": 2
  }'
```

**Expected scheduler log** — note **only one** `claude session: started` line:

```
[teacher] receive: contextID=conv-1 msgID=msg-1 text="Please remember this codeword..."
claude session: started pid=12345 cmd=claude args=[...]               ← spawned once
[teacher] receive: contextID=conv-1 msgID=msg-2 text="What codeword did I just tell you..."
                                                                       ← no second "started"
```

That missing second `claude session: started` is the literal proof of in-process reuse — the pool's hot path returned the cached `claude` process to turn 2.

**Inspect the persistence file:**

```bash
cat example/session-reuse/workspaces/teacher/.a2a/sessions.json
```

```json
{
  "version": 1,
  "entries": {
    "conv-1": {
      "sessionId": "<real-uuid>",
      "state": "active",
      "lastUsed": "2026-06-05T..."
    }
  }
}
```

## Layer 2 — Cross-restart resume

Stop and restart the scheduler. The next curl on the same `contextId` resumes the conversation from the persisted `sessionId`.

```bash
# In the scheduler terminal: Ctrl+C
# Then start again:
./bin/ahsir start example/session-reuse/ahsir.yaml

# Same contextId, ask again:
curl -s -X POST http://127.0.0.1:9801/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-3",
        "contextId": "conv-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "Reminder: what was that codeword again?"}]
      }
    },
    "id": 3
  }'
```

**Expected log** — note the `--resume=<id>` argument:

```
[teacher] receive: contextID=conv-1 msgID=msg-3 text="Reminder: what was that codeword again?"
claude session: started pid=... cmd=claude args=[... --resume=<sessionId-from-sessions.json>]
```

The teacher should still answer `jade-tiger-99` — claude's local jsonl store under `~/.claude/projects/.../<sessionId>.jsonl` holds the prior conversation.

## Layer 3 — Self-healing across SIGKILL

The pool detects when the underlying claude is dead and recreates it with `--resume` transparently. Useful for surviving operator mishaps or runtime crashes.

```bash
# After Layer 1, find the teacher's claude pid from the scheduler log:
TEACHER_PID=<the-pid-from-the-log>

# Kill it externally
kill -9 $TEACHER_PID

# Curl again with the same contextId
curl -s -X POST http://127.0.0.1:9801/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-4",
        "contextId": "conv-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "After all that, what codeword did I give you?"}]
      }
    },
    "id": 4
  }'
```

**Expected log** — a new `claude session: started` with `--resume=<id>`:

```
[teacher] receive: contextID=conv-1 msgID=msg-4 ...
claude session: started pid=<new-pid> cmd=claude args=[... --resume=<sessionId>]
```

The teacher still recalls `jade-tiger-99`. The pool noticed the prior session was unhealthy via `IsHealthy()`, closed the zombie, and let the existing EVICTED-recovery path do its job.

## Gotchas

- The JSON field is **`contextId`** (camelCase, lowercase `d`). `contextID` / `context_id` are silently ignored and you'll never see reuse.
- `messageId` must differ across requests — the A2A SDK may dedupe and replay the previous task otherwise.
- Each request specifies the **outer** `contextId`. The internal `sessionId` (the one in `sessions.json`) is owned by claude and rotates when claude wants — `contextId` is your stable handle.
- If you delete `~/.claude/projects/.../<sessionId>.jsonl` between turns, `--resume` will fail (claude has lost the history). The pool will still spawn a new process but the teacher won't remember the prior turns.

## Stop

`Ctrl+C` in the scheduler terminal.
