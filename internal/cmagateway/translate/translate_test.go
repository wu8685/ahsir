package translate

import (
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/cmagateway/ahsir"
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

func mustAgentCard(t *testing.T, a *cma.Agent, d RuntimeDefaults) *ahsir.AgentCard {
	t.Helper()
	card, err := AgentToCard("cma-coder-v1", a, d)
	if err != nil {
		t.Fatalf("AgentToCard() error = %v", err)
	}
	return card
}

func TestAgentToCard_RuntimeOverrides(t *testing.T) {
	defaults := RuntimeDefaults{
		Provider: "anthropic",
		BaseURL:  "https://global.example/",
		APIKey:   "${GLOBAL_API_KEY}",
	}
	tests := []struct {
		name         string
		model        string
		metadata     map[string]string
		wantProvider string
		wantBaseURL  string
		wantAPIKey   string
	}{
		{
			name:         "global defaults unchanged",
			model:        "claude-x",
			metadata:     map[string]string{},
			wantProvider: "anthropic",
			wantBaseURL:  "https://global.example/",
			wantAPIKey:   "${GLOBAL_API_KEY}",
		},
		{
			name:         "codex provider only",
			model:        "gpt-5.6-sol",
			metadata:     map[string]string{"runtime_provider": "codex"},
			wantProvider: "codex",
			wantBaseURL:  "https://global.example/",
			wantAPIKey:   "${GLOBAL_API_KEY}",
		},
		{
			name:  "kimi full override",
			model: "k3",
			metadata: map[string]string{
				"runtime_provider":    "anthropic",
				"runtime_base_url":    "https://api.kimi.com/coding/",
				"runtime_api_key_env": "MOONSHOT_API_KEY",
			},
			wantProvider: "anthropic",
			wantBaseURL:  "https://api.kimi.com/coding/",
			wantAPIKey:   "${MOONSHOT_API_KEY}",
		},
		{
			name:  "empty values inherit defaults",
			model: "claude-x",
			metadata: map[string]string{
				"runtime_provider":    "",
				"runtime_base_url":    "",
				"runtime_api_key_env": "",
			},
			wantProvider: "anthropic",
			wantBaseURL:  "https://global.example/",
			wantAPIKey:   "${GLOBAL_API_KEY}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := baseAgent()
			a.Model.ID = tc.model
			a.Metadata = tc.metadata
			card := mustAgentCard(t, a, defaults)
			if card.Runtime.Provider != tc.wantProvider {
				t.Errorf("Runtime.Provider = %q, want %q", card.Runtime.Provider, tc.wantProvider)
			}
			if card.Runtime.BaseURL != tc.wantBaseURL {
				t.Errorf("Runtime.BaseURL = %q, want %q", card.Runtime.BaseURL, tc.wantBaseURL)
			}
			if card.Runtime.APIKey != tc.wantAPIKey {
				t.Errorf("Runtime.APIKey = %q, want %q", card.Runtime.APIKey, tc.wantAPIKey)
			}
			if card.Runtime.Model != tc.model {
				t.Errorf("Runtime.Model = %q, want %q", card.Runtime.Model, tc.model)
			}
		})
	}
}

func TestAgentToCard_RuntimeAPIKeyEnvAcceptsShellNames(t *testing.T) {
	for _, envName := range []string{"_KEY", "A", "A1", "MOONSHOT_API_KEY"} {
		t.Run(envName, func(t *testing.T) {
			a := baseAgent()
			a.Metadata["runtime_api_key_env"] = envName
			card := mustAgentCard(t, a, RuntimeDefaults{})
			want := "${" + envName + "}"
			if card.Runtime.APIKey != want {
				t.Fatalf("Runtime.APIKey = %q, want %q", card.Runtime.APIKey, want)
			}
		})
	}
}

func TestAgentToCard_RuntimeAPIKeyEnvRejectsInvalidNames(t *testing.T) {
	for _, envName := range []string{"1KEY", "KEY-NAME", "KEY NAME", "${KEY}", "KEY=value"} {
		t.Run(envName, func(t *testing.T) {
			a := baseAgent()
			a.Metadata["runtime_api_key_env"] = envName
			card, err := AgentToCard("cma-coder-v1", a, RuntimeDefaults{})
			if err == nil || card != nil {
				t.Fatalf("AgentToCard() = (%v, %v), want (nil, error)", card, err)
			}
			if !strings.Contains(err.Error(), "runtime_api_key_env") {
				t.Fatalf("error = %q, want metadata key", err)
			}
		})
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
			card := mustAgentCard(t, a, RuntimeDefaults{})
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
	card := mustAgentCard(t, a, RuntimeDefaults{})
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
			card := mustAgentCard(t, a, RuntimeDefaults{})
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
