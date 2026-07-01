# Auth Baseline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Spec:** docs/superpowers/specs/2026-06-08-auth-baseline.md

**Goal:** A single privileged "admin token" gates the scheduler control plane
(`/admin/agents` + registry write), auto-generated to a `0600` file (env
override), auto-discovered by the CLI; plus a correctness fix so the internal
token is never sent to a registry-card URL.

**Tech Stack:** Go stdlib, existing test suite (`make test` runs `-race`).

---

## Slice A — Admin Token Store

### Task 1: LoadOrCreateAdminToken

**Files:**
- Create: `internal/scheduler/admintoken.go`
- Test: `internal/scheduler/admintoken_test.go`

- [x] Write failing tests: absent → generates a `0600` file (dir `0700`),
      returns source "file"; present non-empty → reads, source "file";
      `AHSIR_ADMIN_TOKEN` set → returns env value, source "env", no file
      created; empty/corrupt file → regenerates; token is 64 hex chars.
- [x] Run focused tests, verify RED.
- [x] Implement (reuse the existing random-hex generator).
- [x] Run focused tests, verify GREEN.

### Task 2: Wire onto Scheduler at Start

**Files:**
- Modify: `internal/scheduler/scheduler.go` (s.adminToken field; load in Start
  using the resolved config path; expose `adminToken()` accessor)
- Modify: `cmd/ahsir/main.go` (pass the resolved config path into the
  scheduler so the token sits beside ahsir.yaml)
- Test: `internal/scheduler/scheduler_test.go`

- [x] Write failing test: a scheduler started with a config path has a
      non-empty adminToken; restart with the same path reuses it.
- [x] Run focused tests, verify RED.
- [x] Implement; log the token source at startup (not the value).
- [x] Run focused tests, verify GREEN.

## Slice B — Token-Leak Fix (independent correctness)

### Task 3: Route internal token to the scheduler-recorded local address

**Files:**
- Modify: `internal/scheduler/scheduler.go` (agentProcess.localURL(); managed-
  agent lookup helper that returns local URL + token, or card.URL + no token
  for non-managed agents)
- Modify: `internal/scheduler/gateway.go` (handleA2AProxy target)
- Modify: `internal/scheduler/scheduler.go` (ChatWithAgentAs,
  ChatWithAgentAsync, AgentHistory build target from local address)
- Test: `internal/scheduler/gateway_test.go`, `internal/scheduler/scheduler_test.go`

- [x] Write failing tests: register a card whose `URL` points at a sink
      server; a scheduler-managed agent (entry in s.agents) routes chat /
      history / A2A-proxy to the recorded local address and the sink never
      receives the internal token; a non-managed (card-only) agent routes to
      card.URL and carries NO internal token.
- [x] Run focused tests, verify RED.
- [x] Implement the managed-vs-card-only split.
- [x] Run focused tests, verify GREEN.

## Slice C — Enforcement

### Task 4: requireAdminToken on /admin/agents + registry write

**Files:**
- Modify: `internal/scheduler/gateway.go` (middleware; gate handleAdmin and
  the registry write methods before delegating to registry; GET stays open)
- Test: `internal/scheduler/gateway_test.go`

- [x] Write failing tests: POST/DELETE `/admin/agents` and POST/DELETE
      `/agents` → 401 without `X-Ahsir-Admin-Token`, succeed with it; GET
      `/agents`, chat, history, tasks, invocations, config/timeouts stay open;
      an unauthorized same-name `POST /agents` leaves the existing card
      unchanged.
- [x] Run focused tests, verify RED.
- [x] Implement; empty adminToken = pass-through (degenerate/no-auth).
- [x] Run focused tests, verify GREEN.

### Task 5: --admin-token to agents for heartbeat registration

**Files:**
- Modify: `internal/scheduler/scheduler.go` (defaultAgentCommand adds
  `--admin-token`)
- Modify: `cmd/ahsir-agent/main.go` (flag → AgentWrapperConfig)
- Modify: `internal/wrapper/wrapper.go` (AgentWrapperConfig.AdminToken;
  heartbeat POST attaches `X-Ahsir-Admin-Token`)
- Test: `internal/wrapper/wrapper_test.go`, `internal/scheduler/scheduler_test.go`

- [x] Write failing tests: wrapper heartbeat POST carries the admin-token
      header when configured; scheduler agent command includes `--admin-token`.
- [x] Run focused tests, verify RED.
- [x] Implement.
- [x] Run focused tests, verify GREEN.

## Slice D — CLI Discovery + Doctor

### Task 6: CLI auto-discover + attach on control-plane calls

**Files:**
- Create: `cmd/ahsir/admintoken.go` (resolveAdminToken: env → file, never
  generates)
- Modify: `internal/schedulerclient/client.go` (admin/registry-write requests
  accept + attach the header)
- Modify: `cmd/ahsir/agent.go` (agent new/delete resolve + pass the token)
- Test: `internal/schedulerclient/client_test.go`, `cmd/ahsir/*_test.go`

- [x] Write failing tests: resolveAdminToken env-wins-over-file, missing-both
      → empty; client attaches header on admin start/stop requests; a 401
      surfaces a clear "token missing/unreadable" message.
- [x] Run focused tests, verify RED.
- [x] Implement.
- [x] Run focused tests, verify GREEN.

### Task 7: ahsir doctor auth line

**Files:**
- Modify: `cmd/ahsir/observability.go` (doctor reports auth enforced + source)
- Test: covered by real-run; add a small unit if the rendering is factored out.

- [x] Implement doctor auth reporting.
- [x] Manual/real-run check.

## Task 8: e2e + Final Verification + README

**Files:**
- Create/Modify: `e2e/auth_test.go`
- Modify: `README.md` (auth section: token file, env override, trust model,
  rotation = delete file + restart)

- [x] E2E: `ahsir agent new` against an enforcing scheduler succeeds via
      auto-discovery; a request with a wrong token → 401.
- [x] Run `make test` (race) + real-run with the demo scheduler.
- [x] README auth section; `git diff` review; report.
