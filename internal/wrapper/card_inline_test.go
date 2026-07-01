package wrapper

import (
	"encoding/json"
	"testing"
)

// TestAgentCardConfig_JSONInlineDecode locks the inline-registration contract:
// a card supplied as JSON (cma-service's wire shape) must decode into
// AgentCardConfig. The underscore/camelCase keys are the ones that would break
// if a json tag drifted from its yaml key.
func TestAgentCardConfig_JSONInlineDecode(t *testing.T) {
	body := `{
		"name":"cma-x-v1",
		"version":"1",
		"claude":{"systemPrompt":"be concise","maxAgentCalls":3},
		"runtime":{"provider":"anthropic","baseURL":"https://api","apiKey":"sk","model":"claude-opus-4-8"},
		"filesystem":{"enabled":true,"write_access":true,"allowed_paths":["."]},
		"streaming":{"partial_messages":true},
		"mcp":{"servers":{"search":{"type":"http","url":"https://mcp"}}}
	}`
	var c AgentCardConfig
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("decode inline card: %v", err)
	}
	if c.Name != "cma-x-v1" || c.Version != "1" {
		t.Errorf("name/version = %q/%q", c.Name, c.Version)
	}
	if c.Claude.SystemPrompt != "be concise" || c.Claude.MaxAgentCalls != 3 {
		t.Errorf("claude = %+v", c.Claude)
	}
	if c.Runtime.BaseURL != "https://api" || c.Runtime.APIKey != "sk" || c.Runtime.Model != "claude-opus-4-8" {
		t.Errorf("runtime = %+v", c.Runtime)
	}
	if !c.Filesystem.Enabled || !c.Filesystem.WriteAccess || len(c.Filesystem.AllowedPaths) != 1 {
		t.Errorf("filesystem = %+v", c.Filesystem)
	}
	if !c.Streaming.PartialMessages {
		t.Error("streaming.partial_messages did not decode")
	}
	if _, ok := c.MCP.Servers["search"]; !ok {
		t.Errorf("mcp servers = %+v", c.MCP.Servers)
	}
}

// TestWriteCardRoundTrip verifies WriteCard persists a card that Load reads back
// faithfully — the on-disk half of inline registration.
func TestWriteCardRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &AgentCardConfig{
		Name:       "cma-x-v1",
		Version:    "1",
		Claude:     ClaudeConfig{SystemPrompt: "hi"},
		Runtime:    RuntimeConfig{Provider: "anthropic", BaseURL: "https://api", Model: "m"},
		Filesystem: FilesystemConfig{Enabled: true, WriteAccess: true, AllowedPaths: []string{"."}},
		Streaming:  StreamingConfig{PartialMessages: true},
	}
	if err := WriteCard(dir, in); err != nil {
		t.Fatalf("WriteCard: %v", err)
	}
	got, err := NewAgentCardBuilder(dir).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Name != "cma-x-v1" || got.Claude.SystemPrompt != "hi" {
		t.Errorf("name/system = %q/%q", got.Name, got.Claude.SystemPrompt)
	}
	if got.Runtime.Model != "m" || got.Runtime.BaseURL != "https://api" {
		t.Errorf("runtime = %+v", got.Runtime)
	}
	if !got.Filesystem.WriteAccess || !got.Streaming.PartialMessages {
		t.Errorf("fs/streaming = %+v / %+v", got.Filesystem, got.Streaming)
	}
}
