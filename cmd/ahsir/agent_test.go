package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAgentCard_CodexDefaultsDoNotLeakDeepSeek(t *testing.T) {
	workspace := t.TempDir()
	if err := writeAgentCard(workspace, agentCardScaffold{
		Name:     "coder",
		Provider: "codex",
		BaseURL:  defaultProviderBaseURL,
		APIKey:   defaultProviderAPIKey,
		Model:    defaultProviderModel,
		Timeout:  "300s",
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(workspace, ".a2a", "agent-card.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	assertContains(t, text, "command: codex")
	assertContains(t, text, "provider: codex")
	assertContains(t, text, "baseURL: \"\"")
	assertContains(t, text, "apiKey: \"\"")
	assertContains(t, text, "model: \"\"")
}

func assertContains(t *testing.T, s, want string) {
	t.Helper()
	if !strings.Contains(s, want) {
		t.Fatalf("expected %q in:\n%s", want, s)
	}
}
