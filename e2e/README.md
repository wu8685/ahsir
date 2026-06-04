# `e2e/` — Real-LLM Integration Tests

Top-to-bottom integration tests that spawn the real `ahsir` scheduler subprocess against real `claude` CLIs and a real LLM provider (DeepSeek by default). Gated behind the `e2e` build tag so the default `go test ./...` pipeline never runs them.

This README is for **future contributors (humans or agents) adding new test cases**. If you just want to run the existing suite, jump to [Run](#run).

## Layout

```
e2e/
├── README.md                 ← you are here
├── framework_test.go         ← fixture + helpers + agent-card YAML templates
└── delegation_test.go        ← TestStudentDelegatesToTeacher_E2E (the first case)
```

Both files use `package e2e` (internal test package) + `//go:build e2e` at the top. Go is happy with a directory containing only build-tagged `_test.go` files — no separate `package e2e` non-test file is needed.

## Run

```bash
# From repo root: build the binaries the framework needs
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent

# Run all cases
AHSIR_E2E_CLAUDE=1 MODEL_API_KEY=<your-deepseek-key> \
  go test -tags=e2e -timeout=5m -v ./e2e/

# Run one case
AHSIR_E2E_CLAUDE=1 MODEL_API_KEY=<your-deepseek-key> \
  go test -tags=e2e -timeout=5m -v -run TestStudentDelegatesToTeacher_E2E ./e2e/
```

Without `AHSIR_E2E_CLAUDE=1` (or with binaries unbuilt / `claude` not on PATH / `MODEL_API_KEY` unset), tests skip cleanly so CI can safely run `go test -tags=e2e ./...` with no gating logic.

---

## Writing a new E2E case — the 3-step pattern

Every E2E case follows the same shape:

```go
//go:build e2e

package e2e

import (
    "strings"
    "testing"
)

func TestMyNewScenario_E2E(t *testing.T) {
    // ① Spin up the whole stack (skip if prereqs missing, hermetic temp config).
    fix := setupE2E(t)

    // ② Drive it with one or more sendMessage calls.
    reply, err := fix.sendMessage(
        fix.studentPort,      // or fix.teacherPort to hit teacher directly
        "msg-id-unique",      // messageId — must differ across calls
        "my-conv-1",          // contextId — same across calls = session reuse
        "What does X mean? Answer in one sentence.",
    )
    if err != nil {
        t.Fatalf("send: %v\n--- log ---\n%s", err, fix.schedulerLog())
    }

    // ③ Assert: content of reply, AND structural markers in the scheduler log.
    //    Content alone is rarely enough — see "Why log markers matter" below.
    if !strings.Contains(reply, "expected substring") {
        t.Errorf("reply missing expected substring: %q", reply)
    }
    logs := fix.schedulerLog()
    if !strings.Contains(logs, "[student → teacher]") {
        t.Errorf("expected delegation marker, got:\n%s", logs)
    }
}
```

Save as `e2e/<scenario>_test.go` (e.g. `session_reuse_test.go`, `kill_recovery_test.go`). One file per scenario keeps assertions focused and lets future agents grep by behaviour.

## What the framework gives you

### `setupE2E(t) *e2eFixture`

Idempotently sets up the whole stack:

1. **Prereq gates** — skips with a hint if `AHSIR_E2E_CLAUDE` / `MODEL_API_KEY` / `claude` / `bin/ahsir(-agent)` are missing.
2. **Hermetic config** — writes a temp `ahsir.yaml` + two agent cards (teacher, student) into `t.TempDir()`. **No filesystem access** (`filesystem.enabled: false`) so the test never depends on host-specific paths like `/Users/wuke/...`.
3. **Random free ports** — picks three unused ports for registry + teacher + student so multiple e2e runs (or a manually-running scheduler) don't collide.
4. **Subprocess scheduler** — spawns `bin/ahsir start <temp-config>` in **its own process group** (`SysProcAttr.Setpgid: true`). Critical: without the process group, `cleanup` only kills the scheduler — children (ahsir-agent → claude) survive holding inherited stdout/stderr pipes, hanging `cmd.Wait` indefinitely.
5. **`t.Cleanup` wired** — on test exit (success, failure, or panic), kills the **whole process group** (`syscall.Kill(-pid, SIGKILL)`) with a bounded 5s `Wait`.
6. **Ready-wait** — polls both agent ports until they accept TCP (30s budget) before returning.

### `fix.sendMessage(port, messageId, contextId, text) (string, error)`

POSTs an A2A `message/send` JSON-RPC request and returns the **last `agent`-role text part** from `result.history` — i.e. what a user would see as the final reply.

- Bound by a 5-minute context (LLM can be slow under load).
- Returns a descriptive error on transport failure, JSON-RPC error, or empty history.
- `messageId` MUST differ between calls within one conversation — the A2A SDK may dedupe and replay the prior task otherwise.
- `contextId` controls session reuse: same value across calls → pool keys on the same entry → reuse / resume / etc.

### `fix.schedulerLog() string`

Returns the cumulative stdout+stderr captured from the scheduler subprocess. Since the scheduler tees agent stdout into its own, this includes **all wrapper-emitted log lines from every agent**. Use for structural assertions.

### Fixture fields you can read directly

| Field | Use |
|---|---|
| `fix.teacherPort` | POST direct to teacher (skip delegation) |
| `fix.studentPort` | POST to student (triggers delegation if prompt asks for it) |
| `fix.registryPort` | Hit scheduler registry / gateway endpoints |
| `fix.repoRoot` | Absolute path to repo root (rarely needed; useful for shelling out) |

The `cmd` / `cancel` / `logBuf` fields exist but are private to the fixture — don't touch them from a case file.

## Why log markers matter (don't skip them)

The headline failure mode of an E2E test that asserts ONLY on reply content is **false-positive on a smart model**: DeepSeek often knows the answer to "what is a goroutine" and a misbehaving `student` that ignores its `A2A_CALL` instruction can still produce a passing reply. The multi-agent path you're trying to test never actually fires.

Always pair content assertions with log-marker assertions. The wrapper emits these markers on the real cross-agent path:

| Marker | Means |
|---|---|
| `[student] receive: contextID=X` | Student's A2A server got the request |
| `claude session: started pid=N cmd=claude args=[...]` | A fresh `claude` subprocess was spawned (no reuse) |
| `claude session: started pid=N cmd=claude args=[... --resume=<id>]` | Pool resumed an evicted/dead session (cross-restart, SIGKILL recovery, idle-evict) |
| `[student → teacher] A2A_CALL: task="..."` | Student emitted a delegation, executor parsed it |
| `[teacher] receive: contextID=X` | Teacher's A2A server got the delegated request (contextID propagated → X matches student's) |
| `[student ← teacher] reply: took=Ts bytes=N preview="..."` | Teacher responded, executor relayed to student |

Pick the subset relevant to your scenario. For session reuse you want **absence** of a second `claude session: started`. For resume you want **presence** of `--resume=` in the args.

## Common patterns

### Hit teacher directly (no delegation involved)

```go
reply, err := fix.sendMessage(fix.teacherPort, "msg-1", "conv-direct", "What is HTTP?")
// teacher answers, no [student → teacher] in log
```

### Multi-turn session reuse within one scheduler

```go
fix := setupE2E(t)

// Turn 1: establish state
_, err := fix.sendMessage(fix.teacherPort, "m1", "reuse-conv", "Remember the codeword: alpha-42.")
if err != nil { t.Fatal(err) }

// Turn 2: same contextId → pool MUST reuse claude
reply, err := fix.sendMessage(fix.teacherPort, "m2", "reuse-conv", "What codeword did I tell you?")
if err != nil { t.Fatal(err) }

if !strings.Contains(reply, "alpha-42") {
    t.Errorf("teacher forgot the codeword: %q", reply)
}

// Exactly ONE "claude session: started" for teacher → process was reused.
logs := fix.schedulerLog()
if n := strings.Count(logs, "claude session: started"); n != 1 {
    t.Errorf("expected 1 claude spawn, got %d:\n%s", n, logs)
}
```

### SIGKILL self-heal (HA path)

```go
fix := setupE2E(t)

// Turn 1: spawn teacher's claude.
_, _ = fix.sendMessage(fix.teacherPort, "m1", "ha-conv", "Remember beta-77.")

// Extract teacher's claude pid from the log and SIGKILL it.
pid := extractFirstPid(t, fix.schedulerLog())
if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
    t.Fatalf("kill claude: %v", err)
}
time.Sleep(500 * time.Millisecond) // let the wrapper's reader see EOF

// Turn 2: pool must detect zombie via IsHealthy, recreate with --resume.
reply, err := fix.sendMessage(fix.teacherPort, "m2", "ha-conv", "What codeword?")
if err != nil { t.Fatal(err) }
if !strings.Contains(reply, "beta-77") {
    t.Errorf("resume didn't recover memory: %q", reply)
}

logs := fix.schedulerLog()
if !strings.Contains(logs, "--resume=") {
    t.Errorf("expected --resume= in log after kill, got:\n%s", logs)
}
```

(You'd need a small `extractFirstPid` helper — regex `claude session: started pid=(\d+)`. Add it to your test file; it's specific to this scenario.)

### Direct gateway / registry checks (no LLM)

```go
fix := setupE2E(t)

resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/agents", fix.registryPort))
// assert resp body lists "teacher" and "student"
```

## Conventions

- **File naming**: `<scenario>_test.go` (lowercase, underscore-separated). Match the assertion theme: `session_reuse_test.go`, `kill_recovery_test.go`, `cross_restart_test.go`.
- **Test naming**: `Test<Scenario>_E2E` — the `_E2E` suffix lets users grep / filter via `-run E2E`.
- **One scenario per file**: easier to grep, easier to skip individually, easier to read in isolation.
- **`//go:build e2e` MUST be on line 1** — Go ignores misplaced build tags silently, and the test will just disappear without error.
- **Skip messages: actionable**. If a precondition fails, say what to do (look at `setupE2E` for examples — e.g. "from repo root run: `go build ...`").
- **Don't reuse `messageId` within one conversation** — A2A SDK may dedupe.
- **Pair every content assertion with a log-marker assertion** when the scenario involves multi-agent path or session lifecycle.

## Adding new framework helpers

If your scenario needs a helper used by more than one case (e.g. `extractFirstPid`, `restartScheduler`, `sendStreamingMessage`), add it to `framework_test.go`. Keep `framework_test.go` reusable, keep case files thin.

If a helper is single-use, keep it in the case file.

## Debugging a failing case

1. **Add `-v`** — gives you `t.Logf` output and per-case timing.
2. **Print the scheduler log on failure** — every assertion in the framework already does this (`fix.schedulerLog()` appended to error messages). Copy that pattern in your own assertions.
3. **Bump the timeout** — `go test -timeout=10m ...` if the LLM is slow today.
4. **Run the scheduler manually** with the same agent cards (copy from `framework_test.go`'s YAML constants) to see what the scheduler log looks like outside the test harness.
5. **Check the cleanup branch** — if `t.Cleanup` says "scheduler subprocess did not exit within 5s after group kill", a child process is wedged. Usually a `claude` waiting on stdout — confirm `Setpgid: true` is set (it should be; framework does this).

## Pitfalls observed during the framework's own development

- **Don't `cmd.Process.Kill()` alone**: kills only the scheduler, leaves ahsir-agent + claude alive holding stdout pipe FDs, `cmd.Wait` hangs forever. Use `syscall.Kill(-pid, SIGKILL)` (negative pid = process group).
- **Don't use `bytes.Buffer` for `cmd.Stdout`**: `exec.Cmd`'s output goroutine writes to it concurrently with your assertion reads. Use a `sync.Mutex`-wrapped buffer (already provided as `syncBuffer`).
- **Don't hard-code `/Users/wuke/...` in agent cards**: the production `example/multi-agent/workspaces/teacher/agent-card.yaml` does this for the brain-spark fixture; copying that card verbatim into the e2e fixture breaks on CI. The framework's inlined cards (`teacherCardYAML` / `studentCardYAML`) deliberately have `filesystem.enabled: false`.
- **Don't share `contextId` across unrelated cases** if running in parallel: even though each test has its own scheduler subprocess (so pools don't actually collide), shared IDs make logs hard to read. Use `t.Name()` or a per-case prefix.

## TL;DR for the next agent

```go
//go:build e2e

package e2e

import (
    "strings"
    "testing"
)

func TestMyScenario_E2E(t *testing.T) {
    fix := setupE2E(t)
    reply, err := fix.sendMessage(fix.studentPort, "msg-1", "my-conv-1", "<your prompt>")
    if err != nil {
        t.Fatalf("send: %v\nlog:\n%s", err, fix.schedulerLog())
    }
    // Content check:
    if !strings.Contains(reply, "<expected>") {
        t.Errorf("reply: %q", reply)
    }
    // Log marker check — required for any multi-agent or session-lifecycle scenario:
    if !strings.Contains(fix.schedulerLog(), "<marker>") {
        t.Errorf("missing marker in log")
    }
}
```

Drop it at `e2e/my_scenario_test.go`. Run:

```bash
AHSIR_E2E_CLAUDE=1 MODEL_API_KEY=<key> \
  go test -tags=e2e -timeout=5m -v -run TestMyScenario_E2E ./e2e/
```

Done.
