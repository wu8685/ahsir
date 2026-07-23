# UI Regression Test Design

**Date:** 2026-07-23
**Status:** Approved

## 1. Context

The ahsir web console is an embedded single-page application served by
`internal/ui`. The repository also ships a mirrored copy under
`plugin/src/internal/ui`. Current protection is split across Go HTTP tests,
static CSS assertions, and one Node-based fake-DOM regression test. There is no
pull-request test workflow.

This leaves two demonstrated gaps:

1. fake-DOM tests can validate state transitions but cannot prove real browser
   layout, visibility, scrolling, responsive behavior, or JavaScript execution;
2. the root and plugin copies can drift silently. At design time, `app.css` and
   `index.html` already differ between the two trees.

The test system must protect both functionality and the minimum usable UI
experience without turning routine CSS changes into flaky screenshot churn.

## 2. Goals

The first phase must:

- cover the console's critical live-agent and archived-agent paths in a real
  Chromium browser;
- verify the left rail remains usable when rooms, conversations, and archived
  conversations are all populated;
- verify the desktop three-column layout and the existing narrow-screen
  navigation contract;
- cover loading, empty, and scheduler-error states without uncaught JavaScript
  errors or permanent loading indicators;
- keep fast state/API tests available through ordinary Go and Node tooling;
- reject any byte-level drift between the canonical root UI assets and the
  plugin-shipped UI assets;
- run automatically for every pull request and for pushes to `main`;
- emit screenshots, Playwright traces, and fixture logs only when browser tests
  fail.

## 3. Non-goals

The first phase will not:

- exhaustively test roundtable creation, the New Agent form, Markdown/tool-call
  rendering, drag-and-drop, clipboard, notifications, or every theme detail;
- use full-page golden screenshots as pass/fail assertions;
- run Firefox or WebKit;
- depend on a live ahsir scheduler, an LLM provider, API keys, user workspaces,
  or existing local transcripts;
- change production UI behavior except for the minimum fixes exposed by the
  agreed core cases: synchronizing the already-present root/plugin asset drift
  and completing the narrow-screen navigation whose CSS contract exists but
  whose controls and JavaScript wiring are currently absent.

Future UI features and fixed UI bugs must add focused cases to the nearest fast
or browser layer rather than expanding one monolithic end-to-end scenario.

## 4. Test architecture

The design uses two complementary layers.

### 4.1 Fast contract layer

The fast layer remains part of the Go test surface:

- existing `internal/ui/server_test.go` tests continue to verify proxy and
  aggregation behavior;
- the existing Node fake-DOM suite continues to verify JavaScript state paths
  such as live, archived, and unavailable participant selection;
- a new Go parity test compares every regular file under
  `internal/ui/assets/` with the corresponding file under
  `plugin/src/internal/ui/assets/`, and fails on missing, extra, or different
  files;
- the parity test locates the full repository from either the canonical package
  or a `make plugin-src`-copied package; a genuinely standalone plugin source
  module skips with an explicit reason because its canonical tree is absent;
- every root UI subpackage, including `internal/ui/e2e/testserver`, and the
  plugin UI package are executed by the fast UI target.

`internal/ui/assets/` is the canonical source. `plugin/src/internal/ui/assets/`
is a release mirror. The parity test is the enforcement mechanism; it does not
silently copy or rewrite files.

### 4.2 Real-browser layer

Playwright drives pinned Chromium against the actual Go `internal/ui` handler.
The browser must receive the same embedded `index.html`, `app.css`, and `app.js`
that production serves.

A test-only Go executable under `internal/ui/e2e/testserver/` owns two in-memory
servers:

1. a mock scheduler that implements only the API routes required by the core
   scenarios;
2. the real ahsir UI handler configured to proxy to that mock scheduler.

The executable also exposes a test-only fixture-control endpoint outside the
production UI handler. Tests use it to reset fixture state and select a named
scenario before reloading the page. The endpoint exists only in the test
executable and cannot be enabled in a production `ahsir` binary.

The Playwright suite runs serially against this shared fixture server. Every
test resets the fixture first, so order does not affect results.

## 5. Fixture model

The fixture server provides deterministic scenarios:

### `core`

- one online live agent with endpoint, version, description, skills, and config;
- one archived agent with retained context history;
- enough rooms, live contexts, and archived contexts to require independent
  scrolling in the left rail;
- one completed live transcript and one archived transcript;
- deterministic invocation records for the selected context;
- retained archived history only at
  `GET /agents/archived-kimi/history/ctx-archived-01`;
- a successful chat submission whose JSON body is exactly `message: "E2E ping"`,
  `async: true`, `speaker: "console"`, and `contextId: "ctx-live-01"`; malformed
  or different payloads return HTTP 400 before the accepted/completed sequence
  yields the fixed reply.

### `empty`

- no agents, rooms, contexts, archived agents, or invocations;
- every API request succeeds with the correct empty JSON shape.

### `scheduler-error`

- the scheduler-facing endpoints return a deterministic HTTP 503 response;
- the UI's static assets and fixture-control endpoint remain available.

Fixture timestamps, titles, IDs, and replies are constants. No test relies on
the wall clock beyond asserting that rendered content is non-empty.

## 6. Core browser cases

### 6.1 Initial load

For the `core` scenario:

- the document reaches its ready state;
- the loading placeholders disappear;
- the scheduler label reports the expected live-agent count;
- rooms, contexts, and archived sections all contain rows;
- no uncaught page error or console error is emitted.

### 6.2 Live agent interaction

After opening the live context and selecting its participant:

- the detail card shows the live agent's name, endpoint, version, and
  description;
- the composer and send button are enabled;
- submitting a fixed message calls the live-agent chat route with the exact
  deterministic JSON body from Section 5;
- the accepted/completed sequence renders the fixed reply;
- the page emits no uncaught error.

### 6.3 Archived agent interaction

After opening the archived context and selecting its participant:

- the detail card contains `已归档 · 只读` and does not contain the unselected
  placeholder;
- the browser requests the exact retained-history path and the archived
  transcript remains visible;
- the agent select has no active send target;
- the composer is read-only or disabled and the send button is disabled;
- no chat request is emitted for the archived agent.

### 6.4 Left-rail usability

At a desktop viewport of 1440×900:

- the rooms, contexts, and archived containers are visible;
- the contexts container has a rendered height of at least 120 pixels;
- each populated auxiliary list stays within its configured height bound;
- the three containers do not visually overlap;
- overflowing lists expose scrollable content rather than expanding beyond the
  viewport or starving the conversations list.

Assertions use computed geometry and scroll metrics, not screenshots.

### 6.5 Responsive navigation

At a narrow viewport of 800×900:

- the mobile navigation is visible;
- the initial center/chat surface, textarea, and send button are usable and
  remain above the bottom navigation rather than being clipped or overlapped;
- switching to the details surface reveals the selected participant and detail
  card without horizontal page overflow;
- switching back restores the chat surface.

The minimum production fix for this case is a three-button bottom navigation
for conversations, chat, and details. It only switches which existing rail or
center surface is visible; it does not redesign those surfaces or add new data
behavior.

### 6.6 Empty and scheduler-error states

For `empty`:

- counts settle at zero;
- rooms, archived rows, conversation rows, and agent options remain empty;
- the composer stays disabled and read-only when no live target exists;
- no loading placeholder remains indefinitely;
- no uncaught page error is emitted.

For `scheduler-error`:

- the scheduler status clearly reports that it is unreachable;
- the page remains interactive enough to render its empty shell;
- no uncaught page error is emitted.

## 7. Assertion and artifact policy

Tests assert semantic text, enabled/disabled state, network effects, visibility,
computed geometry, and scrollability. They must prefer stable IDs and existing
DOM contracts over CSS ancestry or positional selectors.

An automatic Playwright fixture installs console and page-error listeners plus
an HTTP(S) route guard before every test body. Requests to the configured
`baseURL` origin continue normally; every other HTTP(S) request is recorded,
blocked before network I/O, and fails diagnostics. `data:` and `blob:` URLs are
outside that guard. Expected scheduler-error responses use exact URL/status
allowlist entries rather than broad error suppression.

The suite does not compare golden screenshots. On failure, Playwright retains:

- a screenshot;
- a trace archive;
- the browser console/page-error log;
- the fixture server log.

Artifacts are uploaded by CI with a bounded retention period. Successful runs
do not upload browser artifacts.

## 8. Local commands

The Makefile will expose:

- `make test-ui-fast`: run all root UI Go subpackages and the plugin UI Go
  package;
- `make ui-test-deps`: install the pinned Node dependencies and Chromium needed
  by the browser suite;
- `make test-ui-browser`: run the Playwright core suite, assuming dependencies
  are installed;
- `make test-ui`: run the fast and browser layers together.

The existing repository-wide `make test` remains the authoritative Go race-test
target. UI work must run both `make test-ui` and `make test` before completion.

## 9. CI gate

`.github/workflows/ui-test.yml` will run on pull requests and pushes to `main`
with read-only repository permissions. It will:

1. check out the repository;
2. install pinned Go and Node versions;
3. restore Go and npm caches;
4. run the fast UI target;
5. install pinned Chromium with its Linux dependencies;
6. run the browser suite;
7. upload failure-only artifacts.

The stable check name will be `UI / ui-regression`. After the workflow has run
successfully once, `main` branch protection will require this check. Directly
changing branch protection is an explicit deployment step after the workflow
exists; it is not hidden inside test code.

## 10. Dependency and determinism constraints

- Playwright dependencies live under `ui-tests/` with a committed lock file;
- only Chromium is installed;
- browser tests use one worker and retry once only in CI, retaining the first
  failure trace;
- the suite must not read environment secrets or make external network calls;
  its automatic HTTP(S) route guard must block and report every origin other
  than the configured fixture origin before navigation or application code;
- the fixture binds to `127.0.0.1:19809`; Playwright owns its lifecycle with
  `reuseExistingServer: false` and reports a clear startup error if the port is
  already occupied;
- production Go modules gain no browser-automation dependency;
- the root/plugin asset parity test must run without Node or Chromium.

## 11. Delivery sequence and acceptance

Implementation follows TDD:

1. add the asset parity test and demonstrate that it fails against the current
   root/plugin drift;
2. synchronize the plugin assets and make the parity test pass;
3. add each Playwright case against the fixture, observe its initial failure for
   the missing fixture/test behavior, then add the minimum fixture support;
4. add the narrow-screen case, observe that navigation controls are absent, and
   implement only the three-surface switch described in Section 6.5;
5. add Make targets and verify local commands;
6. add CI and verify the workflow on the feature pull request;
7. after a successful workflow run, configure `UI / ui-regression` as a required
   `main` check.

The work is accepted when:

- all cases in Section 6 pass locally in pinned Chromium;
- root and plugin UI assets are byte-identical;
- `make test-ui` and `make test` exit successfully;
- the feature PR shows a successful `UI / ui-regression` check;
- deliberate local mutations to an asset mirror, archived-detail behavior, and
  the conversations min-height cause the parity, archived-participant, and
  left-rail geometry tests to fail, respectively.
