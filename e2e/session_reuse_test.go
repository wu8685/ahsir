//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSessionReusedAcrossDelegations_E2E covers two interlocking guarantees
// in one run, against a real LLM:
//
// Part 1 — in-process reuse across multi-agent delegations.
// Two A2A requests share the same contextId and each triggers a
// student→teacher delegation. Both agents' claude subprocesses must be
// reused. This exercises:
//   - SessionPool's hot-path hit on the second LookupOrCreate per agent.
//   - contextID propagation through the A2A_CALL boundary: student's
//     task.ContextID flows to teacher so teacher's pool keys on the
//     same value both turns.
//
// Part 2 — transparent recovery after the teacher's claude is killed.
// The test then SIGKILLs the teacher's claude process externally and
// fires a third request on the same contextId. Expected outcome:
//   - SessionPool's IsHealthy probe detects the zombie session.
//   - Pool recreates teacher's claude with `--resume=<prior sessionId>`.
//   - Teacher remembers the codeword established in turn 1 (memory
//     preserved across the kill via claude's own jsonl store).
//   - Student's claude is unaffected — same pid throughout.
//
// All three turns share the same contextId. Counts at each phase form
// a precise signature: 2 spawns after turn 2 → 3 spawns after turn 3,
// with exactly one carrying --resume=.
func TestSessionReusedAcrossDelegations_E2E(t *testing.T) {
	fix := setupE2E(t)

	const (
		contextID = "reuse-recovery-conv"
		codeword  = "sapphire-15"
	)

	// === Turn 1 — establish memory in teacher ===
	reply1, err := fix.sendMessage(
		fix.studentPort,
		"msg-rr-1",
		contextID,
		fmt.Sprintf("Please delegate to the teacher: remember the codeword %q. Reply briefly to confirm.", codeword),
	)
	if err != nil {
		t.Fatalf("turn 1: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	t.Logf("turn 1 reply: %q", reply1)

	// After turn 1: exactly 2 claude processes spawned, neither resumed.
	starts := parseStartLines(t, fix.schedulerLog())
	if len(starts) != 2 {
		t.Fatalf("turn 1: expected 2 claude session starts (student + teacher), got %d:\n%+v", len(starts), starts)
	}
	studentPid := starts[0].pid
	teacherPid := starts[1].pid
	for i, s := range starts {
		if s.hasResume {
			t.Errorf("turn 1: starts[%d] unexpectedly has --resume=%s — should be a fresh spawn", i, s.resumeID)
		}
	}

	// === Turn 2 — verify session reuse: same contextID, same processes ===
	reply2, err := fix.sendMessage(
		fix.studentPort,
		"msg-rr-2",
		contextID,
		"Please delegate to the teacher: what codeword did I just tell you? Reply with just the codeword.",
	)
	if err != nil {
		t.Fatalf("turn 2: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	t.Logf("turn 2 reply: %q", reply2)

	// Both agents reused: no new start lines, total still 2.
	starts = parseStartLines(t, fix.schedulerLog())
	if len(starts) != 2 {
		t.Fatalf("turn 2: expected NO new spawns (in-process reuse), still 2 total; got %d:\n%+v", len(starts), starts)
	}
	// Teacher must have remembered the codeword — proves the SAME claude
	// process served turn 2, not a freshly spawned one with empty memory.
	if !strings.Contains(strings.ToLower(reply2), codeword) {
		t.Errorf("turn 2 reply missing codeword %q (in-process memory broken): %q", codeword, reply2)
	}

	// === Kill teacher's claude externally ===
	t.Logf("SIGKILL teacher claude pid=%d", teacherPid)
	if err := syscall.Kill(teacherPid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill teacher claude (pid=%d): %v", teacherPid, err)
	}
	// Give teacher's wrapper reader goroutine time to observe stdout EOF
	// and transition the session to stateEvicted. Pool's IsHealthy probe
	// reads state directly, so once the reader processes EOF the next
	// LookupOrCreate will detect the zombie and trigger recreate-with-resume.
	// 500ms is generous; the transition is a few syscalls.
	time.Sleep(500 * time.Millisecond)

	// === Turn 3 — same contextID, expect teacher recovery + memory ===
	// The prompt explicitly forces delegation even though student's claude
	// itself remembers turn 1+2 (it saw the teacher's prior reply). Without
	// the explicit ask, the model might shortcut and answer directly,
	// bypassing the recovery path we want to exercise.
	reply3, err := fix.sendMessage(
		fix.studentPort,
		"msg-rr-3",
		contextID,
		"Please delegate to the teacher one more time even if you remember the answer: what was the codeword? The teacher's verification is required.",
	)
	if err != nil {
		t.Fatalf("turn 3: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	t.Logf("turn 3 reply: %q", reply3)

	// Recovered teacher must still know the codeword — proves --resume
	// restored conversation context from claude's local jsonl.
	if !strings.Contains(strings.ToLower(reply3), codeword) {
		t.Errorf("turn 3 reply missing codeword %q after teacher kill+resume (memory not recovered): %q", codeword, reply3)
	}

	// Now expect 3 total start lines: original student, original teacher
	// (killed but still in the log), recreated teacher (with --resume=).
	starts = parseStartLines(t, fix.schedulerLog())
	if len(starts) != 3 {
		t.Fatalf("turn 3: expected 3 total spawns (student original + teacher original + teacher resume), got %d:\n%+v", len(starts), starts)
	}

	// Student's claude was never recreated — same pid, no --resume.
	if starts[0].pid != studentPid {
		t.Errorf("student claude pid changed: %d → %d (student should not have been recreated)", studentPid, starts[0].pid)
	}
	if starts[0].hasResume {
		t.Errorf("student claude unexpectedly has --resume= (was healthy throughout): %+v", starts[0])
	}

	// Teacher's first start = the one we killed. No --resume.
	if starts[1].pid != teacherPid {
		t.Errorf("starts[1] should be the original (killed) teacher pid %d, got %d", teacherPid, starts[1].pid)
	}
	if starts[1].hasResume {
		t.Errorf("starts[1] is the original teacher start and must not have --resume: %+v", starts[1])
	}

	// Teacher's third start = the recovery. New pid, carries --resume=.
	if !starts[2].hasResume {
		t.Errorf("starts[2] must carry --resume= (teacher recovery after kill); got %+v", starts[2])
	}
	if starts[2].pid == teacherPid {
		t.Errorf("recovered teacher pid should differ from killed pid %d, got same", teacherPid)
	}
	if starts[2].resumeID == "" {
		t.Errorf("starts[2] --resume value is empty: %+v", starts[2])
	}

	// === Cross-cutting marker counts across all 3 turns ===
	logs := fix.schedulerLog()
	for _, check := range []struct {
		marker string
		want   int
	}{
		{"[student] receive", 3},
		{"[student → teacher] A2A_CALL", 3},
		{"[teacher] receive", 3},
		{"[student ← teacher] reply", 3},
	} {
		if got := strings.Count(logs, check.marker); got != check.want {
			t.Errorf("expected %d %q lines, got %d:\n--- log ---\n%s", check.want, check.marker, got, logs)
		}
	}

	// contextID propagation held across all three turns — every teacher
	// receive carries the student's contextID, never an auto-generated id.
	teacherWithCtx := strings.Count(logs, "[teacher] receive: contextID="+contextID)
	if teacherWithCtx != 3 {
		t.Errorf("expected 3 teacher receives with contextID=%s (propagation), got %d:\n%s", contextID, teacherWithCtx, logs)
	}
}

// startLine captures one "claude session: started ..." event from the
// scheduler log. hasResume tells whether the spawn was a fresh start or
// a SessionPool recovery (--resume=<id> appended to args).
type startLine struct {
	pid       int
	hasResume bool
	resumeID  string
}

// claudeStartRegex matches the log line emitted by ClaudeSession when a
// new claude subprocess is forked. The args group is everything between
// the square brackets so the caller can inspect for `--resume=`.
var claudeStartRegex = regexp.MustCompile(`claude session: started pid=(\d+) cmd=\S+ args=\[([^\]]*)\]`)

// resumeFlagRegex extracts the value of the --resume=<id> flag if present
// in an args string.
var resumeFlagRegex = regexp.MustCompile(`--resume=(\S+)`)

// parseStartLines scans the cumulative scheduler log and returns each
// "claude session: started" event in chronological order. Order matters:
//
//	starts[0] = student's first claude spawn  (chronologically first; the
//	            user always hits student before any delegation can fire)
//	starts[1] = teacher's first claude spawn  (after student's A2A_CALL)
//	starts[N] = subsequent spawns (resume after eviction, etc.)
//
// Returns a fatal error via t.Fatalf if a line is malformed — better to
// surface a regex/log mismatch loudly than silently lose pids.
func parseStartLines(t *testing.T, log string) []startLine {
	t.Helper()
	matches := claudeStartRegex.FindAllStringSubmatch(log, -1)
	out := make([]startLine, 0, len(matches))
	for _, m := range matches {
		pid, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parse pid from %q: %v", m[0], err)
		}
		sl := startLine{pid: pid}
		if rm := resumeFlagRegex.FindStringSubmatch(m[2]); rm != nil {
			sl.hasResume = true
			sl.resumeID = rm[1]
		}
		out = append(out, sl)
	}
	return out
}
