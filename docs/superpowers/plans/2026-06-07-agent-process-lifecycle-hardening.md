# Agent Process Lifecycle Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make scheduler, agent, and provider subprocess lifecycle robust against orphaning, stale port ownership, and Codex notify helper leakage.

**Architecture:** Add a small internal process helper package for process-group start/kill and local process discovery. Use it from scheduler agent management and wrapper provider sessions. Add agent-side registry supervision in `cmd/ahsir-agent`.

**Tech Stack:** Go standard library, darwin process tools via `ps`/`lsof`, existing Go test suite.

---

### Task 1: Process Group Helper

**Files:**
- Create: `internal/process/process.go`
- Create: `internal/process/process_unix.go`
- Test: `internal/process/process_test.go`

- [ ] Write failing tests for `PrepareCommand`, `KillTree`, and local listener discovery.
- [ ] Run `go test ./internal/process` and verify RED.
- [ ] Implement process-group setup and kill helpers.
- [ ] Run `go test ./internal/process` and verify GREEN.

### Task 2: Scheduler Agent Tree Cleanup

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Test: `internal/scheduler/scheduler_test.go`

- [ ] Write failing tests proving scheduler agent commands use process groups.
- [ ] Write failing tests proving stop and unhealthy restart call process-tree kill.
- [ ] Run focused scheduler tests and verify RED.
- [ ] Wire scheduler start/stop/restart through process helper.
- [ ] Run focused scheduler tests and verify GREEN.

### Task 3: Stale Local Agent Eviction

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Test: `internal/scheduler/scheduler_test.go`

- [ ] Write failing tests for stale ahsir-agent eviction by port/workspace.
- [ ] Write failing tests that unrelated listeners are rejected, not killed.
- [ ] Run focused scheduler tests and verify RED.
- [ ] Implement pre-start stale local agent eviction.
- [ ] Run focused scheduler tests and verify GREEN.

### Task 4: Agent Registry Supervision

**Files:**
- Modify: `cmd/ahsir-agent/main.go`
- Test: `cmd/ahsir-agent/main_test.go`

- [ ] Write failing tests for registry monitor cancellation after grace.
- [ ] Run focused agent tests and verify RED.
- [ ] Implement monitor with configurable defaults through functions.
- [ ] Run focused agent tests and verify GREEN.

### Task 5: Provider Process Tree Cleanup And Codex Notify Isolation

**Files:**
- Modify: `internal/wrapper/session_claude.go`
- Modify: `internal/wrapper/session_codex.go`
- Test: `internal/wrapper/session_claude_test.go`
- Test: `internal/wrapper/session_codex_test.go`
- Test: `cmd/ahsir-agent/main_test.go`

- [ ] Write failing tests for provider command process groups.
- [ ] Write failing tests for isolated Codex `CODEX_HOME` injection.
- [ ] Run focused wrapper and agent tests and verify RED.
- [ ] Implement provider process-group setup and Codex env isolation.
- [ ] Run focused wrapper and agent tests and verify GREEN.

### Task 6: Final Verification

**Files:**
- No additional files expected.

- [ ] Run `go test ./...`.
- [ ] Inspect `git diff`.
- [ ] Report changed files, tests, and remaining risks.
