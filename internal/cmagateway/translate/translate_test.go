package translate

import (
	"testing"

	"github.com/wu8685/ahsir/internal/cmagateway/cma"
)

func baseAgent() *cma.Agent {
	return &cma.Agent{
		ID:       "agent_abc",
		Version:  1,
		Name:     "coder",
		Model:    cma.ModelConfig{ID: "claude-x"},
		Metadata: map[string]string{},
	}
}

// session_isolation metadata must flow into card.Pool.SessionIsolation so
// facade-created agents can opt into per-session worktrees (issue #19/#24).
func TestAgentToCard_SessionIsolation(t *testing.T) {
	cases := []struct {
		name string
		meta string
		want string
	}{
		{"worktree", "worktree", "worktree"},
		{"scratch", "scratch", "scratch"},
		{"off", "off", "off"},
		{"unset defaults empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := baseAgent()
			if tc.meta != "" {
				a.Metadata["session_isolation"] = tc.meta
			}
			card := AgentToCard("cma-coder-v1", a, RuntimeDefaults{})
			if card.Pool.SessionIsolation != tc.want {
				t.Fatalf("Pool.SessionIsolation = %q, want %q", card.Pool.SessionIsolation, tc.want)
			}
		})
	}
}

// Sanity: the existing shell_access / runtime_timeout knobs still map, so the
// new Pool wiring didn't disturb the sibling metadata mappings.
func TestAgentToCard_ExistingMetadataStillMaps(t *testing.T) {
	a := baseAgent()
	a.Metadata["shell_access"] = "true"
	a.Metadata["runtime_timeout"] = "10m"
	card := AgentToCard("cma-coder-v1", a, RuntimeDefaults{})
	if !card.Filesystem.ShellAccess {
		t.Errorf("ShellAccess = false, want true")
	}
	if card.Runtime.Timeout != "10m" {
		t.Errorf("Runtime.Timeout = %q, want 10m", card.Runtime.Timeout)
	}
}

// agent_idle_timeout metadata must flow into card.Runtime.AgentIdleTimeout so a
// facade-created agent can be pinned resident ("0") and never cold-started on
// the next event (issue #17). Unset must stay empty so ahsir applies its 10m
// default.
func TestAgentToCard_AgentIdleTimeout(t *testing.T) {
	cases := []struct {
		name string
		meta string
		want string
	}{
		{"pin resident", "0", "0"},
		{"explicit window", "30m", "30m"},
		{"unset defaults empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := baseAgent()
			if tc.meta != "" {
				a.Metadata["agent_idle_timeout"] = tc.meta
			}
			card := AgentToCard("cma-coder-v1", a, RuntimeDefaults{})
			if card.Runtime.AgentIdleTimeout != tc.want {
				t.Fatalf("Runtime.AgentIdleTimeout = %q, want %q", card.Runtime.AgentIdleTimeout, tc.want)
			}
		})
	}
}

func TestInstances(t *testing.T) {
	cases := []struct {
		name string
		meta string
		want int
	}{
		{"two", "2", 2},
		{"whitespace trimmed", "  4 ", 4},
		{"unset", "", 0},
		{"zero collapses", "0", 0},
		{"negative collapses", "-3", 0},
		{"non-numeric", "lots", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := baseAgent()
			if tc.meta != "" {
				a.Metadata["instances"] = tc.meta
			}
			if got := Instances(a); got != tc.want {
				t.Fatalf("Instances(%q) = %d, want %d", tc.meta, got, tc.want)
			}
		})
	}
}
