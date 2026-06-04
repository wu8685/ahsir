//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// TestSessionReusedAcrossDelegations_E2E verifies that when two A2A
// requests share the same contextId and each triggers a student→teacher
// delegation, BOTH agents' claude subprocesses are reused — not just the
// student's.
//
// This is the interesting multi-agent variant of session reuse:
//
//   - Single-agent reuse (curl teacher twice) only needs SessionPool's
//     hot-path hit to work.
//   - Multi-agent reuse additionally requires contextID propagation
//     through the A2A_CALL boundary: student's task.ContextID must flow
//     to teacher so teacher's pool sees the same key on the second
//     delegation and returns its cached session instead of spawning a
//     fresh claude.
//
// Headline assertion: exactly 2 `claude session: started` log lines —
// one per agent — across two complete delegation rounds. Each agent's
// process was created once on its first invocation and reused on the
// second. Any other count means a pool failed to reuse:
//
//   - 0 or 1: setup failed (agents never started or test stopped early).
//   - 3: one agent reused, the other didn't (contextID propagation bug
//        or per-request ctx leakage killing the process between turns).
//   - 4: neither reused — pool entirely broken on this path.
//
// Cross-checks:
//
//   - No `--resume=` in the log: pure in-process reuse, not eviction
//     recovery. (Eviction recovery is tested separately by the SIGKILL
//     scenario in another file.)
//   - `[teacher] receive: contextID=reuse-conv-multi` — teacher's pool
//     key matches the user-supplied contextID, proving propagation.
//   - 2× `[student → teacher] A2A_CALL` — student really did dispatch
//     each turn rather than answering itself from in-process memory.
func TestSessionReusedAcrossDelegations_E2E(t *testing.T) {
	fix := setupE2E(t)

	const contextID = "reuse-conv-multi"

	// Turn 1 — delegation about goroutines.
	reply1, err := fix.sendMessage(
		fix.studentPort,
		"msg-reuse-1",
		contextID,
		"Please delegate to the teacher: what is a goroutine? Answer in one sentence.",
	)
	if err != nil {
		t.Fatalf("turn 1: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	t.Logf("turn 1 reply: %q", reply1)

	// Turn 2 — same contextId, different topic so the student can't shortcut
	// by reusing turn 1's answer.
	reply2, err := fix.sendMessage(
		fix.studentPort,
		"msg-reuse-2",
		contextID,
		"Now please delegate to the teacher: what is a channel in Go? Answer in one sentence.",
	)
	if err != nil {
		t.Fatalf("turn 2: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	t.Logf("turn 2 reply: %q", reply2)

	// Content sanity — each reply should land on its own topic.
	if !strings.Contains(strings.ToLower(reply1), "goroutine") {
		t.Errorf("turn 1 reply missing 'goroutine': %q", reply1)
	}
	if !strings.Contains(strings.ToLower(reply2), "channel") {
		t.Errorf("turn 2 reply missing 'channel': %q", reply2)
	}

	logs := fix.schedulerLog()

	// Headline: exactly 2 claude spawns total. Anything else means a pool
	// failed to reuse on one side or the other.
	if got := strings.Count(logs, "claude session: started"); got != 2 {
		t.Errorf("expected exactly 2 'claude session: started' lines (1 per agent, both reused across both turns), got %d:\n--- log ---\n%s", got, logs)
	}

	// Pure in-process reuse — no eviction recovery should have fired.
	if strings.Contains(logs, "--resume=") {
		t.Errorf("found --resume= in log; a process was evicted and recreated rather than reused:\n--- log ---\n%s", logs)
	}

	// Per-turn structural markers. Each turn produces:
	//   1× [student] receive   (the user's curl reaches student)
	//   1× [student → teacher] A2A_CALL  (student dispatched)
	//   1× [teacher] receive   (teacher's A2A server got the delegated msg)
	//   1× [student ← teacher] reply
	// → 2 turns ⇒ 2 of each.
	if got := strings.Count(logs, "[student] receive"); got != 2 {
		t.Errorf("expected 2 [student] receive lines, got %d:\n%s", got, logs)
	}
	if got := strings.Count(logs, "[student → teacher] A2A_CALL"); got != 2 {
		t.Errorf("expected 2 A2A_CALL dispatches, got %d:\n%s", got, logs)
	}
	if got := strings.Count(logs, "[teacher] receive"); got != 2 {
		t.Errorf("expected 2 [teacher] receive lines, got %d:\n%s", got, logs)
	}
	if got := strings.Count(logs, "[student ← teacher] reply"); got != 2 {
		t.Errorf("expected 2 reply lines, got %d:\n%s", got, logs)
	}

	// contextID propagation: teacher must see the SAME contextID the
	// student saw, not a fresh auto-generated one. Without this, teacher's
	// pool would key on different contextIDs each turn and spawn a second
	// claude (which the headline assertion would already catch — this
	// marker just makes the failure mode explicit).
	if !strings.Contains(logs, "[teacher] receive: contextID="+contextID) {
		t.Errorf("teacher's receive log missing contextID=%q — contextID propagation broken:\n%s", contextID, logs)
	}
}
