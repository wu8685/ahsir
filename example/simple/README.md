# Example: Simple Single-Agent Q&A

The minimum possible AHSIR setup — one agent (a teacher), one scheduler-owned
A2A request, one LLM round trip, one answer.

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
curl -s -X POST http://127.0.0.1:9800/a2a/teacher \
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

## Streaming mode (token-level deltas)

The teacher's `agent-card.yaml` enables `streaming.partial_messages: true`, so the agent is willing to stream token-level deltas back if the client asks for it. To trigger it, use the **`message/stream`** JSON-RPC method instead of `message/send`, and add `Accept: text/event-stream`:

```bash
curl -N -X POST http://127.0.0.1:9800/a2a/teacher \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/stream",
    "params": {
      "message": {
        "messageId": "msg-stream-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "Briefly explain TCP three-way handshake in two sentences."}]
      }
    },
    "id": 1
  }'
```

`-N` (`--no-buffer`) is critical — without it curl waits for EOF and you'd see nothing until the response is fully done.

You'll see something like:

```
id: 9a1f...
data: {"jsonrpc":"2.0","id":1,"result":{"kind":"status-update","status":{"state":"working"},...}}

id: 9a20...
data: {"jsonrpc":"2.0","id":1,"result":{"kind":"status-update","status":{"state":"working","message":{"parts":[{"kind":"text","text":"A TCP"}]}},...}}

id: 9a21...
data: {"jsonrpc":"2.0","id":1,"result":{"kind":"status-update","status":{"state":"working","message":{"parts":[{"kind":"text","text":" three-way"}]}},...}}

...

id: 9a99...
data: {"jsonrpc":"2.0","id":1,"result":{"kind":"task","status":{"state":"completed"},"history":[...]}}
```

The format:

- **`kind: status-update`** — one frame per token-level delta. The `status.message.parts[].text` field carries the increment.
- **`kind: task`** — the terminal frame. `status.state: completed` marks the end of the response and the `history` field holds the full conversation (same shape as a `message/send` response).

### Typewriter effect — extracting delta text live

The raw SSE frames above are JSON-shaped — readable but not pretty. To get a "typewriter" effect where only the answer text streams out word by word, pipe the curl output through `awk` to strip the `data:` prefix from each SSE line, then `jq` to pull out the delta text, then `tr` to suppress newlines so the chunks concatenate naturally:

```bash
curl -sN -X POST http://127.0.0.1:9800/a2a/teacher \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/stream",
    "params": {
      "message": {
        "messageId": "msg-stream-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "Briefly explain TCP three-way handshake in two sentences."}]
      }
    },
    "id": 1
  }' \
  | awk '/^data:/ { sub(/^data: ?/, ""); print; fflush() }' \
  | jq -r --unbuffered '.result.status.message.parts[]?.text // empty' \
  | tr -d "\n"
echo  # final newline once the stream ends
```

What each stage does:

- `curl -sN` — `-s` quiets curl's progress bar, `-N` (`--no-buffer`) disables curl's own output buffering so each SSE frame appears on stdout the instant it arrives.
- `awk '/^data:/ {...; fflush()}'` — drops everything that's not an `data:` line (the SSE id lines and blank separators), strips the `data: ` prefix so what's left is raw JSON, and `fflush()` after every print forces awk to push immediately (without it awk would block-buffer to 8 KB and you'd lose the live feel).
- `jq -r --unbuffered '.result.status.message.parts[]?.text // empty'` — for each JSON envelope, pulls out the delta text under `status.message.parts[].text`. The terminal `kind: task` frame has no `status.message`, so `?` short-circuits and `// empty` swallows it. `--unbuffered` is jq's flush-per-record switch.
- `tr -d "\n"` — token deltas already have their own spacing baked in; stripping the per-frame newlines lets adjacent chunks fuse into a continuous sentence.

You should see the answer print left-to-right, a few words at a time, until it stops.

A pure-awk version (no `jq` required) — slightly more brittle because awk doesn't do real JSON parsing, but useful if you don't have jq handy:

```bash
curl -sN -X POST http://127.0.0.1:9800/a2a/teacher \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -d '{"jsonrpc":"2.0","method":"message/stream","params":{"message":{"messageId":"m","role":"user","parts":[{"kind":"text","text":"Briefly explain TCP three-way handshake in two sentences."}]}},"id":1}' \
  | awk '
    /"kind":"status-update"/ {
      # Match the first text payload inside a parts array — the leading
      # parts pattern guards against accidentally picking up tool_use or
      # metadata text fields if the SDK ever adds them.
      if (match($0, /"parts":\[\{"kind":"text","text":"[^"]*"/)) {
        chunk = substr($0, RSTART, RLENGTH)
        sub(/.*"text":"/, "", chunk)
        sub(/"$/, "", chunk)
        printf "%s", chunk
        fflush()
      }
    }'
echo
```

Both produce the same live-streaming output. Use the jq version when you have it — its JSON parsing is correct under arbitrary escaping; the awk-only version assumes the SDK never emits a double-quote inside a delta (true today, but fragile).

### Telling stream from non-stream in the scheduler log

Every inbound A2A message logs a `receive:` line; the discriminator is `mode=send` vs `mode=stream`. A streaming turn also emits a `stream done` summary at the end so you can see how many deltas the agent produced and how long it took.

Example from a real run:

```
[teacher] receive: contextID= msgID=...           mode=send   text="Say hi in 5 words."
[teacher] receive: contextID= msgID=cli-stream-X  mode=stream text="Count to three slowly."
[teacher] stream done contextID= msgID=cli-stream-X deltas=8 bytes=20 took=13.6s
```

Useful greps:

```bash
grep "mode=stream" scheduler.log   # every streaming turn
grep "stream done" scheduler.log   # per-turn delta/byte/time summary
```

### Disabling streaming

If you want to disable streaming for an agent, set `streaming.partial_messages: false` (or just remove the block — `false` is the default). The agent will still accept `message/stream` requests, but only emits the initial `working` status update and the final `task` frame — no token-level increments.

## Stop

`Ctrl+C` in the scheduler terminal. The scheduler kills the agent subprocess.
