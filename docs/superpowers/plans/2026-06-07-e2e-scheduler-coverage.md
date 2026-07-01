# E2E Scheduler Coverage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend P0/P1 tests so real and integration E2E coverage matches the scheduler-owned entrypoint architecture.

**Architecture:** Real-LLM E2E should drive public traffic through scheduler `/a2a/{agent}` by default, while direct agent ports remain an explicit internal/debug path. Scheduler and wrapper integration tests cover restart continuation, ledger persistence/rotation, and session mapping retention without requiring live provider CLIs.

**Tech Stack:** Go test, build-tagged `e2e` package, scheduler integration tests, wrapper SessionPool persistence tests.

---

### Task 1: Scheduler-Owned A2A E2E Entry

**Files:**
- Modify: `e2e/framework_test.go`
- Modify: `e2e/delegation_test.go`
- Modify: `e2e/session_reuse_test.go`
- Modify: `e2e/codex_provider_test.go`
- Modify: `e2e/mixed_provider_test.go`
- Modify: `e2e/concurrent_test.go`
- Modify: `e2e/sse_streaming_test.go`
- Create: `e2e/scheduler_entrypoint_test.go`

- [x] **Step 1: Write failing tests**
  Add a build-tagged E2E test that calls `fix.sendMessageToAgent("student", ...)` and asserts scheduler ledger/proxy markers, plus a direct-port negative helper call.

- [x] **Step 2: Verify red**
  Run: `go test -tags=e2e ./e2e -run TestSchedulerEntrypoint_E2E`
  Expected: compile failure for the missing helper or a failing assertion proving the helper still bypasses scheduler.

- [x] **Step 3: Implement minimal helper changes**
  Add `sendMessageToAgent`, `sendMessageStreamToAgent`, and `sendDirectMessage` helpers. Update existing real E2E cases to use the scheduler-owned helper.

- [x] **Step 4: Verify green/compile**
  Run: `go test -tags=e2e ./e2e`
  Expected: skipped without env gates or passing when provider env is enabled.

### Task 2: Restart Continuation And Ledger Integration

**Files:**
- Modify: `internal/scheduler/gateway_test.go`
- Modify: `internal/scheduler/scheduler_test.go`
- Modify: `internal/scheduler/invocation_ledger_test.go`

- [x] **Step 1: Write failing tests**
  Cover recoverable invocation recording through `/a2a/{agent}`, replay from `.ahsir/ledger.jsonl`, and continuation dispatch after supervised restart.

- [x] **Step 2: Verify red**
  Run the targeted scheduler tests before implementation adjustments.

- [x] **Step 3: Implement minimal test/support changes**
  Prefer existing fake A2A servers and scheduler hooks. Do not add live LLM dependencies for ledger/restart behavior.

- [x] **Step 4: Verify green**
  Run: `go test ./internal/scheduler`

### Task 3: Session Mapping Retention Integration

**Files:**
- Modify: `internal/wrapper/session_pool_persist_test.go`

- [x] **Step 1: Write failing test**
  Exercise persisted evicted session mappings with `max_evicted`, asserting the oldest inactive records are removed.

- [x] **Step 2: Verify red**
  Run: `go test ./internal/wrapper -run TestSessionPool`

- [x] **Step 3: Implement minimal support if needed**
  Keep retention behavior in SessionPool/FilePersistence; avoid scheduler coupling.

- [x] **Step 4: Verify green**
  Run: `go test ./internal/wrapper`

### Task 4: Documentation Sync

**Files:**
- Modify: `e2e/README.md`
- Modify: `README.md` if command examples need adjustment

- [x] **Step 1: Update e2e README**
  Document current files, env gates, scheduler-owned helpers, and direct-port debug helper.

- [x] **Step 2: Verify docs references**
  Run: `rg "fix\\.studentPort|fix\\.teacherPort|AHSIR_E2E_CODEX|/a2a/" e2e/README.md README.md`
