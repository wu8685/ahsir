# Issue #37: CMA Runtime Reconciliation and Reusable Dynamic Ports

## Status

Confirmed by the user-authored issue #37 on 2026-08-03.

## Problem

The persisted CMA facade state and the scheduler's in-memory desired/runtime
state can diverge. A persisted, non-archived CMA Agent may therefore accept a
Session event even though its versioned scheduler Agent no longer exists. The
turn then terminates with `agent not found`.

Separately, auto-allocated Agent ports are consumed monotonically. Repeated
scale-to-zero wake cycles eventually exhaust a finite `port_range`, even after
the processes that used earlier ports have exited.

## Required behavior

### Dynamic ports

1. Auto-allocation scans the configured range circularly.
2. A port owned by a running scheduler Agent or reserved for an explicitly
   pinned desired Agent is not selected.
3. A candidate occupied by an unrelated local listener is skipped.
4. Selection and publication of a dynamically allocated port are atomic with
   respect to concurrent Agent starts and wakes.
5. When a dynamic Agent exits or fails to start, its port becomes eligible for
   reuse.
6. Repeated idle-stop/wake cycles can exceed the size of `port_range` without
   exhausting it when a usable port exists.

### CMA reconciliation

1. A persisted, non-archived CMA Agent version is desired state whenever a
   Session for that version is created or dispatched.
2. Before dispatch, the facade verifies that the versioned scheduler Agent
   exists. A process-local registration cache is only a hint.
3. If the scheduler Agent is missing, the facade registers it from the
   persisted CMA definition and waits until the runtime is healthy before the
   event request is acknowledged.
4. A typed, pre-stream scheduler `agent not found` response invalidates cached
   registration, performs one reconciliation attempt, and retries dispatch
   once. A generic wake `502` or a failure after streaming begins is not
   replayed, because the turn may already have started.
5. The retry is bounded to one; a second failure terminates the turn with an
   error containing the CMA Agent ID/version, scheduler name, and
   reconciliation outcome.
6. Archived CMA Agents are never registered or resurrected.
7. A registration conflict is success only after the existing scheduler Agent
   has been verified as compatible with the requested version/configuration.
8. Reconciliation is lazy and scoped to a Session's referenced Agent version;
   startup does not eagerly spawn every persisted version.
9. `POST .../events` preflights all `user.message` events synchronously before
   persisting or enqueueing any of them. If the Agent is archived, missing, or
   cannot be reconciled, the request fails without accepting a partial batch,
   so event-driven callers can retry before acknowledging their source event.
10. Generic transport errors and ambiguous proxy `502` responses are not
    automatically replayed. Only an explicit scheduler `agent not found`
    response known to precede upstream dispatch is eligible for reconciliation
    and one retry.

### Wake failure state

1. Concurrent callers waiting on one wake observe the leader's failure rather
   than continuing as though the Agent were healthy.
2. A process that starts but fails the wake health check does not leave an
   unretryable `running`/cache state; a later request can attempt recovery.
3. Pooled child startup uses the same complete start-plus-readiness
   singleflight semantics: every waiter observes the same result, and a failed
   child is cleaned up before its port is reusable.

## Compatibility definition

For issue #37, compatibility means that the scheduler's managed Agent config
matches the inline card and instance cap derived from the persisted CMA Agent.
An unverifiable or mismatched conflict is an error; it must not be collapsed
into generic idempotent success.

## Observability

Logs must identify dynamic-port exhaustion/skips, CMA reconciliation attempts,
the versioned scheduler name, and the final reconciliation result. Existing
HTTP status and event-stream behavior remains unchanged except for successful
self-healing and the more descriptive terminal error.

## Test matrix

- More idle-stop/wake cycles than the number of ports continue to work.
- Released dynamic ports are reused.
- Concurrent dynamic starts/wakes never select the same port.
- Foreign listeners are skipped and all-busy ranges fail clearly.
- A persisted Agent is registered again after scheduler state loss/restart.
- A stale registration cache is invalidated after `agent not found`.
- Dispatch retries at most once after reconciliation.
- Archived Agents are not resurrected.
- Compatible conflicts succeed; mismatched or unverifiable conflicts fail.

## Non-goals

- Persisting the scheduler's complete runtime state across restarts.
- Replaying or re-acknowledging the upstream Hetairoi source event.
- Unbounded dispatch retries.
