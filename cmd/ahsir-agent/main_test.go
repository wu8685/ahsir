package main

import (
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/wrapper"
)

func TestBuildSessionConfig_CodexProvider(t *testing.T) {
	cfg, err := buildSessionConfig("coder", wrapper.RuntimeConfig{
		Provider: "codex",
		Command:  "codex",
		Model:    "gpt-5.4",
		Args:     []string{"--sandbox=workspace-write"},
	}, wrapper.FilesystemConfig{
		Enabled:      true,
		AllowedPaths: []string{"."},
	}, wrapper.StreamingConfig{PartialMessages: true}, "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != wrapper.ProviderCodex {
		t.Fatalf("Provider = %q", cfg.Provider)
	}
	if cfg.Command != "codex" {
		t.Fatalf("Command = %q", cfg.Command)
	}
	joined := strings.Join(cfg.Args, " ")
	if !strings.Contains(joined, "--model=gpt-5.4") {
		t.Fatalf("missing codex model flag in %v", cfg.Args)
	}
	if !strings.Contains(joined, "--add-dir=/tmp/workspace") {
		t.Fatalf("missing add-dir in %v", cfg.Args)
	}
	if strings.Contains(joined, "--allowedTools") {
		t.Fatalf("codex config should not receive claude allowedTools: %v", cfg.Args)
	}
	if strings.Contains(joined, "--include-partial-messages") {
		t.Fatalf("codex config should not receive claude partial flag: %v", cfg.Args)
	}
}

func TestBuildSessionConfig_ClaudeProviderStillGetsClaudeFlags(t *testing.T) {
	cfg, err := buildSessionConfig("teacher", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{
		Enabled:      true,
		AllowedPaths: []string{"."},
	}, wrapper.StreamingConfig{PartialMessages: true}, "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Args, " ")
	if !strings.Contains(joined, "--allowedTools=Read,LS,Glob,Grep") {
		t.Fatalf("missing claude allowedTools in %v", cfg.Args)
	}
	if !strings.Contains(joined, "--include-partial-messages") {
		t.Fatalf("missing claude partial flag in %v", cfg.Args)
	}
}
