package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseConfig = `agents: []

registry:
  host: "127.0.0.1"
  port: 9800
  heartbeat_interval: 10s

mcp: {}

timeouts:
  chat: 10m
  task_status: 30s

port_range:
  start: 9801
  end: 9810
`

// TestAddAgentToConfig_AppendsToEmpty verifies the headline scaffold path:
// an existing config with `agents: []` gains a new entry, the rest of the
// document is preserved, and the file is valid YAML that LoadConfig can
// round-trip.
func TestAddAgentToConfig_AppendsToEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ahsir.yaml")
	if err := os.WriteFile(path, []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AddAgentToConfig(path, "alpha", "/tmp/alpha-ws", 0); err != nil {
		t.Fatalf("AddAgentToConfig: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after add: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("want 1 agent, got %d", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "alpha" {
		t.Errorf("name: got %q want alpha", cfg.Agents[0].Name)
	}
	if cfg.Agents[0].Workspace != "/tmp/alpha-ws" {
		t.Errorf("workspace: got %q", cfg.Agents[0].Workspace)
	}

	// Surrounding sections must survive the round-trip — this is the
	// reason for using yaml.Node-based editing instead of struct marshal.
	contents, _ := os.ReadFile(path)
	for _, marker := range []string{"port: 9800", "chat: 10m", "port_range:"} {
		if !strings.Contains(string(contents), marker) {
			t.Errorf("file missing %q after add:\n%s", marker, contents)
		}
	}
}

// TestAddAgentToConfig_IdempotentOnDuplicate verifies that re-running
// `agent new` with an existing name is a no-op rather than appending a
// duplicate. Useful when the previous invocation half-succeeded (e.g.
// scheduler was down) and the caller retries.
func TestAddAgentToConfig_IdempotentOnDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ahsir.yaml")
	if err := os.WriteFile(path, []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := AddAgentToConfig(path, "alpha", "/tmp/alpha-ws", 0); err != nil {
			t.Fatalf("AddAgentToConfig iter %d: %v", i, err)
		}
	}
	cfg, _ := LoadConfig(path)
	if len(cfg.Agents) != 1 {
		t.Errorf("want 1 agent after 3 duplicate adds, got %d", len(cfg.Agents))
	}
}

// TestAddAgentToConfig_PreservesExisting checks that appending leaves
// already-registered agents intact (e.g. a hand-written agent alongside
// CLI-managed ones).
func TestAddAgentToConfig_PreservesExisting(t *testing.T) {
	configWithOne := `agents:
  - name: original
    workspace: /tmp/original-ws

registry:
  host: "127.0.0.1"
  port: 9800

mcp: {}
timeouts: { chat: 10m, task_status: 30s }
port_range: { start: 9801, end: 9810 }
`
	path := filepath.Join(t.TempDir(), "ahsir.yaml")
	if err := os.WriteFile(path, []byte(configWithOne), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentToConfig(path, "added", "/tmp/added-ws", 0); err != nil {
		t.Fatalf("AddAgentToConfig: %v", err)
	}
	cfg, _ := LoadConfig(path)
	if len(cfg.Agents) != 2 {
		t.Fatalf("want 2 agents, got %d", len(cfg.Agents))
	}
	names := []string{cfg.Agents[0].Name, cfg.Agents[1].Name}
	if !(contains(names, "original") && contains(names, "added")) {
		t.Errorf("want both original and added, got %v", names)
	}
}

// TestAddAgentToConfig_WithExplicitPort verifies port plumbing — when
// callers want a deterministic port (rare; typically port=0 to
// auto-allocate), the value is preserved.
func TestAddAgentToConfig_WithExplicitPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ahsir.yaml")
	if err := os.WriteFile(path, []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AddAgentToConfig(path, "pinned", "/tmp/pinned-ws", 9805); err != nil {
		t.Fatalf("AddAgentToConfig: %v", err)
	}
	cfg, _ := LoadConfig(path)
	if cfg.Agents[0].Port != 9805 {
		t.Errorf("port: got %d want 9805", cfg.Agents[0].Port)
	}
}

// TestRemoveAgentFromConfig_DropsNamed verifies removal preserves the
// other agents and the surrounding document structure.
func TestRemoveAgentFromConfig_DropsNamed(t *testing.T) {
	configWithTwo := `agents:
  - name: keep-me
    workspace: /tmp/keep
  - name: delete-me
    workspace: /tmp/delete

registry:
  host: "127.0.0.1"
  port: 9800

mcp: {}
timeouts: { chat: 10m, task_status: 30s }
port_range: { start: 9801, end: 9810 }
`
	path := filepath.Join(t.TempDir(), "ahsir.yaml")
	if err := os.WriteFile(path, []byte(configWithTwo), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAgentFromConfig(path, "delete-me"); err != nil {
		t.Fatalf("RemoveAgentFromConfig: %v", err)
	}
	cfg, _ := LoadConfig(path)
	if len(cfg.Agents) != 1 {
		t.Fatalf("want 1 agent after remove, got %d", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "keep-me" {
		t.Errorf("wrong agent kept: %q", cfg.Agents[0].Name)
	}
}

// TestRemoveAgentFromConfig_NoopOnMissing — `agent delete X` where X
// doesn't exist should be a no-op, not an error. Lets callers
// idempotently call delete during cleanup.
func TestRemoveAgentFromConfig_NoopOnMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ahsir.yaml")
	if err := os.WriteFile(path, []byte(baseConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAgentFromConfig(path, "never-existed"); err != nil {
		t.Errorf("RemoveAgentFromConfig should be no-op on missing, got: %v", err)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
