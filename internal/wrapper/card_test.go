package wrapper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveUploadDir(t *testing.T) {
	// Default: a stable subdir of the OS temp dir, shared by both the console
	// and the agent so a dropped file is readable with zero config.
	t.Setenv("AHSIR_UPLOAD_DIR", "")
	if got, want := ResolveUploadDir(), filepath.Join(os.TempDir(), "ahsir-uploads"); got != want {
		t.Fatalf("default = %q, want %q", got, want)
	}
	// Env override wins (so the operator can point both sides at an
	// agent-readable directory in one place).
	t.Setenv("AHSIR_UPLOAD_DIR", "/srv/ahsir/drops")
	if got := ResolveUploadDir(); got != "/srv/ahsir/drops" {
		t.Fatalf("override = %q, want /srv/ahsir/drops", got)
	}
}

func TestLoadAgentCardFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: Backend Agent
description: Go backend development
version: "1.0.0"
provider:
  name: ahsir
  url: https://github.com/wu8685/ahsir
skills:
  - name: api-design
    description: Design RESTful APIs
  - name: database-schema
    description: Database schema design
claude:
  systemPrompt: "You are a Go backend developer."
  maxAgentCalls: 5
network:
  bind: "127.0.0.1"
`
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(yamlContent), 0644)

	builder := NewAgentCardBuilder(dir)
	card, err := builder.Load()
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "Backend Agent" {
		t.Errorf("expected 'Backend Agent', got '%s'", card.Name)
	}
	if card.Version != "1.0.0" {
		t.Errorf("expected '1.0.0', got '%s'", card.Version)
	}
	if len(card.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(card.Skills))
	}
	if card.Skills[0].Name != "api-design" {
		t.Errorf("expected 'api-design', got '%s'", card.Skills[0].Name)
	}
}

func TestBuildRuntimeAgentCard(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: Test Agent
description: Test
version: "1.0.0"
skills: []
network:
  bind: "127.0.0.1"
`
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(yamlContent), 0644)

	builder := NewAgentCardBuilder(dir)
	cfg, err := builder.Load()
	if err != nil {
		t.Fatal(err)
	}

	runtimeCard := builder.BuildRuntime(cfg, 9801)
	if runtimeCard.URL != "http://127.0.0.1:9801/" {
		t.Errorf("expected URL 'http://127.0.0.1:9801/', got '%s'", runtimeCard.URL)
	}
	if runtimeCard.Provider == nil {
		t.Error("expected provider to be set")
	}
}

func TestLoadAgentCardFileNotFound(t *testing.T) {
	dir := t.TempDir()
	builder := NewAgentCardBuilder(dir)
	_, err := builder.Load()
	if err == nil {
		t.Error("expected error for missing agent-card.yaml")
	}
}

func TestLoadAgentCardRuntimeDefaults(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: A
version: "1.0.0"
skills: []
`
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(yamlContent), 0644)

	cfg, err := NewAgentCardBuilder(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Command != "claude" {
		t.Errorf("expected default command 'claude', got %q", cfg.Runtime.Command)
	}
	if len(cfg.Runtime.Args) == 0 || cfg.Runtime.Args[0] != "-p" {
		t.Errorf("expected default args to start with -p, got %v", cfg.Runtime.Args)
	}
	if cfg.Runtime.Timeout != "120s" {
		t.Errorf("expected default timeout 120s, got %q", cfg.Runtime.Timeout)
	}
}

func TestLoadAgentCardRuntimeOverride(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: A
version: "1.0.0"
skills: []
runtime:
  command: gemini
  args: ["-p", "--model", "gemini-2.5-flash"]
  env:
    GOOGLE_API_KEY: "fake-key"
  timeout: 30s
`
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(yamlContent), 0644)

	cfg, err := NewAgentCardBuilder(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Command != "gemini" {
		t.Errorf("expected command 'gemini', got %q", cfg.Runtime.Command)
	}
	if len(cfg.Runtime.Args) != 3 || cfg.Runtime.Args[2] != "gemini-2.5-flash" {
		t.Errorf("unexpected args: %v", cfg.Runtime.Args)
	}
	if cfg.Runtime.Env["GOOGLE_API_KEY"] != "fake-key" {
		t.Errorf("expected env GOOGLE_API_KEY to be set, got %v", cfg.Runtime.Env)
	}
	if cfg.Runtime.Timeout != "30s" {
		t.Errorf("expected timeout 30s, got %q", cfg.Runtime.Timeout)
	}
}

// TestLoadAgentCardAgentIdleTimeout verifies the scale-to-zero
// runtime.agent_idle_timeout field parses from YAML and that an unset value
// stays empty (so the agent applies the global DefaultAgentIdleTimeout, not a
// literal zero that would pin it resident).
func TestLoadAgentCardAgentIdleTimeout(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: A
version: "1.0.0"
skills: []
runtime:
  command: claude
  agent_idle_timeout: 15m
`
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(yamlContent), 0644)

	cfg, err := NewAgentCardBuilder(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.AgentIdleTimeout != "15m" {
		t.Errorf("expected agent_idle_timeout '15m', got %q", cfg.Runtime.AgentIdleTimeout)
	}
	d, enabled, err := ParseAgentIdleTimeout(cfg.Runtime.AgentIdleTimeout)
	if err != nil || !enabled || d != 15*time.Minute {
		t.Errorf("ParseAgentIdleTimeout(%q) = (%v,%v,%v)", cfg.Runtime.AgentIdleTimeout, d, enabled, err)
	}

	// Unset case: field stays empty, so the default applies.
	dir2 := t.TempDir()
	a2aDir2 := filepath.Join(dir2, ".a2a")
	os.MkdirAll(a2aDir2, 0755)
	os.WriteFile(filepath.Join(a2aDir2, "agent-card.yaml"), []byte("name: B\nversion: \"1.0.0\"\nskills: []\n"), 0644)
	cfg2, err := NewAgentCardBuilder(dir2).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Runtime.AgentIdleTimeout != "" {
		t.Errorf("expected unset agent_idle_timeout to stay empty, got %q", cfg2.Runtime.AgentIdleTimeout)
	}
	if d, enabled, _ := ParseAgentIdleTimeout(cfg2.Runtime.AgentIdleTimeout); !enabled || d != DefaultAgentIdleTimeout {
		t.Errorf("unset agent_idle_timeout should resolve to DefaultAgentIdleTimeout, got (%v,%v)", d, enabled)
	}
}

// TestLoadAgentCardSessionIdleTTL verifies the renamed pool.session_idle_ttl
// key parses into the SessionIdleTTL field.
func TestLoadAgentCardSessionIdleTTL(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: A
version: "1.0.0"
skills: []
pool:
  session_idle_ttl: 45m
`
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(yamlContent), 0644)

	cfg, err := NewAgentCardBuilder(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool.SessionIdleTTL != "45m" {
		t.Errorf("expected session_idle_ttl '45m', got %q", cfg.Pool.SessionIdleTTL)
	}
}

// TestLoadAgentCardRejectsRenamedKeys locks in the fail-loud contract from
// issue #11: a card still carrying a pre-rename key must be rejected with an
// error that names the replacement, never silently ignored (which would flip
// behaviour — e.g. a lost idle_timeout: 0 turning a resident agent into a
// 10m-reaped one).
func TestLoadAgentCardRejectsRenamedKeys(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string // substring the error must mention
	}{
		{
			name:    "runtime idle_timeout",
			yaml:    "name: A\nversion: \"1.0.0\"\nskills: []\nruntime:\n  idle_timeout: 0\n",
			wantSub: "runtime.agent_idle_timeout",
		},
		{
			name:    "pool idle_ttl",
			yaml:    "name: A\nversion: \"1.0.0\"\nskills: []\npool:\n  idle_ttl: 30m\n",
			wantSub: "pool.session_idle_ttl",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			a2aDir := filepath.Join(dir, ".a2a")
			os.MkdirAll(a2aDir, 0755)
			os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(c.yaml), 0644)

			_, err := NewAgentCardBuilder(dir).Load()
			if err == nil {
				t.Fatalf("expected error for renamed key, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q should mention new key %q", err.Error(), c.wantSub)
			}
		})
	}
}

func TestLoadAgentCardInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte("invalid: [yaml"), 0644)

	builder := NewAgentCardBuilder(dir)
	_, err := builder.Load()
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}
