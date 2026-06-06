# Example: Restart Continuation and Session Retention

This example shows the recovery path that sits above ordinary session reuse:

1. scheduler records each mediated request in `.ahsir/ledger.jsonl`;
2. an `ahsir-agent` process is killed while a request is in flight;
3. scheduler restarts the same local agent on the same port;
4. scheduler scans the ledger and sends a continuation prompt with the original
   `contextId`;
5. the restarted agent resumes through `<workspace>/.a2a/sessions.json`.

It also shows how inactive session mappings are bounded by `pool.max_evicted`.

## Layout

```
recovery-continuation/
├── ahsir.yaml
├── workspaces/
│   └── teacher/
│       └── .a2a/
│           └── agent-card.yaml
└── README.md
```

The teacher card uses:

```yaml
runtime:
  timeout: 0s
pool:
  idle_ttl: 10s
  evicted_ttl: 30d
  max_evicted: 2
```

`timeout: 0s` avoids killing the provider turn by deadline during the demo.
`idle_ttl: 10s` and `max_evicted: 2` make retention behavior easy to observe.

## Prerequisites

Same as `../simple/`:

```bash
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent
export MODEL_API_KEY=<your-deepseek-api-key>
which claude
```

Run all commands from the repo root.

## Start

```bash
./bin/ahsir start example/recovery-continuation/ahsir.yaml
```

Watch for:

```
Agent teacher started on port 9801 (pid: ...)
Executor wired: deepseek SessionPool (... idle_ttl=10s, evicted_ttl=720h0m0s, max_evicted=2)
```

## Trigger An Interrupted Request

In a second terminal, start a long request with a stable `contextId`:

```bash
curl -s -X POST http://127.0.0.1:9800/a2a/teacher \
  -H 'Content-Type: application/json' \
  -d '{
    "jsonrpc": "2.0",
    "method": "message/send",
    "params": {
      "message": {
        "messageId": "msg-recovery-1",
        "contextId": "recovery-demo-1",
        "role": "user",
        "parts": [{"kind": "text", "text": "Write a detailed 8 section study plan for learning Go concurrency. Take your time and make each section concrete."}]
      }
    },
    "id": 1
  }'
```

While that request is still running, kill the `ahsir-agent` process, not the
scheduler:

```bash
pgrep -fl 'ahsir-agent.*recovery-continuation'
kill -9 <ahsir-agent-pid>
```

The curl will usually fail because the upstream agent disappeared mid-request.
That is expected; the scheduler has already recorded the invocation.

## Observe Recovery

In the scheduler terminal, look for this sequence:

```
Agent teacher exited: signal: killed
Agent teacher scheduling restart attempt=1 delay=...
Agent teacher restarted attempt=1
Agent teacher recovery: dispatch invocation=... contextID=recovery-demo-1 status=failed
claude session: started pid=... cmd=claude args=[... --resume=<sessionId>]
Agent teacher recovery: recovered invocation=... contextID=recovery-demo-1
```

The important parts:

- `recovery: dispatch` means the scheduler found a recoverable ledger record.
- `contextID=recovery-demo-1` means the continuation prompt is sent to the same
  A2A context.
- `--resume=<sessionId>` means the restarted wrapper found
  `contextId -> sessionId` in `sessions.json` and resumed the provider session.
- `recovered` means the continuation prompt returned successfully.

If the agent is not ready quickly enough, you may see `recovery_failed`. That
state is persisted in the ledger and can be retried after a later supervised
restart.

## Inspect The Ledger

The scheduler ledger lives beside this example config:

```bash
cat example/recovery-continuation/.ahsir/ledger.jsonl
```

You should see events such as:

```json
{"type":"started","id":"inv-1","source":"a2a_proxy","agentName":"teacher","contextId":"recovery-demo-1"}
{"type":"failed","id":"inv-1","error":"proxy teacher: ..."}
{"type":"recovering","id":"inv-1"}
{"type":"recovered","id":"inv-1"}
```

On scheduler startup, completed records older than 7 days are compacted away.
Non-completed records are retained for 30 days.

## Observe Session Mapping Retention

The agent session mapping lives here:

```bash
cat example/recovery-continuation/workspaces/teacher/.a2a/sessions.json
```

This example keeps at most two inactive mappings:

```yaml
pool:
  idle_ttl: 10s
  max_evicted: 2
```

To see the oldest inactive mapping disappear, send three short requests with
three different `contextId` values, waiting more than 10 seconds between each
request so the previous active session becomes inactive:

```bash
for c in keep-a keep-b keep-c; do
  curl -s -X POST http://127.0.0.1:9800/a2a/teacher \
    -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"method\":\"message/send\",\"params\":{\"message\":{\"messageId\":\"msg-$c\",\"contextId\":\"$c\",\"role\":\"user\",\"parts\":[{\"kind\":\"text\",\"text\":\"Reply with the context id $c.\"}]}},\"id\":1}"
  sleep 12
done

cat example/recovery-continuation/workspaces/teacher/.a2a/sessions.json
```

The mapping for `keep-a` should be gone after the reaper runs, while the two
newer inactive mappings remain. Active mappings are not deleted by
`pool.max_evicted`.

## Stop

`Ctrl+C` in the scheduler terminal.
