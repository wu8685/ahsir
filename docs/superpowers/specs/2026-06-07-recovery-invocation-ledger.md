# AHSIR: Scheduler Recovery Invocation Ledger

**Status:** Implemented with JSONL persistence and recovery lifecycle states
**Date:** 2026-06-07
**Version:** 0.1.0

## 1. Motivation

Now that local A2A traffic is routed through the scheduler-owned
`/a2a/{agentName}` entrypoint, the scheduler can become the authoritative
observer of invocation lifecycle. This avoids relying on agent-side task
lifecycle callbacks, which are fragile around crashes because the agent process
is exactly the component that may disappear before it can report state.

The ledger is the next recovery layer: record a request before forwarding it,
then mark it completed or failed after the downstream agent responds. Agent
restart logic can inspect interrupted or failed invocations, dispatch a
continuation prompt with the original `contextId`, and persist the recovery
outcome.

## 2. Goals

- Record scheduler-mediated invocations before forwarding to an agent.
- Cover both public scheduler entrypoints:
  - `POST /a2a/{agentName}` native A2A JSON-RPC;
  - `POST /agents/{agentName}/chat` CLI-friendly chat gateway.
- Record enough metadata for later recovery:
  - invocation id;
  - source (`a2a_proxy` or `chat_gateway`);
  - agent name;
  - A2A method when available;
  - `contextId`;
  - `messageId`;
  - user text;
  - status;
  - start/end timestamps;
  - error detail.
- Persist ledger events as append-only JSONL when the scheduler is started from
  an `ahsir.yaml` path.
- Replay JSONL at scheduler startup to reconstruct the current in-memory
  snapshot.
- Compact the JSONL ledger on startup:
  - delete completed records older than 7 days;
  - delete non-completed records older than 30 days.
- Record recovery lifecycle states: `recovering`, `recovered`, and
  `recovery_failed`.
- Keep response semantics unchanged.

## 3. Non-Goals

- Replaying the original user prompt verbatim.
- Parsing every possible A2A payload shape. The first parser extracts metadata
  from standard `message/send` and `message/stream` requests; other methods are
  still recorded with method/source/agent.
- Replaying or deduplicating requests.

## 4. Lifecycle

```text
request enters scheduler
  -> ledger Begin: append "started", in_flight
  -> scheduler forwards to target agent
  -> ledger Complete: append "completed" when forwarding succeeds
  -> ledger Fail: append "failed" when forwarding fails or upstream returns an error status
```

Statuses:

| Status | Meaning |
|---|---|
| `in_flight` | Scheduler has accepted the invocation and is waiting for downstream result. |
| `completed` | Downstream forwarding returned successfully. |
| `failed` | Forwarding failed, request context was canceled, or upstream returned a 5xx-style failure. |
| `recovering` | Scheduler is sending a continuation prompt after agent restart. |
| `recovered` | Continuation prompt returned successfully. |
| `recovery_failed` | Continuation prompt failed and may be retried after a later restart. |

## 5. Persistence

When `LoadConfig(path)` is used, the default ledger path is:

```text
<dir-of-ahsir.yaml>/.ahsir/ledger.jsonl
```

The file is append-only JSONL:

```json
{"type":"started","id":"inv-1","source":"a2a_proxy","agentName":"teacher","method":"message/send","contextId":"ctx-1","messageId":"msg-1","userText":"...","ts":"2026-06-07T00:00:00Z"}
{"type":"completed","id":"inv-1","ts":"2026-06-07T00:00:05Z"}
{"type":"failed","id":"inv-2","error":"proxy teacher: connection refused","ts":"2026-06-07T00:00:10Z"}
```

On startup, the scheduler replays the file in order:

- `started` creates or replaces the invocation as `in_flight`;
- `completed` marks the invocation complete;
- `failed` marks it failed with error detail;
- `recovering`, `recovered`, and `recovery_failed` update restart recovery
  lifecycle;
- `checkpoint` preserves the monotonic invocation id counter across compaction;
- invalid JSON lines are ignored so a partially written final line does not
  block scheduler startup.

After replay, the scheduler compacts the file by rewriting only retained
records:

| Record | Retention |
|---|---|
| `completed` | Delete after 7 days, using `finishedAt`. |
| not `completed` (`in_flight`, `failed`) | Delete after 30 days, using `startedAt`. |

The compaction rewrite is atomic: write a temporary JSONL file in the same
directory, then rename it over the old ledger.

## 6. Acceptance Tests

- `/agents/{name}/chat` creates an `in_flight` ledger entry before forwarding
  and marks it `completed` after response.
- `/a2a/{name}` creates an `in_flight` ledger entry before proxying native
  `message/send` and marks it `completed` after response.
- Failed scheduler proxy forwarding marks the ledger entry `failed`.
- `Begin`, `Complete`, and `Fail` append JSONL events.
- Replaying JSONL restores `in_flight`, `completed`, and `failed` records.
- Bad JSONL lines are ignored during replay.
- Startup compaction removes completed records older than 7 days.
- Startup compaction removes non-completed records older than 30 days.
- Startup compaction keeps newer completed and non-completed records.
- Recovery lifecycle states are persisted and replayed.
- Existing chat and A2A proxy responses are unchanged.

## 7. Restart Continuation

The continuation prompt layer consumes ledger records after a supervised
restart:

1. Find records for the restarted agent whose status indicates unfinished or
   recoverable work.
2. Re-send a scheduler-owned continuation prompt with the original `contextId`.
3. Mark records as `recovering`, `recovered`, or `recovery_failed`.
