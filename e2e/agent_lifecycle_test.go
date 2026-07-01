//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentLifecycle_E2E pins the orchestrator-skill main path end to end:
//
//	ahsir agent new <name>   → scaffold + register + start, NO scheduler restart
//	ahsir list               → new agent visible
//	ahsir chat <name> "..."  → immediately conversable
//	ahsir agent delete       → stopped + gone from list, files preserved
//
// This is the exact command sequence the Claude Code skill walks every time
// a user says "create a reviewer agent" — it had no coverage before this
// test, while being the most user-visible flow in the product.
func TestAgentLifecycle_E2E(t *testing.T) {
	fix := setupE2E(t)

	const name = "e2e-newbie"
	cfgPath := filepath.Join(fix.configDir, "ahsir.yaml")
	ws := filepath.Join(fix.configDir, "workspaces", name)

	// agent new — scaffolds card + appends config + asks the RUNNING
	// scheduler to start it. Exercises the admin API and (because the
	// fixture's port_range starts at the teacher's occupied port) the
	// auto-allocation skip-occupied-port path too.
	stdout, stderr, exit := fix.runCLI("agent", "new", name,
		"--prompt", "You are an echo agent. Follow the user's instruction exactly and answer in one short line.",
		"--skill", "echo=repeat what the user asks for",
		"--config", cfgPath,
		"--workspace", ws,
		"--scheduler", fix.schedulerURL(),
	)
	if exit != 0 {
		t.Fatalf("agent new failed (exit %d)\nstdout:\n%s\nstderr:\n%s\n--- scheduler log ---\n%s", exit, stdout, stderr, fix.schedulerLog())
	}
	if !strings.Contains(stdout, name) {
		t.Errorf("agent new must print the new agent's name on stdout, got: %q", stdout)
	}

	// Scaffolded card may carry a literal apiKey — must never be
	// world-readable.
	cardPath := filepath.Join(ws, ".a2a", "agent-card.yaml")
	if info, err := os.Stat(cardPath); err != nil {
		t.Errorf("scaffolded card missing: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("agent-card.yaml permissions = %o, want 600", perm)
	}

	// list — the new agent shows up alongside teacher/student.
	stdout, stderr, exit = fix.runCLI("list", "--scheduler", fix.schedulerURL())
	if exit != 0 {
		t.Fatalf("list failed (exit %d): %s", exit, stderr)
	}
	if !strings.Contains(stdout, name) {
		t.Fatalf("new agent %q missing from list output:\n%s\n--- scheduler log ---\n%s", name, stdout, fix.schedulerLog())
	}

	// chat — immediately conversable through the CLI, no restart in between.
	stdout, stderr, exit = fix.runCLI("chat", name,
		"Reply with exactly: lifecycle-ok",
		"--scheduler", fix.schedulerURL(),
	)
	if exit != 0 {
		t.Fatalf("chat with fresh agent failed (exit %d)\nstderr:\n%s\n--- scheduler log ---\n%s", exit, stderr, fix.schedulerLog())
	}
	if !strings.Contains(stdout, "lifecycle-ok") {
		t.Errorf("fresh agent reply missing expected text: %q", stdout)
	}

	// agent delete — process stopped, registry entry gone, workspace files
	// preserved (recoverable persona).
	_, stderr, exit = fix.runCLI("agent", "delete", name,
		"--config", cfgPath,
		"--scheduler", fix.schedulerURL(),
	)
	if exit != 0 {
		t.Fatalf("agent delete failed (exit %d): %s", exit, stderr)
	}
	stdout, _, _ = fix.runCLI("list", "--scheduler", fix.schedulerURL())
	if strings.Contains(stdout, name) {
		t.Errorf("deleted agent %q still in list output:\n%s", name, stdout)
	}
	if _, err := os.Stat(cardPath); err != nil {
		t.Errorf("delete must preserve workspace files, card gone: %v", err)
	}
}
