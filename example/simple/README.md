# Example: Simple Single-Agent Q&A

The minimum possible AHSIR setup — one agent (a teacher) listening on A2A, one HTTP request, one LLM round trip, one answer.

Run this first to confirm your build, registry, agent card, runtime config, and DeepSeek API key are all healthy. Once it works, move on to `../session-reuse/` for the session continuity story.

## Layout

```
simple/
├── ahsir.yaml                                   # scheduler + registry config
├── workspaces/
│   └── teacher/
│       └── .a2a/
│           └── agent-card.yaml                  # teacher's agent card
└── README.md
```

## Prerequisites

- `bin/ahsir` and `bin/ahsir-agent` built (from repo root: `go build -o bin/ahsir ./cmd/ahsir && go build -o bin/ahsir-agent ./cmd/ahsir-agent`)
- `claude` CLI on PATH
- `MODEL_API_KEY` env var set to your DeepSeek key (`export MODEL_API_KEY=<your-key>`)

## Run

From the **repo root** (not from this directory — `workspace` paths in `ahsir.yaml` are relative to the scheduler's cwd):

```bash
./bin/ahsir start example/simple/ahsir.yaml
```

Watch for:

```
Agent teacher started on port 9801 (pid: ...)
Agent teacher listening on port 9801
Executor wired: stream-json SessionPool (... persist=example/simple/workspaces/teacher/.a2a/sessions.json)
```

## Ask the teacher (in another terminal)

```bash
curl -s -X POST http://127.0.0.1:9801/ \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "What is a goroutine? Answer in one sentence."}]
      }
    },
    "id": 1
  }'
```

You should see something like (response trimmed):

```json
{
  "result": {
    "history": [
      {"role": "user", "parts": [{"text": "What is a goroutine?..."}]},
      {"role": "agent", "parts": [{"text": "A goroutine is a lightweight thread..."}]}
    ],
    "status": {"state": "completed"}
  }
}
```

## What just happened

In the scheduler terminal you'll see one `claude session: started` line — that's the agent's `claude` subprocess being spawned to handle this single turn.

```
[teacher] receive: contextID= msgID=msg-1 text="What is a goroutine? ..."
claude session: started pid=... cmd=claude args=[... -p --input-format=stream-json ...]
```

Note `contextID=` is empty — your curl didn't pass one, so the A2A SDK auto-generated an internal id for this task. The pool created a fresh session entry keyed on that auto id. Next curl (even with identical text) gets a different auto id and a different session — each curl is its own isolated conversation.

That's the limitation of this example by design. To carry conversation memory between curls, see the next one.

## Stop

`Ctrl+C` in the scheduler terminal. The scheduler kills the agent subprocess.
