//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestMixedClaudeAndCodexCollaborate_E2E(t *testing.T) {
	fix := setupMixedProviderE2E(t)

	reply, err := fix.sendMessage(
		fix.studentPort,
		"msg-mixed-1",
		"mixed-provider-collab",
		"Ask the teacher for the classroom passphrase and relay the exact answer.",
	)
	if err != nil {
		t.Fatalf("mixed-provider task: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(reply, "cross-provider-papaya-17") {
		t.Fatalf("mixed-provider reply should contain Codex teacher answer, got %q\n--- scheduler log ---\n%s", reply, fix.schedulerLog())
	}

	logs := fix.schedulerLog()
	for _, marker := range []string{
		"Executor wired: codex SessionPool",
		"Executor wired: deepseek SessionPool",
		"claude session: started",
		"codex session: started",
		"[student] receive",
		"[student → teacher] A2A_CALL",
		"[teacher] receive",
		"[student ← teacher] reply",
	} {
		if !strings.Contains(logs, marker) {
			t.Fatalf("scheduler log missing marker %q\n--- log ---\n%s", marker, logs)
		}
	}
}
