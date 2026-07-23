# Agent Detail Empty State Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Explain empty and unavailable Agent detail states without changing the console's APIs.

**Architecture:** The browser client derives the detail copy from the selected Agent plus active and archived collections. Playwright fixture scenarios exercise user-visible state; the existing Node client test protects historical participant selection. The canonical asset remains byte-identical to the plugin mirror.

**Tech Stack:** Vanilla JavaScript UI, Playwright, Node assertions, Go asset-parity tests.

## Global Constraints

- Text-only UI states; no new call to action or scheduler/persistence changes.
- Preserve archived read-only behavior and the exact approved Chinese copy.
- Keep `internal/ui/assets` and `plugin/src/internal/ui/assets` identical.

---

### Task 1: Specify and expose required UI states

**Files:**
- Create: `docs/superpowers/specs/2026-07-24-ui-empty-agent-detail-state-design.md`
- Modify: `ui-tests/core.spec.ts`
- Modify: `internal/ui/app_participant_test.js`

**Interfaces:**
- Consumes: fixture scenarios `empty` and `core`; detail container `#detailCard`.
- Produces: failing UI assertions for no-active, active-unselected, and unavailable historical participant states.

- [ ] **Step 1: Add failing assertions**

```ts
await expect(page.locator('#detailCard')).toContainText('当前没有运行中的 Agent');
await expect(page.locator('#detailCard')).toContainText('启动 Agent 后，可在这里查看运行状态和配置信息');
await expect(page.locator('#detailCard')).not.toContainText('选择一个 agent 查看详情');
```

- [ ] **Step 2: Run the focused UI test to verify it fails**

Run: `npm test --prefix ui-tests -- --grep 'empty state'`
Expected: FAIL because the old detail panel renders `选择一个 agent 查看详情`.

### Task 2: Render state-specific details

**Files:**
- Modify: `internal/ui/assets/app.js`
- Modify: `plugin/src/internal/ui/assets/app.js`

**Interfaces:**
- Consumes: `state.agents`, `state.archivedAgents`, and `state.agent`.
- Produces: exact approved messages in `renderDetail` and `renderUnavailableDetail`.

- [ ] **Step 1: Implement the minimum state branches**

```js
if (!state.agent && !state.agents.length) {
  card.innerHTML = '<div class="muted-line">当前没有运行中的 Agent</div>' +
    '<div class="muted-line">启动 Agent 后，可在这里查看运行状态和配置信息</div>';
  return;
}
```

- [ ] **Step 2: Mirror the canonical asset and run focused tests**

Run: `npm test --prefix ui-tests -- --grep 'empty state'`
Expected: PASS.

### Task 3: Verify the full regression surface

**Files:**
- Test: `ui-tests/core.spec.ts`
- Test: `internal/ui/app_participant_test.js`
- Test: `internal/ui/assets_parity_test.go`

- [ ] **Step 1: Run UI tests**

Run: `make test-ui`
Expected: PASS.

- [ ] **Step 2: Run Go build, tests, and coverage**

Run: `GO111MODULE=on go build ./... && GO111MODULE=on go test ./... && GO111MODULE=on go test -coverprofile=/tmp/cov ./... && go tool cover -func=/tmp/cov`
Expected: all commands exit 0; `internal/ui` asset-parity coverage is exercised.
