# AHSIR: Restart Continuation and Session Mapping Retention

**Status:** Implemented
**Date:** 2026-06-07
**Version:** 0.1.0

## 1. Motivation

Scheduler recovery now has three layers:

1. restart a local agent process after crash or health failure;
2. persist scheduler invocation lifecycle in `.ahsir/ledger.jsonl`;
3. persist each agent's `contextId -> provider session id` mapping in
   `<workspace>/.a2a/sessions.json`.

The missing layer is the post-restart continuation prompt. After the process is
back, the scheduler should nudge the recovered agent to inspect the existing
provider session for unfinished work and continue.

The session mapping file also needs bounded retention. It is a conversation
resume cache, not a task ledger. It should be maintained by maximum inactive
record count and remove the oldest inactive mappings first.

## 2. Goals

- After an unexpected restart succeeds, scan scheduler ledger records for that
  agent.
- Recover records that are unfinished or recoverable and have a `contextId`.
- Send a scheduler-owned continuation prompt using the original `contextId`.
- Mark recovery lifecycle as `recovering`, `recovered`, or
  `recovery_failed`.
- Keep recovery prompts out of the normal user-invocation ledger path.
- Keep `sessions.json` bounded by deleting oldest inactive mappings when the
  inactive mapping count exceeds `pool.max_evicted`.
- Never delete active session mappings because of `pool.max_evicted`.
- Keep time-based evicted TTL as a secondary safety valve.

## 3. Non-Goals

- Replaying the original user prompt verbatim.
- Recovering records without `contextId`.
- Recovering remote agents.
- Inferring provider-level completion from model output.

## 4. Session Mapping Retention

`sessions.json` records are owned by each `ahsir-agent` `SessionPool`.

Retention policy:

| Mapping state | Retention |
|---|---|
| active | Not deleted by `pool.max_evicted`; may become evicted after `pool.idle_ttl`. |
| evicted | Counted toward `pool.max_evicted`; oldest `evictedAt` entries are deleted first. |

Defaults:

| Field | Default |
|---|---|
| `pool.idle_ttl` | `30m` |
| `pool.evicted_ttl` | `30d` |
| `pool.max_evicted` | `1000` |

`pool.evicted_ttl` still deletes stale evicted records even when the count is
under the limit.

## 5. Continuation Flow

```text
agent process exits or is killed by health watcher
  -> scheduler restarts the same local agent config
  -> scheduler scans invocation ledger for that agent
  -> records in in_flight / failed with non-empty contextId become recovering
  -> scheduler sends continuation prompt through the agent's A2A endpoint
  -> success: recovered
  -> failure: recovery_failed
```

Continuation prompt:

```text
You were restarted while working on a previous task in this session. Inspect the existing conversation context and continue the interrupted work from where it left off. If the prior task was already complete, briefly report that no further action is needed.
```

## 6. Acceptance Tests

- `SessionPool` removes the oldest evicted mappings when `max_evicted` is
  exceeded.
- `SessionPool` does not remove active mappings when enforcing
  `max_evicted`.
- `agent-card.yaml` can configure `pool.idle_ttl`, `pool.evicted_ttl`, and
  `pool.max_evicted`.
- A restarted agent receives continuation prompts for recoverable invocations
  with `contextId`.
- Invocations without `contextId` are skipped and remain failed/in-flight.
- Recovery success is persisted as `recovered`.
- Recovery failure is persisted as `recovery_failed`.
