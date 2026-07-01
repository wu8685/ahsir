# Shared-Context Collaboration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** docs/superpowers/specs/2026-06-08-shared-context-collaboration.md

**Goal:** Same-machine multi-client collaboration on one contextId: speaker
attribution, per-context transcript with replay, and FIFO queueing with
task-based waiting.

**Architecture:** Three independently landable slices. Speaker travels as A2A
`Message.Metadata["speaker"]` end to end. Transcript is a wrapper-owned
append-only jsonl per context, read through an internal-token-protected wrapper
endpoint proxied by the scheduler. The turn queue is a bounded FIFO gate on the
session-pool entry; overflow degrades to the existing `ErrTurnInFlight`/409
contract; `configuration.blocking=false` returns a `submitted` task immediately.

**Tech Stack:** Go standard library, vendored a2a-go SDK, existing test suite
(`make test` runs with `-race`).

---

## Slice 1 — Speaker Attribution

### Task 1: Speaker plumbing (CLI payload → gateway → A2A metadata → ledger)

**Files:**
- Modify: `internal/schedulerclient/client.go` (ChatWithAgent carries speaker)
- Modify: `internal/scheduler/gateway.go` (chatRequest.Speaker, ledger Begin)
- Modify: `internal/scheduler/scheduler.go` (ChatWithAgent passthrough)
- Modify: `internal/wrapper/client.go` (SendMessage sets Message.Metadata)
- Modify: `internal/scheduler/invocation_ledger.go` (Speaker field, persisted)
- Modify: `cmd/ahsir/observability.go` (trace renders speaker)
- Test: `internal/schedulerclient/client_test.go`, `internal/scheduler/gateway_test.go`, `internal/scheduler/invocation_ledger_test.go`, `internal/wrapper/client_test.go`

- [x] Write failing tests: chat POST body carries `speaker`; gateway forwards
      it onto the outbound A2A message metadata; ledger started events record
      and replay it; trace output prints speaker over the constant source.
- [x] Run focused tests and verify RED.
- [x] Implement the plumbing (no prompt change yet).
- [x] Run focused tests and verify GREEN.

### Task 2: Executor prompt injection

**Files:**
- Modify: `internal/wrapper/executor.go` (both Execute and streaming paths)
- Modify: prompt builder (`BuildSystemPrompt` site)
- Test: `internal/wrapper/executor_test.go`

- [x] Write failing tests: with `Metadata["speaker"]="alice"` the provider
      prompt contains `[speaker: alice]` before the user text and the
      multi-speaker system-prompt line; without metadata the prompt is
      byte-identical to today's output.
- [x] Run focused tests and verify RED.
- [x] Implement tag + system-prompt line injection.
- [x] Run focused tests and verify GREEN.

### Task 3: CLI `--as` flag

**Files:**
- Modify: `cmd/ahsir/cli.go` (chatCmd)
- Test: `internal/schedulerclient/client_test.go` (payload-level assertion)

- [x] Write failing test: empty `--as` resolves to the OS username
      (`os/user.Current()`, fallback `$USER`); explicit value wins.
- [x] Run focused tests and verify RED.
- [x] Implement flag + default resolution.
- [x] Run focused tests and verify GREEN.

## Slice 2 — Transcript + History

### Task 4: Transcript store (wrapper-owned jsonl)

**Files:**
- Create: `internal/wrapper/transcript.go`
- Test: `internal/wrapper/transcript_test.go`

- [x] Write failing tests: append writes one jsonl record per turn (success
      and failure shapes); file is `hex(sha256(contextId))[:16].jsonl` under
      `.a2a/transcripts/` with `0700` dir / `0600` files; raw contextId never
      appears in any path; `index.json` maps contextId → file; reads return
      turns in order.
- [x] Run focused tests and verify RED.
- [x] Implement append-only store + index.
- [x] Run focused tests and verify GREEN.

### Task 5: Executor wiring + wrapper /history endpoint

**Files:**
- Modify: `internal/wrapper/executor.go` (append at turn end, both paths)
- Modify: `internal/wrapper/wrapper.go` (route `GET /history?contextId=`,
  internal-token protected like all wrapper ingress)
- Test: `internal/wrapper/executor_test.go`, `internal/wrapper/wrapper_test.go`

- [x] Write failing tests: a completed turn appends {ts, turn, taskId, speaker,
      userText, reply, status, durationMs}; a failed turn appends status=failed
      with error; `/history` returns the jsonl turns as JSON, 401 without
      internal token, empty list for unknown contextId.
- [x] Run focused tests and verify RED.
- [x] Implement wiring + endpoint.
- [x] Run focused tests and verify GREEN.

### Task 6: Scheduler proxy + `ahsir history`

**Files:**
- Modify: `internal/scheduler/gateway.go` (`GET /agents/{name}/history/{contextId}`)
- Modify: `internal/scheduler/scheduler.go` (history fetch via agent client)
- Modify: `internal/schedulerclient/client.go` (GetHistory)
- Modify: `cmd/ahsir/observability.go` (history command rendering)
- Modify: `cmd/ahsir/main.go` (subcommand registration + help)
- Test: `internal/scheduler/gateway_test.go`, `internal/schedulerclient/client_test.go`

- [x] Write failing tests: gateway proxies history with internal token; 404 for
      unknown agent; client decodes turns; renderer shows speaker + timestamps.
- [x] Run focused tests and verify RED.
- [x] Implement proxy, client method, CLI command.
- [x] Run focused tests and verify GREEN.

## Slice 3 — FIFO Queue + Task-Based Waiting

### Task 7: Session-pool FIFO turn gate

**Files:**
- Modify: `internal/wrapper/session_pool.go` (bounded per-entry FIFO)
- Modify: `internal/wrapper/card.go` (pool.queue_depth, default 4, 0 = off)
- Test: `internal/wrapper/session_pool_test.go`

- [x] Write failing tests: N concurrent turns on one context all run, in
      arrival order; depth+1 waiters → exactly one ErrTurnInFlight; a waiter
      whose context is cancelled leaves without running; distinct contexts stay
      parallel; queue_depth=0 restores fail-fast.
- [x] Run focused tests with `-race` and verify RED.
- [x] Implement the gate.
- [x] Run focused tests with `-race` and verify GREEN.

### Task 8: Non-blocking send (`configuration.blocking=false`)

**Files:**
- Modify: `internal/wrapper/server.go` (OnSendMessage branches on blocking)
- Modify: `internal/wrapper/executor.go` (async execution updates TaskStore
  submitted → working → terminal)
- Test: `internal/wrapper/server_test.go`, `internal/wrapper/executor_test.go`

- [x] Write failing tests: blocking=false returns a `submitted` task
      immediately; `tasks/get` converges to the same terminal state and text
      as the blocking path; a task cancelled while queued is skipped at
      dequeue; blocking unset/true keeps today's hold-until-done semantics.
- [x] Run focused tests and verify RED.
- [x] Implement the async path.
- [x] Run focused tests and verify GREEN.

### Task 9: Ledger `queued` state + gateway async chat

**Files:**
- Modify: `internal/scheduler/invocation_ledger.go` (queued event)
- Modify: `internal/scheduler/gateway.go` (chatRequest.Async → returns taskId)
- Modify: `internal/scheduler/scheduler.go` (non-blocking send variant)
- Test: `internal/scheduler/invocation_ledger_test.go`, `internal/scheduler/gateway_test.go`

- [x] Write failing tests: async chat returns 202 + taskId without waiting;
      ledger timeline shows started → queued → terminal; trace renders the
      queued phase.
- [x] Run focused tests and verify RED.
- [x] Implement.
- [x] Run focused tests and verify GREEN.

### Task 10: CLI waiting modes + restart degradation

**Files:**
- Modify: `cmd/ahsir/cli.go` (default sync-via-poll, `--async` prints taskId)
- Modify: `internal/schedulerclient/client.go` (poll with backoff; 404 maps to
  an error naming `ahsir history`)
- Test: `internal/schedulerclient/client_test.go`

- [x] Write failing tests: sync mode polls to terminal and prints the reply;
      `--async` prints the taskId and exits 0; polling that hits 404 (agent
      restarted) returns the degradation error pointing at
      `ahsir history <agent> <contextId>`.
- [x] Run focused tests and verify RED.
- [x] Implement.
- [x] Run focused tests and verify GREEN.

## Task 11: E2E + Final Verification

**Files:**
- Create: `e2e/shared_context_test.go`
- Modify: `e2e/same_context_busy_test.go` (depth=0 pin note if needed)
- Modify: `README.md` (transcript privacy stance alongside ledger notes)

- [x] E2E: two clients with different `--as` interleave on one context; both
      complete in order (no 409 at default depth); `ahsir history` shows both
      turns with correct speakers and full replies; trace shows `queued`.
- [x] E2E: `--async` returns a taskId while queued; polling reaches completed.
- [x] Run `make test` (race suite) and the e2e suite; verify GREEN.
- [x] Inspect `git diff`; report changed files, tests, remaining risks.
