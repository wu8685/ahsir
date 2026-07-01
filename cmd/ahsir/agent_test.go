package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/wrapper"
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

func TestParseMCPConfigFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Empty path → no servers, no error (the no-MCP default).
	if got, err := parseMCPConfigFile(""); got != nil || err != nil {
		t.Fatalf("empty path = (%v, %v), want (nil, nil)", got, err)
	}

	// claude .mcp.json envelope is unwrapped to the servers map.
	envelope := write("env.json", `{"mcpServers":{"docs":{"command":"npx"}}}`)
	got, err := parseMCPConfigFile(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["docs"]; !ok || len(got) != 1 {
		t.Fatalf("envelope not unwrapped: %v", got)
	}

	// A bare servers map is accepted as-is.
	bare := write("bare.json", `{"docs":{"command":"npx"}}`)
	if got, err := parseMCPConfigFile(bare); err != nil || got["docs"] == nil {
		t.Fatalf("bare map = (%v, %v)", got, err)
	}

	// Garbage and empty-servers cases error rather than silently passing.
	bad := write("bad.json", `not json`)
	if _, err := parseMCPConfigFile(bad); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
	empty := write("empty.json", `{"mcpServers":{}}`)
	if _, err := parseMCPConfigFile(empty); err == nil {
		t.Fatal("expected error when no servers declared")
	}
}

// TestWriteAgentCard_MCPRoundTrips writes a card with MCP servers and loads it
// back through the REAL wrapper card loader — proving the scaffolded YAML is
// consumable downstream (not just string-matchable).
func TestWriteAgentCard_MCPRoundTrips(t *testing.T) {
	workspace := t.TempDir()
	servers := map[string]any{
		"docs": map[string]any{"command": "npx", "args": []any{"-y", "doc-server"}},
	}
	if err := writeAgentCard(workspace, agentCardScaffold{
		Name:       "reviewer",
		Provider:   "anthropic",
		BaseURL:    defaultProviderBaseURL,
		APIKey:     defaultProviderAPIKey,
		Model:      defaultProviderModel,
		Timeout:    "300s",
		MCPServers: servers,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := wrapper.NewAgentCardBuilder(workspace).Load()
	if err != nil {
		t.Fatalf("load scaffolded card: %v", err)
	}
	if cfg.MCP.Servers["docs"] == nil {
		t.Fatalf("mcp.servers.docs missing after round-trip: %+v", cfg.MCP)
	}
	// And the declared server survives as a usable claude mcpServers entry.
	raw, _ := json.Marshal(map[string]any{"mcpServers": cfg.MCP.Servers})
	if !strings.Contains(string(raw), `"command":"npx"`) {
		t.Fatalf("server detail lost: %s", raw)
	}

	// A scaffold WITHOUT MCP servers must leave no mcp block (isolated default).
	bare := t.TempDir()
	if err := writeAgentCard(bare, agentCardScaffold{Name: "x", Provider: "anthropic", Timeout: "300s"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(bare, ".a2a", "agent-card.yaml"))
	if strings.Contains(string(data), "mcp:") {
		t.Fatalf("no-MCP scaffold should omit the mcp block:\n%s", data)
	}
}

func assertContains(t *testing.T, s, want string) {
	t.Helper()
	if !strings.Contains(s, want) {
		t.Fatalf("expected %q in:\n%s", want, s)
	}
}
