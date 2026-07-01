# AHSIR: Agent Recovery and Health Supervision

**Status:** Implemented through active health watcher and restart continuation prompt
**Date:** 2026-06-06
**Version:** 0.1.0

## 1. Motivation

ahsir runs each local agent as an `ahsir-agent` child process. Long-running
work needs the scheduler to survive common local failures:

- the agent process exits unexpectedly;
- the process stays alive but its HTTP endpoint is unhealthy or unreachable;
- an intentional stop must not be confused with a crash;
- a restarted agent must eventually resume the previous session and be nudged
  to continue unfinished work.

This spec records the recovery behavior as it is built, so implementation,
tests, and docs stay aligned.

## 2. Goals

- Restart local agents that exit unexpectedly.
- Expose process-level operational endpoints on every `ahsir-agent`.
- Actively probe local agents through `/healthz`.
- Kill and restart a local agent after repeated health failures.
- Keep explicit user stops and scheduler shutdown terminal.
- Preserve the same configured port across restarts.
- Resume previous runtime session and submit a continuation prompt after
  restart when scheduler ledger records indicate unfinished work.

## 3. Non-Goals

- Restart remote agents registered by URL.
- Infer model quality or tool correctness from `/healthz`.
- Detect an in-flight provider turn that is still producing no events. That
  requires executor/session-level turn progress tracking and is a separate
  watchdog.
- Replay the original user prompt verbatim. Recovery uses a continuation prompt
  against the existing `contextId` instead.

## 4. Operational Endpoints

Each `ahsir-agent` HTTP server exposes:

| Endpoint | Meaning |
|---|---|
| `GET /healthz` | Liveness: the process and HTTP server can answer. |
| `GET /readyz` | Readiness: the agent card is loaded and executor is wired. |
| `GET /.well-known/agent-card.json` | A2A Agent Card discovery. |

`/healthz` is the scheduler's active liveness probe. `/readyz` is intentionally
stricter and is useful for future routing decisions, but the first active
recovery loop only needs to know whether the local HTTP server is alive.

## 5. Scheduler Supervisor

For every local agent started by the scheduler:

1. The scheduler records the desired `AgentConfig`.
2. It starts `ahsir-agent` with the resolved port.
3. It monitors process exit.
4. If the process exits while still desired, the scheduler schedules a restart
   with exponential backoff.
5. If the user calls `StopAgent` or the scheduler is shutting down, the desired
   config is removed and no restart happens.

Backoff starts at 1s and caps at 30s by default. Tests override this to keep
failure cases fast.

## 6. Active Health Watcher

For each local process, the scheduler also starts a health watcher:

1. Wait for `startupGrace` before the first probe.
2. Periodically request `http://127.0.0.1:<port>/healthz`.
3. Treat non-2xx responses, connection failures, and request timeouts as
   failures.
4. Reset the failure counter after a successful probe.
5. After `failureThreshold` consecutive failures, cancel/kill the current agent
   process.
6. The existing process-exit supervisor observes that exit and performs the
   restart, preserving the resolved port.

Default health watcher tuning:

| Field | Default |
|---|---|
| startup grace | 5s |
| interval | 5s |
| request timeout | 2s |
| consecutive failures | 3 |

## 7. Acceptance Tests

- An agent process that exits unexpectedly is restarted on the same port.
- `StopAgent` does not restart a stopped agent.
- An alive process whose `/healthz` repeatedly fails is killed and restarted on
  the same port.
- Transient `/healthz` failures below the threshold do not restart the agent.
- `StopAgent` cancels the health watcher before it can trigger recovery.

## 8. Resume and Continue Prompt

Restarting the process is only the first layer. After a supervised restart,
the scheduler now:

- consumes scheduler invocation ledger records created by the
  scheduler-owned `/a2a/{agent}` and `/agents/{agent}/chat` entrypoints;
- reuses the persisted session metadata for the same A2A `contextId`;
- detects unfinished or failed work;
- enqueues a continuation prompt after restart asking the runtime to
  inspect prior context and continue the interrupted task;
- logs the recovery attempt, resumed session id, continuation prompt dispatch,
  and resulting task state.
