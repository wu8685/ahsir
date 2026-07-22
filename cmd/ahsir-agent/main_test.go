package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		WriteAccess:  true,
		AllowedPaths: []string{"."},
	}, wrapper.MCPConfig{}, wrapper.StreamingConfig{PartialMessages: true}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != wrapper.ProviderCodex {
		t.Fatalf("Provider = %q", cfg.Provider)
	}
	if cfg.Command != "codex" {
		t.Fatalf("Command = %q", cfg.Command)
	}
	if !cfg.WriteAccess {
		t.Fatal("WriteAccess = false, want true for enabled writable filesystem")
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

func TestBuildSessionConfig_CodexWriteAccessRequiresEnabledFilesystem(t *testing.T) {
	tests := []struct {
		name string
		fs   wrapper.FilesystemConfig
	}{
		{
			name: "disabled filesystem ignores write flag",
			fs:   wrapper.FilesystemConfig{Enabled: false, WriteAccess: true},
		},
		{
			name: "enabled read only filesystem",
			fs:   wrapper.FilesystemConfig{Enabled: true, WriteAccess: false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := buildSessionConfig("coder", wrapper.RuntimeConfig{
				Provider: "codex",
				Command:  "codex",
			}, tc.fs, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, t.TempDir(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WriteAccess {
				t.Fatal("WriteAccess = true, want false")
			}
		})
	}
}

func TestBuildSessionConfig_CodexNetworkAccessRequiresWritableFilesystem(t *testing.T) {
	for _, tc := range []struct {
		name string
		fs   wrapper.FilesystemConfig
		net  wrapper.NetworkConfig
		want bool
	}{
		{
			name: "explicit outbound access with writable filesystem",
			fs:   wrapper.FilesystemConfig{Enabled: true, WriteAccess: true},
			net:  wrapper.NetworkConfig{OutboundAccess: true},
			want: true,
		},
		{
			name: "outbound access defaults disabled",
			fs:   wrapper.FilesystemConfig{Enabled: true, WriteAccess: true},
		},
		{
			name: "read only filesystem cannot gain outbound access",
			fs:   wrapper.FilesystemConfig{Enabled: true, WriteAccess: false},
			net:  wrapper.NetworkConfig{OutboundAccess: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := buildSessionConfig("coder", wrapper.RuntimeConfig{
				Provider: "codex",
				Command:  "codex",
			}, tc.fs, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, t.TempDir(), t.TempDir(), tc.net)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.NetworkAccess != tc.want {
				t.Fatalf("NetworkAccess = %v, want %v", cfg.NetworkAccess, tc.want)
			}
		})
	}
}

func TestBuildSessionConfig_RuntimeTimeoutZeroIsPreserved(t *testing.T) {
	cfg, err := buildSessionConfig("coder", wrapper.RuntimeConfig{
		Provider: "codex",
		Command:  "codex",
		Timeout:  "0s",
	}, wrapper.FilesystemConfig{}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 0 {
		t.Fatalf("expected runtime.timeout=0s to disable provider deadline, got %s", cfg.Timeout)
	}
}

func TestBuildSessionConfig_CodexInjectsIsolatedCodexHome(t *testing.T) {
	workspace := t.TempDir()
	sourceHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), []byte(`{"token":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceConfig := strings.Join([]string{
		`model = "gpt-5.5"`,
		`notify = ["notify-helper", "turn-ended"]`,
		`approval_policy = "never"`,
		`[mcp_servers.node_repl.env]`,
		`CODEX_HOME = "/Users/example/.codex"`,
		`NODE_REPL_NODE_PATH = "/Applications/Codex.app/node"`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(sourceHome, "config.toml"), []byte(sourceConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", sourceHome)

	cfg, err := buildSessionConfig("coder", wrapper.RuntimeConfig{
		Provider: "codex",
		Command:  "codex",
	}, wrapper.FilesystemConfig{}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workspace, ".a2a", "codex-home")
	if got := envValue(cfg.Env, "CODEX_HOME"); got != want {
		t.Fatalf("CODEX_HOME=%q, want %q", got, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("CODEX_HOME directory not created: info=%v err=%v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(want, "auth.json")); err != nil || string(data) != `{"token":"test"}` {
		t.Fatalf("auth.json not mirrored: data=%q err=%v", data, err)
	}
	filteredConfig, err := os.ReadFile(filepath.Join(want, "config.toml"))
	if err != nil {
		t.Fatalf("filtered config.toml not written: %v", err)
	}
	configText := string(filteredConfig)
	if strings.Contains(configText, "notify") {
		t.Fatalf("filtered config should not contain notify: %s", configText)
	}
	if strings.Contains(configText, "CODEX_HOME") {
		t.Fatalf("filtered config should not contain CODEX_HOME: %s", configText)
	}
	if !strings.Contains(configText, `model = "gpt-5.5"`) {
		t.Fatalf("filtered config lost regular settings: %s", configText)
	}
}

func TestBuildSessionConfig_CodexPreservesExplicitCodexHome(t *testing.T) {
	workspace := t.TempDir()
	cfg, err := buildSessionConfig("coder", wrapper.RuntimeConfig{
		Provider: "codex",
		Command:  "codex",
		Env: map[string]string{
			"CODEX_HOME": "/custom/codex-home",
		},
	}, wrapper.FilesystemConfig{}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(cfg.Env, "CODEX_HOME"); got != "/custom/codex-home" {
		t.Fatalf("CODEX_HOME=%q, want explicit value", got)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func TestBuildSessionConfig_ClaudeProviderStillGetsClaudeFlags(t *testing.T) {
	cfg, err := buildSessionConfig("teacher", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{
		Enabled:      true,
		AllowedPaths: []string{"."},
	}, wrapper.MCPConfig{}, wrapper.StreamingConfig{PartialMessages: true}, "/tmp/workspace", "/tmp/workspace")
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

func TestBuildSessionConfig_ClaudeWriteAccessExpandsAllowedTools(t *testing.T) {
	cfg, err := buildSessionConfig("writer", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{
		Enabled:      true,
		WriteAccess:  true,
		AllowedPaths: []string{"."},
	}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Args, " ")
	if !strings.Contains(joined, "--allowedTools=Read,LS,Glob,Grep,Edit,MultiEdit,Write") {
		t.Fatalf("missing claude write-capable allowedTools in %v", cfg.Args)
	}
}

func TestBuildSessionConfig_ClaudeShellAccessAddsBash(t *testing.T) {
	cfg, err := buildSessionConfig("sheller", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{
		Enabled:      true,
		WriteAccess:  true,
		ShellAccess:  true,
		AllowedPaths: []string{"."},
	}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Args, " ")
	if !strings.Contains(joined, "--allowedTools=Read,LS,Glob,Grep,Edit,MultiEdit,Write,Bash") {
		t.Fatalf("missing Bash in shell-access allowedTools: %v", cfg.Args)
	}
}

func TestBuildSessionConfig_ClaudeNoShellAccessOmitsBash(t *testing.T) {
	cfg, err := buildSessionConfig("noshell", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{
		Enabled:      true,
		WriteAccess:  true,
		AllowedPaths: []string{"."},
	}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cfg.Args, " "), "Bash") {
		t.Fatalf("Bash must not be granted without shell_access: %v", cfg.Args)
	}
}

// TestBuildSessionConfig_AllowListsConsoleUploadDir locks the cross-process
// wiring behind the web console's file-drop: an fs-enabled agent must carry the
// console's upload dir on --add-dir so a file dropped into the composer (and
// copied there by `ahsir ui`) is readable. An fs-disabled agent has no read
// tools at all, so the dir must NOT be added — a path would be useless anyway.
func TestBuildSessionConfig_AllowListsConsoleUploadDir(t *testing.T) {
	t.Setenv("AHSIR_UPLOAD_DIR", "/tmp/e2e-upload-dir")

	enabled, err := buildSessionConfig("teacher", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{
		Enabled:      true,
		AllowedPaths: []string{"."},
	}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(enabled.Args, " "), "--add-dir=/tmp/e2e-upload-dir") {
		t.Fatalf("fs-enabled agent missing console upload-dir add-dir in %v", enabled.Args)
	}

	disabled, err := buildSessionConfig("teacher", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(disabled.Args, " "), "/tmp/e2e-upload-dir") {
		t.Fatalf("fs-disabled agent should not allow-list the upload dir: %v", disabled.Args)
	}
}

// TestBuildSessionConfig_ClaudeIsolatesMCPByDefault locks the default: a claude
// agent with no mcp block runs with --strict-mcp-config and NO --mcp-config, so
// it never inherits the operator's global/project MCP servers. (The bug this
// guards against — inherited MCP schemas ballooning a trivial turn to ~60k input
// tokens — was found during the web-console verification.)
func TestBuildSessionConfig_ClaudeIsolatesMCPByDefault(t *testing.T) {
	cfg, err := buildSessionConfig("teacher", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cfg.Args, " ")
	if !strings.Contains(joined, "--strict-mcp-config") {
		t.Fatalf("claude must run with --strict-mcp-config (no MCP inheritance): %v", cfg.Args)
	}
	if strings.Contains(joined, "--mcp-config") {
		t.Fatalf("no servers declared, so no --mcp-config expected: %v", cfg.Args)
	}
}

// TestBuildSessionConfig_ClaudeMCPServersDeclared verifies that declared servers
// are passed through as a claude mcpServers document via --mcp-config, alongside
// --strict-mcp-config (so ONLY the declared servers load).
func TestBuildSessionConfig_ClaudeMCPServersDeclared(t *testing.T) {
	cfg, err := buildSessionConfig("teacher", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{}, wrapper.MCPConfig{
		Servers: map[string]any{
			"docs": map[string]any{"command": "npx", "args": []any{"-y", "doc-server"}},
		},
	}, wrapper.StreamingConfig{}, "/tmp/workspace", "/tmp/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cfg.Args, " "), "--strict-mcp-config") {
		t.Fatalf("expected --strict-mcp-config alongside declared servers: %v", cfg.Args)
	}
	var mcpArg string
	for _, a := range cfg.Args {
		if strings.HasPrefix(a, "--mcp-config=") {
			mcpArg = strings.TrimPrefix(a, "--mcp-config=")
		}
	}
	if mcpArg == "" {
		t.Fatalf("missing --mcp-config in %v", cfg.Args)
	}
	// The argument must be a valid claude mcpServers document containing our server.
	var doc struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(mcpArg), &doc); err != nil {
		t.Fatalf("--mcp-config is not valid JSON: %v; raw=%s", err, mcpArg)
	}
	if doc.MCPServers["docs"].Command != "npx" {
		t.Fatalf("declared server not threaded into mcpServers doc: %s", mcpArg)
	}
}

// TestBuildSessionConfig_CodexRejectsMCPServers verifies the honest failure: the
// card's mcp.servers field is claude-only, so a codex agent declaring it errors
// rather than silently dropping the servers.
func TestBuildSessionConfig_CodexRejectsMCPServers(t *testing.T) {
	_, err := buildSessionConfig("coder", wrapper.RuntimeConfig{
		Provider: "codex",
		Command:  "codex",
	}, wrapper.FilesystemConfig{}, wrapper.MCPConfig{
		Servers: map[string]any{"docs": map[string]any{"command": "npx"}},
	}, wrapper.StreamingConfig{}, t.TempDir(), t.TempDir())
	if err == nil {
		t.Fatal("expected error when codex agent declares mcp.servers")
	}
	if !strings.Contains(err.Error(), "claude-backed") {
		t.Fatalf("error should explain claude-only support, got: %v", err)
	}
}

func TestPoolRetentionConfigDefaultsAndOverrides(t *testing.T) {
	defaults, err := poolRetentionConfig(wrapper.PoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.idleTTL != 30*time.Minute {
		t.Fatalf("default idleTTL = %s", defaults.idleTTL)
	}
	if defaults.evictedTTL != 30*24*time.Hour {
		t.Fatalf("default evictedTTL = %s", defaults.evictedTTL)
	}
	if defaults.maxEvicted != 1000 {
		t.Fatalf("default maxEvicted = %d", defaults.maxEvicted)
	}

	got, err := poolRetentionConfig(wrapper.PoolConfig{
		SessionIdleTTL: "45m",
		EvictedTTL:     "7d",
		MaxEvicted:     25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.idleTTL != 45*time.Minute {
		t.Fatalf("idleTTL override = %s", got.idleTTL)
	}
	if got.evictedTTL != 7*24*time.Hour {
		t.Fatalf("evictedTTL override = %s", got.evictedTTL)
	}
	if got.maxEvicted != 25 {
		t.Fatalf("maxEvicted override = %d", got.maxEvicted)
	}
}

func TestRegistryMonitorCancelsAfterSustainedFailures(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer registry.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan struct{})
	startRegistryMonitor(ctx, registryMonitorConfig{
		URL:          registry.URL,
		Interval:     10 * time.Millisecond,
		Timeout:      5 * time.Millisecond,
		FailureGrace: 40 * time.Millisecond,
	}, func() {
		close(cancelled)
		cancel()
	})

	select {
	case <-cancelled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("registry monitor did not cancel after sustained failures")
	}
}

// TestBuildSessionConfig_WorkdirDecoupledFromWorkspace locks the workspace/cwd
// split that lets several agents share one working directory while each keeps a
// private workspace: WorkDir and the base for relative allowed_paths follow
// workdir, while .a2a state (codex-home) stays under workspace.
func TestBuildSessionConfig_WorkdirDecoupledFromWorkspace(t *testing.T) {
	workspace := t.TempDir() // private .a2a state lives here
	workdir := t.TempDir()   // shared cwd

	cfg, err := buildSessionConfig("teacher", wrapper.RuntimeConfig{
		Provider: "anthropic",
		Command:  "claude",
	}, wrapper.FilesystemConfig{
		Enabled:      true,
		AllowedPaths: []string{"."}, // relative → resolves against workdir
	}, wrapper.MCPConfig{}, wrapper.StreamingConfig{}, workspace, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkDir != workdir {
		t.Fatalf("WorkDir = %q, want workdir %q (not workspace)", cfg.WorkDir, workdir)
	}
	joined := strings.Join(cfg.Args, " ")
	if !strings.Contains(joined, "--add-dir="+workdir) {
		t.Fatalf("relative allowed_path should resolve against workdir; args=%v", cfg.Args)
	}
	if strings.Contains(joined, "--add-dir="+workspace) {
		t.Fatalf("relative allowed_path must NOT resolve against workspace; args=%v", cfg.Args)
	}
}
