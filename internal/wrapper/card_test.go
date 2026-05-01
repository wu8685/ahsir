package wrapper

import (
	"os"
	"path/filepath"
	"testing"
)

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
