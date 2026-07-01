//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestObservability_E2E pins the two commands a user reaches for first when
// something feels off:
//
//	ahsir doctor          → green checklist against a healthy stack, exit 0;
//	                        readable failure + exit 1 when the scheduler is down
//	ahsir trace <ctx>     → the conversation that just happened, with status
//
// Also asserts the ledger file behind trace keeps its 0600 posture — the
// trace feature was explicitly gated on that hygiene work.
func TestObservability_E2E(t *testing.T) {
	fix := setupE2E(t)

	// Produce one real invocation for trace to find.
	if _, err := fix.sendMessageToAgent("teacher", "obs-m1", "obs-conv", "What is HTTP? One short sentence."); err != nil {
		t.Fatalf("seed chat: %v\n--- log ---\n%s", err, fix.schedulerLog())
	}

	// doctor against the healthy stack.
	stdout, stderr, exit := fix.runCLI("doctor", "--scheduler", fix.schedulerURL())
	if exit != 0 {
		t.Fatalf("doctor must exit 0 on a healthy stack (exit %d)\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	}
	for _, want := range []string{"scheduler", "agent teacher", "agent student", "online"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "✗") {
		t.Errorf("doctor reported a failed check on a healthy stack:\n%s", stdout)
	}

	// doctor against a dead scheduler: actionable error + exit 1.
	stdout, stderr, exit = fix.runCLI("doctor", "--scheduler", "http://127.0.0.1:9")
	if exit != 1 {
		t.Errorf("doctor must exit 1 when the scheduler is unreachable, got %d\nstdout:\n%s", exit, stdout)
	}
	if !strings.Contains(stdout+stderr, "unreachable") {
		t.Errorf("doctor failure must say the scheduler is unreachable:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}

	// trace <contextId> — the invocation we just made, settled as completed.
	stdout, stderr, exit = fix.runCLI("trace", "obs-conv", "--scheduler", fix.schedulerURL())
	if exit != 0 {
		t.Fatalf("trace failed (exit %d): %s", exit, stderr)
	}
	for _, want := range []string{"teacher", "obs-conv", "completed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("trace output missing %q:\n%s", want, stdout)
		}
	}

	// trace --json parses and carries the same record.
	stdout, _, exit = fix.runCLI("trace", "obs-conv", "--json", "--scheduler", fix.schedulerURL())
	if exit != 0 {
		t.Fatalf("trace --json failed (exit %d)", exit)
	}
	var records []map[string]any
	if err := json.Unmarshal([]byte(stdout), &records); err != nil {
		t.Fatalf("trace --json output is not valid JSON: %v\n%s", err, stdout)
	}
	if len(records) == 0 {
		t.Fatal("trace --json returned no records for obs-conv")
	}
	if records[0]["status"] != "completed" || records[0]["agentName"] != "teacher" {
		t.Errorf("unexpected trace record: %+v", records[0])
	}

	// The ledger behind trace must keep the private-file posture.
	if info, err := os.Stat(fix.ledgerPath()); err != nil {
		t.Errorf("ledger file missing: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("ledger permissions = %o, want 600", perm)
	}
}
