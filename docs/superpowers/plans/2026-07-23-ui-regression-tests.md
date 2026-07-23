# UI Regression Tests Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add deterministic fast and Chromium UI regression tests for ahsir's core live-agent, archived-agent, rail-layout, responsive-navigation, empty, and scheduler-error experiences.

**Architecture:** Keep API/state checks in Go and the existing dependency-free Node fake DOM suite, then run Playwright Chromium against the real embedded UI handler backed by a test-only in-memory scheduler. Treat `internal/ui/assets/` as canonical and enforce a byte-identical plugin mirror.

**Tech Stack:** Go 1.24.4+, Node 24, `@playwright/test` 1.61.1, Chromium, GNU Make, GitHub Actions.

## Global Constraints

- Bind the E2E fixture only to `127.0.0.1:19809`, with Playwright `reuseExistingServer: false`.
- Run one Playwright worker; retry once in CI and never retry locally.
- Do not require API keys, a live scheduler, user workspaces, external network calls, or production Go dependencies.
- Install and test Chromium only; do not add Firefox, WebKit, or screenshot-golden assertions.
- Retain screenshots, traces, browser errors, and fixture logs only for failed CI tests.
- Keep `internal/ui/assets/` canonical and `plugin/src/internal/ui/assets/` byte-identical.
- Before completion, both `make test-ui` and `make test` must pass.

## File Map

- Create `internal/ui/assets_parity_test.go`: compare canonical and plugin asset trees.
- Modify `plugin/src/internal/ui/assets/app.css`: synchronize the canonical rail constraints.
- Modify `plugin/src/internal/ui/assets/index.html`: synchronize archived-list markup and limits.
- Create `internal/ui/e2e/testserver/main.go`: start the real UI handler and mock scheduler.
- Create `internal/ui/e2e/testserver/fixture.go`: own named fixture state and scheduler routes.
- Create `internal/ui/e2e/testserver/fixture_test.go`: lock fixture-control and scheduler contracts.
- Create `ui-tests/package.json` and `ui-tests/package-lock.json`: pin Playwright.
- Create `ui-tests/playwright.config.ts`: browser, server lifecycle, artifacts, and CI settings.
- Create `ui-tests/helpers.ts`: reset scenarios and collect browser errors.
- Create `ui-tests/core.spec.ts`: semantic, interaction, layout, and responsive cases.
- Modify `internal/ui/assets/index.html`: add the narrow-screen three-surface navigation.
- Modify `internal/ui/assets/app.js`: wire deterministic mobile surface switching.
- Modify `internal/ui/assets/app.css`: style active mobile controls and surface visibility.
- Modify `Makefile`: expose fast, dependency, browser, and combined UI targets.
- Create `.github/workflows/ui-test.yml`: enforce the UI regression check on PRs and `main`.

---

### Task 1: Asset parity contract and mirror repair

**Files:**
- Create: `internal/ui/assets_parity_test.go`
- Modify: `plugin/src/internal/ui/assets/app.css`
- Modify: `plugin/src/internal/ui/assets/index.html`

**Interfaces:**
- Consumes: canonical files below `internal/ui/assets/`.
- Produces: `TestPluginAssetsMatchCanonical`, which fails on missing, extra, or byte-different regular files.

- [ ] **Step 1: Write the failing parity test**

```go
package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func regularFiles(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil { return err }
		if entry.Type().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil { return err }
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil { t.Fatalf("walk %s: %v", root, err) }
	sort.Strings(names)
	return names
}

func TestPluginAssetsMatchCanonical(t *testing.T) {
	canonical := "assets"
	mirror := filepath.Join("..", "..", "plugin", "src", "internal", "ui", "assets")
	want, got := regularFiles(t, canonical), regularFiles(t, mirror)
	if !reflect.DeepEqual(got, want) { t.Fatalf("asset names = %v, want %v", got, want) }
	for _, name := range want {
		a, err := os.ReadFile(filepath.Join(canonical, filepath.FromSlash(name)))
		if err != nil { t.Fatal(err) }
		b, err := os.ReadFile(filepath.Join(mirror, filepath.FromSlash(name)))
		if err != nil { t.Fatal(err) }
		if !bytes.Equal(a, b) { t.Errorf("plugin asset differs: %s", name) }
	}
}
```

- [ ] **Step 2: Run the test and verify red**

Run: `go test ./internal/ui -run TestPluginAssetsMatchCanonical -count=1`

Expected: FAIL naming `app.css` and `index.html` as different.

- [ ] **Step 3: Synchronize the two known drifted files from the canonical tree**

Run:

```bash
cp internal/ui/assets/app.css plugin/src/internal/ui/assets/app.css
cp internal/ui/assets/index.html plugin/src/internal/ui/assets/index.html
```

- [ ] **Step 4: Verify green in both Go modules**

Run: `go test ./internal/ui -count=1 && (cd plugin/src && go test ./internal/ui -count=1)`

Expected: both packages PASS; the existing Node participant suite prints `participant selection regression tests passed` when Node is installed.

- [ ] **Step 5: Commit the parity contract**

```bash
git add internal/ui/assets_parity_test.go plugin/src/internal/ui/assets/app.css plugin/src/internal/ui/assets/index.html
git commit -m "test: enforce UI asset parity"
```

### Task 2: Deterministic scheduler fixture

**Files:**
- Create: `internal/ui/e2e/testserver/fixture.go`
- Create: `internal/ui/e2e/testserver/fixture_test.go`
- Create: `internal/ui/e2e/testserver/main.go`

**Interfaces:**
- Produces: `newFixture() *fixture`, `(*fixture).schedulerHandler() http.Handler`, and `(*fixture).controlHandler() http.Handler`.
- Produces named scenarios `core`, `empty`, and `scheduler-error` selected by `POST /__test/reset?scenario=<name>`.
- The test executable serves the real `ui.New(mockURL, "").Handler()` on `127.0.0.1:19809`.

- [ ] **Step 1: Write fixture contract tests first**

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFixtureResetAndCoreRoutes(t *testing.T) {
	f := newFixture()
	req := httptest.NewRequest(http.MethodPost, "/__test/reset?scenario=core", nil)
	w := httptest.NewRecorder()
	f.controlHandler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent { t.Fatalf("reset status = %d", w.Code) }

	for _, path := range []string{"/agents", "/archived-agents", "/rooms", "/invocations"} {
		w = httptest.NewRecorder()
		f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK { t.Errorf("GET %s = %d", path, w.Code) }
	}
}

func TestFixtureChatCompletes(t *testing.T) {
	f := newFixture()
	w := httptest.NewRecorder()
	f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(
		http.MethodPost, "/agents/live-codex/chat", strings.NewReader(`{"message":"E2E ping"}`),
	))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "task-live-01") {
		t.Fatalf("chat = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/agents/live-codex/tasks/task-live-01", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "E2E fixed reply") {
		t.Fatalf("task = %d %s", w.Code, w.Body.String())
	}
}

func TestSchedulerErrorScenario(t *testing.T) {
	f := newFixture()
	f.setScenario("scheduler-error")
	w := httptest.NewRecorder()
	f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if w.Code != http.StatusServiceUnavailable { t.Fatalf("status = %d", w.Code) }
}
```

- [ ] **Step 2: Run the fixture tests and verify red**

Run: `go test ./internal/ui/e2e/testserver -count=1`

Expected: FAIL because `newFixture` and its handlers do not exist.

- [ ] **Step 3: Implement the fixture state and exact route table**

Implement `fixture.go` with a mutex-protected `scenario string`, JSON helpers, and this route contract:

```go
type fixture struct {
	mu       sync.RWMutex
	scenario string
}

func newFixture() *fixture { return &fixture{scenario: "core"} }

var agents = []map[string]any{{
	"name": "live-codex", "url": "http://127.0.0.1:9802", "status": "online",
	"version": "1.2.3-e2e", "description": "deterministic live agent",
	"skills": []map[string]string{{"name": "coding"}},
}}

var liveHistory = []map[string]any{{
	"turn": 1, "speaker": "operator", "userText": "Existing core question",
	"reply": "Existing core reply", "status": "completed",
	"ts": "2026-07-23T02:00:01Z", "durationMs": 1000,
}}

func coreInvocations() []map[string]any {
	rows := make([]map[string]any, 0, 18)
	for i := 1; i <= 18; i++ {
		title := fmt.Sprintf("Core conversation %02d", i)
		if i == 1 { title = "Existing core question" }
		rows = append(rows, map[string]any{
			"agentName": "live-codex", "contextId": fmt.Sprintf("ctx-live-%02d", i),
			"userText": title, "status": "completed", "speaker": "operator",
			"source": "console", "startedAt": fmt.Sprintf("2026-07-23T02:%02d:00Z", i),
			"durationMs": 1000,
		})
	}
	return rows
}

func coreRooms() []map[string]any {
	rows := make([]map[string]any, 0, 14)
	for i := 1; i <= 14; i++ {
		rows = append(rows, map[string]any{
			"id": fmt.Sprintf("room-%02d", i), "topic": fmt.Sprintf("Core room %02d", i),
			"mode": "collaboration", "status": "stopped", "participants": []string{"live-codex"},
			"organizer": "live-codex",
		})
	}
	return rows
}

func coreArchived() []map[string]any {
	contexts := make([]map[string]any, 0, 8)
	for i := 1; i <= 8; i++ {
		title := fmt.Sprintf("Archived context %02d", i)
		if i == 1 { title = "Archived core context" }
		contexts = append(contexts, map[string]any{
			"contextId": fmt.Sprintf("ctx-archived-%02d", i), "title": title, "turns": 1,
			"lastStatus": "completed", "lastActivity": fmt.Sprintf("2026-07-22T02:%02d:00Z", i),
		})
	}
	return []map[string]any{{"name": "archived-kimi", "contexts": contexts}}
}
```

Implement `controlHandler` as POST-only, accept exactly the three scenario names,
and respond with 204. Implement `schedulerHandler` with these exact branches:

```go
func (f *fixture) schedulerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.currentScenario() == "scheduler-error" {
			writeFixtureJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "fixture scheduler unavailable"})
			return
		}
		empty := f.currentScenario() == "empty"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agents":
			if empty { writeFixtureJSON(w, 200, []any{}); return }
			writeFixtureJSON(w, 200, agents)
		case r.Method == http.MethodGet && r.URL.Path == "/archived-agents":
			if empty { writeFixtureJSON(w, 200, []any{}); return }
			writeFixtureJSON(w, 200, coreArchived())
		case r.Method == http.MethodGet && r.URL.Path == "/rooms":
			if empty { writeFixtureJSON(w, 200, []any{}); return }
			writeFixtureJSON(w, 200, coreRooms())
		case r.Method == http.MethodGet && r.URL.Path == "/invocations":
			if empty { writeFixtureJSON(w, 200, []any{}); return }
			rows := coreInvocations()
			if id := r.URL.Query().Get("contextId"); id != "" {
				filtered := rows[:0]
				for _, row := range rows { if row["contextId"] == id { filtered = append(filtered, row) } }
				rows = filtered
			}
			writeFixtureJSON(w, 200, rows)
		case r.Method == http.MethodGet && r.URL.Path == "/agents/live-codex/history/ctx-live-01":
			writeFixtureJSON(w, 200, liveHistory)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/agents/live-codex/history/"):
			writeFixtureJSON(w, 200, []any{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/agents/archived-kimi/history/"):
			writeFixtureJSON(w, 200, []map[string]any{{
				"turn": 1, "speaker": "operator", "userText": "Archived retained question",
				"reply": "Archived retained reply", "status": "completed",
				"ts": "2026-07-22T02:00:01Z", "durationMs": 1000,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/agents/live-codex/config":
			writeFixtureJSON(w, 200, map[string]string{"path": "/tmp/e2e/agent-card.yaml", "yaml": "name: live-codex"})
		case r.Method == http.MethodPost && r.URL.Path == "/agents/live-codex/chat":
			writeFixtureJSON(w, http.StatusAccepted, map[string]string{"taskId": "task-live-01", "contextId": "ctx-live-01"})
		case r.Method == http.MethodGet && r.URL.Path == "/agents/live-codex/tasks/task-live-01":
			writeFixtureJSON(w, 200, map[string]any{
				"status": map[string]string{"state": "completed"},
				"history": []map[string]any{{"role": "agent", "parts": []map[string]string{{"kind": "text", "text": "E2E fixed reply"}}}},
			})
		default:
			http.NotFound(w, r)
		}
	})
}
```

`writeFixtureJSON` sets `Content-Type: application/json`, writes the status,
and encodes the value. `currentScenario` and `setScenario` must hold `mu.RLock`
and `mu.Lock`, respectively. The control handler returns 405 for non-POST
requests and 400 for a scenario outside `core`, `empty`, and `scheduler-error`.

- [ ] **Step 4: Implement the test-only executable**

```go
func main() {
	f := newFixture()
	mock := httptest.NewServer(f.schedulerHandler())
	defer mock.Close()
	console, err := ui.New(mock.URL, "")
	if err != nil { log.Fatal(err) }

	mux := http.NewServeMux()
	mux.Handle("/__test/", f.controlHandler())
	mux.Handle("/", console.Handler())
	server := &http.Server{Addr: "127.0.0.1:19809", Handler: requestLogger(mux)}
	log.Printf("UI E2E fixture listening on http://%s", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed { log.Fatal(err) }
}
```

Implement `requestLogger` using a response-writer wrapper that records the
status passed to `WriteHeader`; default it to 200 when a handler only calls
`Write`. Log `method=<method> path=<request-uri> status=<status>` and never log
request headers or bodies.

- [ ] **Step 5: Verify fixture tests and real UI serving**

Run: `go test ./internal/ui/e2e/testserver -count=1`

Expected: PASS.

Run in one terminal: `go run ./internal/ui/e2e/testserver`

Run in another: `curl -fsS http://127.0.0.1:19809/ | rg '<title>|id="contexts"'`

Expected: the embedded ahsir HTML contains both matches. Stop the fixture with Ctrl-C.

- [ ] **Step 6: Commit the fixture**

```bash
git add internal/ui/e2e/testserver
git commit -m "test: add deterministic UI fixture server"
```

### Task 3: Playwright harness and page-state cases

**Files:**
- Create: `ui-tests/package.json`
- Create: `ui-tests/package-lock.json`
- Create: `ui-tests/playwright.config.ts`
- Create: `ui-tests/helpers.ts`
- Create: `ui-tests/core.spec.ts`

**Interfaces:**
- Consumes: `POST /__test/reset?scenario=<name>` and base URL `http://127.0.0.1:19809`.
- Produces: `resetScenario(request, scenario)` and `collectPageErrors(page)`.

- [ ] **Step 1: Pin Playwright and generate the lock file**

Create `ui-tests/package.json`:

```json
{
  "name": "ahsir-ui-tests",
  "private": true,
  "scripts": { "test": "playwright test" },
  "devDependencies": { "@playwright/test": "1.61.1" }
}
```

Run: `npm install --prefix ui-tests --package-lock-only`

Expected: `ui-tests/package-lock.json` records `@playwright/test` 1.61.1.

- [ ] **Step 2: Configure Chromium and fixture lifecycle**

```ts
import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: 'core.spec.ts',
  outputDir: 'test-results',
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: 'http://127.0.0.1:19809',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    video: 'off',
    ...devices['Desktop Chrome'],
  },
  webServer: {
    command: "sh -c 'mkdir -p ui-tests/test-results && exec go run ./internal/ui/e2e/testserver >>ui-tests/test-results/fixture.log 2>&1'",
    cwd: '..',
    url: 'http://127.0.0.1:19809/',
    reuseExistingServer: false,
    stdout: 'ignore',
    stderr: 'ignore',
    timeout: 120_000,
  },
});
```

- [ ] **Step 3: Add reset and browser-error helpers**

```ts
import { APIRequestContext, Page, expect } from '@playwright/test';

export async function resetScenario(request: APIRequestContext, scenario: string) {
  const response = await request.post(`/__test/reset?scenario=${encodeURIComponent(scenario)}`);
  expect(response.status()).toBe(204);
}

export function collectPageErrors(page: Page) {
  const errors: string[] = [];
  page.on('pageerror', error => errors.push(`pageerror: ${error.message}`));
  page.on('console', message => {
    if (message.type() === 'error') errors.push(`console: ${message.text()}`);
  });
  return errors;
}
```

- [ ] **Step 4: Write initial, empty, and error cases before installing Chromium**

```ts
import { test, expect } from '@playwright/test';
import { collectPageErrors, resetScenario } from './helpers';

test('core page settles with all primary rails', async ({ page, request }) => {
  await resetScenario(request, 'core');
  const errors = collectPageErrors(page);
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler · 1 agents');
  await expect(page.locator('#contexts .sess')).toHaveCount(18);
  await expect(page.locator('#rooms .sess')).toHaveCount(14);
  await expect(page.locator('#archived .sess')).toHaveCount(8);
  await expect(page.locator('#contexts')).not.toContainText('加载中…');
  expect(errors).toEqual([]);
});

test('empty state settles without a writable composer', async ({ page, request }) => {
  await resetScenario(request, 'empty');
  const errors = collectPageErrors(page);
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler · 0 agents');
  await expect(page.locator('#contexts')).toContainText('还没有对话');
  await expect(page.locator('#sendBtn')).toBeDisabled();
  expect(errors).toEqual([]);
});

test('scheduler failure is explicit and leaves a rendered shell', async ({ page, request }) => {
  await resetScenario(request, 'scheduler-error');
  const errors = collectPageErrors(page);
  await page.goto('/');
  await expect(page.locator('#schedLabel')).toHaveText('scheduler 不可达');
  await expect(page.locator('.app')).toBeVisible();
  await expect(page.locator('#contexts')).not.toContainText('加载中…');
  expect(errors).toEqual([]);
});
```

- [ ] **Step 5: Verify red, install Chromium, then verify green**

Run: `npm ci --prefix ui-tests && npm test --prefix ui-tests`

Expected before browser installation: FAIL stating that the Chromium executable is missing.

Run: `npm exec --prefix ui-tests playwright install chromium`

Run: `npm test --prefix ui-tests`

Expected: 3 passed.

- [ ] **Step 6: Commit the harness**

```bash
git add ui-tests
git commit -m "test: cover core UI page states in Chromium"
```

### Task 4: Live and archived agent browser contracts

**Files:**
- Modify: `ui-tests/core.spec.ts`

**Interfaces:**
- Consumes: live context title `Existing core question`, archived title `Archived core context`, agent names `live-codex` and `archived-kimi`.
- Produces: browser protection for the detail card, composer writability, transcript, chat POST, task poll, and fixed reply.

- [ ] **Step 1: Add failing interaction cases**

```ts
test('live participant details and chat remain usable', async ({ page, request }) => {
  await resetScenario(request, 'core');
  const errors = collectPageErrors(page);
  await page.goto('/');
  await page.locator('#contexts .sess', { hasText: 'Existing core question' }).click();
  await page.locator('#agentRows .agent-row', { hasText: 'live-codex' }).click();
  await expect(page.locator('#detailCard')).toContainText('deterministic live agent');
  await expect(page.locator('#detailCard')).toContainText('http://127.0.0.1:9802');
  await expect(page.locator('#detailCard')).toContainText('1.2.3-e2e');
  await expect(page.locator('#sendBtn')).toBeEnabled();
  await page.locator('#ta').fill('E2E ping');
  const chat = page.waitForRequest(r => r.url().endsWith('/api/agents/live-codex/chat') && r.method() === 'POST');
  await page.locator('#sendBtn').click();
  await chat;
  await expect(page.locator('#thread')).toContainText('E2E fixed reply');
  expect(errors).toEqual([]);
});

test('archived participant is detailed but read-only', async ({ page, request }) => {
  await resetScenario(request, 'core');
  const errors = collectPageErrors(page);
  await page.goto('/');
  let chatRequests = 0;
  page.on('request', r => { if (/\/api\/agents\/.*\/chat$/.test(r.url())) chatRequests++; });
  await page.locator('#archived .sess', { hasText: 'Archived core context' }).click();
  await expect(page.locator('#detailCard')).toContainText('archived-kimi');
  await expect(page.locator('#detailCard')).toContainText('已归档 · 只读');
  await expect(page.locator('#detailCard')).not.toContainText('选择一个 agent 查看详情');
  await expect(page.locator('#thread')).toContainText('Archived retained reply');
  await expect(page.locator('#ta')).toBeDisabled();
  await expect(page.locator('#sendBtn')).toBeDisabled();
  await page.waitForTimeout(100);
  expect(chatRequests).toBe(0);
  expect(errors).toEqual([]);
});
```

- [ ] **Step 2: Run the characterization cases**

Run: `npm test --prefix ui-tests -- --grep 'live participant|archived participant'`

Expected: 2 passed because these behaviors were repaired before this test project. Prove the archived test's sensitivity by temporarily changing `renderArchivedDetail` to render the unselected placeholder, rerun the archived case and observe FAIL, then restore `app.js`.

- [ ] **Step 3: Verify the archived fixture body and rerun green**

Ensure the archived history response contains:

```json
[{"turn":1,"speaker":"operator","userText":"Archived retained question","reply":"Archived retained reply","status":"completed","ts":"2026-07-22T02:00:01Z","durationMs":1000}]
```

Run: `npm test --prefix ui-tests -- --grep 'live participant|archived participant'`

Expected: 2 passed.

- [ ] **Step 4: Commit the interaction protection**

```bash
git add ui-tests/core.spec.ts internal/ui/e2e/testserver/fixture.go
git commit -m "test: protect live and archived agent interactions"
```

### Task 5: Desktop rail geometry and scroll protection

**Files:**
- Modify: `ui-tests/core.spec.ts`

**Interfaces:**
- Consumes: populated `#rooms`, `#contexts`, and `#archived` fixture lists.
- Produces: a 1440×900 geometry contract without screenshot comparison.

- [ ] **Step 1: Write the geometry test**

```ts
test('desktop left rail preserves conversations and independent scrolling', async ({ page, request }) => {
  await resetScenario(request, 'core');
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto('/');
  await expect(page.locator('#contexts .sess')).toHaveCount(18);
  const metrics = await page.locator('.rail.left').evaluate(rail => {
    const ids = ['rooms', 'contexts', 'archived'] as const;
    const boxes = Object.fromEntries(ids.map(id => {
      const el = rail.querySelector<HTMLElement>(`#${id}`)!;
      const rect = el.getBoundingClientRect();
      return [id, { top: rect.top, bottom: rect.bottom, height: rect.height, client: el.clientHeight, scroll: el.scrollHeight }];
    }));
    return { boxes, railBottom: rail.getBoundingClientRect().bottom };
  });
  expect(metrics.boxes.contexts.height).toBeGreaterThanOrEqual(120);
  expect(metrics.boxes.rooms.height).toBeLessThanOrEqual(900 * 0.26 + 1);
  expect(metrics.boxes.archived.height).toBeLessThanOrEqual(900 * 0.26 + 1);
  expect(metrics.boxes.rooms.bottom).toBeLessThanOrEqual(metrics.boxes.contexts.top + 1);
  expect(metrics.boxes.contexts.bottom).toBeLessThanOrEqual(metrics.boxes.archived.top + 1);
  expect(metrics.boxes.archived.bottom).toBeLessThanOrEqual(metrics.railBottom);
  expect(metrics.boxes.rooms.scroll).toBeGreaterThan(metrics.boxes.rooms.client);
  expect(metrics.boxes.contexts.scroll).toBeGreaterThan(metrics.boxes.contexts.client);
  expect(metrics.boxes.archived.scroll).toBeGreaterThan(metrics.boxes.archived.client);
});
```

- [ ] **Step 2: Run the test and verify current canonical CSS green**

Run: `npm test --prefix ui-tests -- --grep 'desktop left rail'`

Expected: PASS on canonical assets. To prove sensitivity, temporarily change `.sessions{...min-height:120px...}` to `min-height:0`, rerun and observe FAIL, then restore the file before continuing.

- [ ] **Step 3: Commit the layout contract**

```bash
git add ui-tests/core.spec.ts
git commit -m "test: protect left rail layout and scrolling"
```

### Task 6: Narrow-screen navigation, test first

**Files:**
- Modify: `ui-tests/core.spec.ts`
- Modify: `internal/ui/assets/index.html`
- Modify: `internal/ui/assets/app.js`
- Modify: `internal/ui/assets/app.css`
- Modify: `plugin/src/internal/ui/assets/index.html`
- Modify: `plugin/src/internal/ui/assets/app.js`
- Modify: `plugin/src/internal/ui/assets/app.css`

**Interfaces:**
- Produces buttons `#mobileLeft`, `#mobileChat`, and `#mobileDetail` in `.mob`.
- Produces `showMobileSurface(surface)` where `surface` is `left`, `center`, or `right`.

- [ ] **Step 1: Write the narrow-screen test**

```ts
test('narrow screen switches conversations, chat, and details without overflow', async ({ page, request }) => {
  await resetScenario(request, 'core');
  await page.setViewportSize({ width: 800, height: 900 });
  await page.goto('/');
  await expect(page.locator('.mob')).toBeVisible();
  await expect(page.locator('main.center')).toBeVisible();
  await page.locator('#mobileDetail').click();
  await expect(page.locator('.rail.right')).toBeVisible();
  await expect(page.locator('#detailCard')).toBeVisible();
  await expect(page.locator('#detailCard')).toContainText('live-codex');
  await page.locator('#mobileLeft').click();
  await expect(page.locator('.rail.left')).toBeVisible();
  await page.locator('#mobileChat').click();
  await expect(page.locator('main.center')).toBeVisible();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow).toBeLessThanOrEqual(1);
});
```

- [ ] **Step 2: Run and verify red**

Run: `npm test --prefix ui-tests -- --grep 'narrow screen'`

Expected: FAIL because `.mob` and its buttons are absent.

- [ ] **Step 3: Add the three-button navigation markup**

Insert after `.app` and before `.toast`:

```html
<nav class="mob" aria-label="移动端页面导航">
  <button id="mobileLeft" data-surface="left" aria-pressed="false">会话</button>
  <button id="mobileChat" data-surface="center" class="on" aria-pressed="true">聊天</button>
  <button id="mobileDetail" data-surface="right" aria-pressed="false">详情</button>
</nav>
```

- [ ] **Step 4: Wire exact surface-state changes**

Add before `init()` and call `showMobileSurface("center")` during initialization:

```js
function showMobileSurface(surface) {
  const left = document.querySelector(".rail.left");
  const center = document.querySelector("main.center");
  const right = document.querySelector(".rail.right");
  left.classList.toggle("show", surface === "left");
  right.classList.toggle("show", surface === "right");
  center.classList.toggle("hide", surface !== "center");
  center.classList.toggle("show", surface === "center");
  document.querySelectorAll(".mob button").forEach((button) => {
    const active = button.dataset.surface === surface;
    button.classList.toggle("on", active);
    button.setAttribute("aria-pressed", String(active));
  });
}
```

In `init()`, bind every `.mob button` click to `showMobileSurface(button.dataset.surface)`.

- [ ] **Step 5: Verify browser behavior and mirror parity**

Run: `npm test --prefix ui-tests -- --grep 'narrow screen'`

Expected: 1 passed.

Copy all three canonical assets to the plugin mirror, then run:

```bash
cp internal/ui/assets/app.css plugin/src/internal/ui/assets/app.css
cp internal/ui/assets/app.js plugin/src/internal/ui/assets/app.js
cp internal/ui/assets/index.html plugin/src/internal/ui/assets/index.html
go test ./internal/ui -run TestPluginAssetsMatchCanonical -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the responsive contract and fix**

```bash
git add ui-tests/core.spec.ts internal/ui/assets plugin/src/internal/ui/assets
git commit -m "fix: add narrow-screen UI navigation"
```

### Task 7: Local targets and CI gate

**Files:**
- Modify: `Makefile`
- Create: `.github/workflows/ui-test.yml`

**Interfaces:**
- Produces Make targets `test-ui-fast`, `ui-test-deps`, `test-ui-browser`, and `test-ui`.
- Produces stable Actions job name `UI / ui-regression`.

- [ ] **Step 1: Add Make targets**

```make
.PHONY: all build plugin plugin-src clean test test-ui-fast ui-test-deps test-ui-browser test-ui

test-ui-fast:
	GO111MODULE=on $(GO) test -count=1 ./internal/ui
	cd $(PLUGIN_SRC) && GO111MODULE=on $(GO) test -count=1 ./internal/ui

ui-test-deps:
	npm ci --prefix ui-tests
	npm exec --prefix ui-tests playwright install chromium

test-ui-browser:
	npm test --prefix ui-tests

test-ui: test-ui-fast test-ui-browser
```

- [ ] **Step 2: Verify the local target contract**

Run: `make test-ui-fast && make test-ui-browser`

Expected: root and plugin Go packages PASS and all Chromium cases pass.

- [ ] **Step 3: Add the PR and main workflow**

```yaml
name: UI

on:
  pull_request:
  push:
    branches: [main]

permissions:
  contents: read

jobs:
  ui-regression:
    name: ui-regression
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - uses: actions/setup-node@v4
        with:
          node-version: 24
          cache: npm
          cache-dependency-path: ui-tests/package-lock.json
      - run: npm ci --prefix ui-tests
      - run: make test-ui-fast
      - run: npm exec --prefix ui-tests playwright install --with-deps chromium
      - run: make test-ui-browser
      - if: failure()
        uses: actions/upload-artifact@v4
        with:
          name: ui-regression-failure
          path: |
            ui-tests/test-results
            ui-tests/playwright-report
          retention-days: 7
          if-no-files-found: ignore
```

The workflow check displayed by GitHub is `UI / ui-regression` because the workflow is named `UI` and the job is named `ui-regression`.

- [ ] **Step 4: Validate workflow shape and commit**

Run: `git diff --check && make test-ui`

Expected: no whitespace errors and all UI tests pass.

```bash
git add Makefile .github/workflows/ui-test.yml
git commit -m "ci: gate pull requests on UI regressions"
```

### Task 8: Full verification, PR, CI, and required check

**Files:**
- Modify only files required by failures proven in this task.

**Interfaces:**
- Consumes all previous targets and commits.
- Produces a green PR and required `UI / ui-regression` branch check.

- [ ] **Step 1: Run complete local verification**

Run: `make test-ui`

Expected: all fast UI and Chromium tests pass.

Run: `make test`

Expected: all repository Go race tests pass.

Run: `git diff --check && git status --short`

Expected: no whitespace errors and no uncommitted files outside this branch's intentional changes.

- [ ] **Step 2: Review the complete branch diff**

Run: `git diff main...HEAD --stat && git diff main...HEAD -- . ':(exclude)ui-tests/package-lock.json'`

Expected: only the design/plan, UI tests, fixture, synchronized assets, Make targets, and workflow are present.

- [ ] **Step 3: Commit any verification-only correction**

If Step 1 required a correction, rerun the failing command and commit only that correction with a message naming the proven failure. If no correction was required, create no empty commit.

- [ ] **Step 4: Push and open the PR**

```bash
GIT_SSH_COMMAND="ssh -o Hostname=ssh.github.com -o Port=443" git push -u origin codex/ui-test-coverage
gh pr create --base main --head codex/ui-test-coverage --title "test: protect core UI experience" --body-file docs/superpowers/specs/2026-07-23-ui-regression-test-design.md
```

Expected: GitHub returns a PR URL and starts `UI / ui-regression`.

- [ ] **Step 5: Confirm CI before merge**

Run: `gh pr checks --watch <PR-number>`

Expected: `UI / ui-regression` and every other required check report PASS.

- [ ] **Step 6: Add the required check without discarding existing checks**

First read the current rule:

```bash
gh api repos/wu8685/ahsir/branches/main/protection/required_status_checks > /tmp/ahsir-required-checks.json
jq '.contexts + ["UI / ui-regression"] | unique' /tmp/ahsir-required-checks.json
```

Use the resulting unique context list when updating branch protection through GitHub. Re-read the endpoint afterward and verify every prior context remains and `UI / ui-regression` is present. If the repository has no branch-protection rule yet, report that fact before creating one because creation also requires review and enforcement policy choices outside this UI-test scope.

- [ ] **Step 7: Merge only after checks and review are clean**

Run: `gh pr merge <PR-number> --squash --delete-branch`

Expected: the PR reports merged and `main` contains the UI regression workflow.
