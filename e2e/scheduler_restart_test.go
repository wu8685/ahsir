//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// TestSchedulerRestart_E2E pins the cross-restart session recovery UX: a user
// stops the scheduler (deploy, reboot, crash) and starts it again — then
// continues a conversation under the same contextId and the agent still
// remembers it. The chain under test:
//
//	sessions.json persisted under <workspace>/.a2a/  (survives in tmp dir)
//	→ fresh ahsir-agent rehydrates the pool as EVICTED entries
//	→ next turn spawns claude with --resume=<old session id>
//
// This is the flagship "recover interrupted agent sessions" feature — without
// this test a regression silently downgrades every restart to amnesia.
func TestSchedulerRestart_E2E(t *testing.T) {
	fix := setupE2E(t)

	// Turn 1: establish memory on the first scheduler generation.
	if _, err := fix.sendMessageToAgent("teacher", "restart-m1", "restart-conv", "Remember the codeword: gamma-31. Confirm in one short sentence."); err != nil {
		t.Fatalf("turn 1: %v\n--- log ---\n%s", err, fix.schedulerLog())
	}

	// Full scheduler bounce: graceful stop (kills agent subprocesses too),
	// then a fresh `ahsir start` against the same config + workspaces.
	fix.restartScheduler()

	// Turn 2 on the new generation, same contextId.
	reply, err := fix.sendMessageToAgent("teacher", "restart-m2", "restart-conv", "What codeword did I tell you? Answer with the codeword only.")
	if err != nil {
		t.Fatalf("turn 2 after restart: %v\n--- log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(reply, "gamma-31") {
		t.Errorf("conversation memory did not survive the scheduler restart, reply: %q", reply)
	}

	logs := fix.schedulerLog()
	if !strings.Contains(logs, "--resume=") {
		t.Errorf("expected the post-restart turn to spawn claude with --resume=, log:\n%s", logs)
	}
}
