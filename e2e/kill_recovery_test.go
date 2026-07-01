//go:build e2e

package e2e

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKillRecovery_E2E pins the SIGKILL self-heal path: when an agent's
// claude process dies out-of-band (OOM-killed, manual kill, crash), the next
// turn on the same contextId must transparently recreate the session with
// `--resume` — same conversation memory, no error surfaced to the user.
//
// This is the UX contract behind "long-lived persistent personas": agent
// memory must survive anything short of losing the on-disk session store.
func TestKillRecovery_E2E(t *testing.T) {
	fix := setupE2E(t)

	// Turn 1: spawn teacher's claude and establish memory.
	if _, err := fix.sendMessageToAgent("teacher", "kill-m1", "kill-conv", "Remember the codeword: beta-77. Confirm in one short sentence."); err != nil {
		t.Fatalf("turn 1: %v\n--- log ---\n%s", err, fix.schedulerLog())
	}

	// SIGKILL the claude process behind the teacher. extractFirstPid finds
	// the first spawn marker — only the teacher has spawned at this point.
	pid := extractFirstPid(t, fix.schedulerLog())
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill claude pid=%d: %v", pid, err)
	}
	// Give the wrapper's reader goroutine a beat to observe stdout EOF and
	// transition the session to EVICTED.
	time.Sleep(500 * time.Millisecond)

	// Turn 2: same contextId. Pool must detect the zombie via IsHealthy and
	// recreate with --resume; memory must be intact.
	reply, err := fix.sendMessageToAgent("teacher", "kill-m2", "kill-conv", "What codeword did I tell you? Answer with the codeword only.")
	if err != nil {
		t.Fatalf("turn 2 after SIGKILL must succeed transparently: %v\n--- log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(reply, "beta-77") {
		t.Errorf("resume did not recover conversation memory, reply: %q", reply)
	}

	logs := fix.schedulerLog()
	if !strings.Contains(logs, "--resume=") {
		t.Errorf("expected a --resume= spawn after SIGKILL, log:\n%s", logs)
	}
	// Exactly two spawns for this scenario: the original and the resume.
	if n := strings.Count(logs, "claude session: started"); n != 2 {
		t.Errorf("expected 2 claude spawns (original + resume), got %d", n)
	}
}
