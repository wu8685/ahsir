package wrapper

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/a2aproject/a2a-go/a2a"
	"gopkg.in/yaml.v3"
)

// AgentCardConfig represents the .a2a/agent-card.yaml file structure.
//
// Every field carries a json tag equal to its yaml key so the card can also be
// supplied inline over JSON — the dynamic-registration path (POST /admin/agents
// with a `card` body) decodes JSON into this struct, then persists it as YAML
// via WriteCard. Keep the two tag sets in lockstep.
type AgentCardConfig struct {
	Name        string          `yaml:"name" json:"name"`
	Description string          `yaml:"description" json:"description"`
	Version     string          `yaml:"version" json:"version"`
	Provider    *ProviderConfig `yaml:"provider" json:"provider"`
	Skills      []SkillConfig   `yaml:"skills" json:"skills"`
	// Claude holds agent behavior (system prompt, max delegation depth).
	// The field is named "Claude" for historical reasons; its contents are
	// provider-agnostic — any LLM CLI configured via Runtime can consume them.
	Claude     ClaudeConfig     `yaml:"claude" json:"claude"`
	Runtime    RuntimeConfig    `yaml:"runtime" json:"runtime"`
	Network    NetworkConfig    `yaml:"network" json:"network"`
	Filesystem FilesystemConfig `yaml:"filesystem" json:"filesystem"`
	Pool       PoolConfig       `yaml:"pool" json:"pool"`
	Streaming  StreamingConfig  `yaml:"streaming" json:"streaming"`
	MCP        MCPConfig        `yaml:"mcp" json:"mcp"`
}

// MCPConfig declares the MCP servers an agent's claude process may use.
//
// ahsir always runs claude with --strict-mcp-config, so an agent NEVER inherits
// the operator's global (~/.claude.json) or project (.mcp.json) MCP
// configuration. Only the servers declared here are loaded. The empty default
// (no `mcp:` block) therefore means the agent runs with NO MCP servers at all —
// the isolated, fast, cheap baseline. (Inheriting the operator's MCP servers
// was measured to balloon a trivial turn to tens of thousands of input tokens
// and add seconds of per-call init; it's also a privilege-bleed risk.)
//
// Each Servers entry is passed through verbatim as a claude `mcpServers` entry,
// so any server shape claude supports works without ahsir tracking its schema —
// stdio (command/args/env) or remote (type/url/headers). Example:
//
//	mcp:
//	  servers:
//	    filesystem:
//	      command: npx
//	      args: ["-y", "@modelcontextprotocol/server-filesystem", "/data"]
//	    docs:
//	      type: http
//	      url: https://example.com/mcp
//
// Only supported for claude-backed agents; codex configures MCP through its own
// config.toml under the isolated CODEX_HOME.
type MCPConfig struct {
	Servers map[string]any `yaml:"servers" json:"servers"`
}

// StreamingConfig toggles per-turn partial-message emission. Off by default
// to preserve the historical single-EventText behavior.
//
// Example:
//
//	streaming:
//	  partial_messages: true
//
// When enabled and the runtime is claude, buildSessionConfig appends
// --include-partial-messages so claude emits stream_event NDJSON lines with
// content_block_delta payloads. ClaudeSession surfaces them as
// EventTextDelta and the A2A server's OnSendMessageStream relays them as
// TaskStatusUpdateEvents to subscribers.
type StreamingConfig struct {
	// PartialMessages enables incremental delta delivery. Required for
	// useful output on `message/stream` JSON-RPC calls; `message/send`
	// callers continue to get the final aggregated text regardless.
	PartialMessages bool `yaml:"partial_messages" json:"partial_messages"`
}

// PoolConfig caps the agent's SessionPool. Optional — all fields default
// to zero / empty which leaves the pool unbounded (the historical
// behaviour). Wire via wrapper.SessionPool.SetCap in main.go after pool
// construction.
//
// Example:
//
//	pool:
//	  max_active: 50
//	  max_evicted: 1000
//	  idle_ttl: 30m
//	  evicted_ttl: 30d
//	  overload_policy: reject  # or "evict-lru"
type PoolConfig struct {
	// MaxActive is the maximum number of ACTIVE entries (live claude
	// processes) the pool will hold concurrently. 0 means unlimited.
	// EVICTED entries (sessionID retained, process gone) don't count.
	MaxActive int `yaml:"max_active" json:"max_active"`

	// OverloadPolicy is "reject" (default — error on cap) or
	// "evict-lru" (kick out the LRU active entry). Validated via
	// wrapper.ParseOverloadPolicy at startup so a typo doesn't silently
	// fall back to the default.
	OverloadPolicy string `yaml:"overload_policy" json:"overload_policy"`

	// IdleTTL controls when an ACTIVE session is closed and moved to
	// EVICTED. Empty means the ahsir-agent default.
	IdleTTL string `yaml:"idle_ttl" json:"idle_ttl"`

	// EvictedTTL controls time-based deletion of inactive mappings. Empty
	// means the ahsir-agent default.
	EvictedTTL string `yaml:"evicted_ttl" json:"evicted_ttl"`

	// MaxEvicted bounds how many inactive mappings are retained for resume.
	// 0 means the ahsir-agent default.
	MaxEvicted int `yaml:"max_evicted" json:"max_evicted"`

	// QueueDepth bounds the per-context FIFO turn queue (shared-context
	// collaboration): how many turns may wait behind an in-flight turn on
	// one contextId before busy/409 rejections resume. Pointer so the
	// zero value is distinguishable: unset → default (4), explicit 0 →
	// no queueing, restore fail-fast busy.
	QueueDepth *int `yaml:"queue_depth" json:"queue_depth"`
}

// ProviderConfig maps to a2a.AgentProvider.
type ProviderConfig struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
}

// SkillConfig maps a skill definition.
type SkillConfig struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
}

// ClaudeConfig holds agent-behavior settings (system prompt + max delegation depth).
// Despite the name, the contents are not Claude-specific.
type ClaudeConfig struct {
	SystemPrompt  string `yaml:"systemPrompt" json:"systemPrompt"`
	MaxAgentCalls int    `yaml:"maxAgentCalls" json:"maxAgentCalls"`
}

// RuntimeConfig configures the LLM CLI subprocess that backs an agent.
// This is the multi-provider extension point.
//
// High-level fields (Provider, BaseURL, APIKey, Model) are the recommended
// way to switch providers — they get translated into the env vars the
// underlying CLI expects. Low-level fields (Command/Args/Env) are escape
// hatches for unusual setups.
//
// Provider values:
//   - "" or "anthropic" (default): drives `claude -p`, sets
//     ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN / ANTHROPIC_MODEL.
//   - "zhipu": same env mapping as anthropic (Zhipu/智谱 GLM exposes an
//     Anthropic-compatible endpoint), with BaseURL defaulting to
//     https://open.bigmodel.cn/api/anthropic.
//   - "codex": drives `codex exec --json`, maps APIKey to CODEX_API_KEY and
//     Model to --model.
//   - any other value: provider mapping is skipped; user must populate Env
//     (and likely Command) directly.
//
// Value expansion: BaseURL, APIKey, Model, and every value in Env support
// ${VAR} / $VAR expansion via os.ExpandEnv, so secrets can live in shell
// env instead of YAML.
//
// Timeout has the form accepted by time.ParseDuration (e.g. "120s", "2m").
type RuntimeConfig struct {
	Provider string            `yaml:"provider" json:"provider"`
	BaseURL  string            `yaml:"baseURL" json:"baseURL"`
	APIKey   string            `yaml:"apiKey" json:"apiKey"`
	Model    string            `yaml:"model" json:"model"`
	Command  string            `yaml:"command" json:"command"`
	Args     []string          `yaml:"args" json:"args"`
	Env      map[string]string `yaml:"env" json:"env"`
	Timeout  string            `yaml:"timeout" json:"timeout"`
}

// FilesystemConfig holds filesystem tool configuration from agent-card.yaml.
type FilesystemConfig struct {
	Enabled      bool     `yaml:"enabled" json:"enabled"`
	WriteAccess  bool     `yaml:"write_access" json:"write_access"`
	// ShellAccess is the explicit opt-in that adds the Bash tool to the
	// claude allowedTools whitelist. It is deliberately separate from
	// WriteAccess: granting file edits must not implicitly grant arbitrary
	// command execution. This is the card-schema knob that stripClaudePermissionBypassArgs
	// points to — the sanctioned way to widen a claude agent to shell, rather
	// than smuggling --dangerously-skip-permissions through runtime.args.
	ShellAccess  bool     `yaml:"shell_access" json:"shell_access"`
	AllowedPaths []string `yaml:"allowed_paths" json:"allowed_paths"`
}

// ResolveUploadDir reports the directory the web console copies dropped files
// into, and that agents auto-allow-list when filesystem access is enabled.
// Both the console (`ahsir ui`) and the agent process resolve it identically,
// so a file dropped in the composer is readable with zero config: the console
// writes it here, the agent already has it on its --add-dir list. Override on
// both sides at once with the AHSIR_UPLOAD_DIR env var; `ahsir ui --upload-dir`
// overrides only the console.
func ResolveUploadDir() string {
	if d := os.Getenv("AHSIR_UPLOAD_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "ahsir-uploads")
}

// NetworkConfig holds network settings from the card.
type NetworkConfig struct {
	Bind      string `yaml:"bind" json:"bind"`
	Advertise string `yaml:"advertise" json:"advertise"`
}

// isLoopbackBind reports whether the configured network.bind value names the
// loopback interface — the only listen target the wrapper supports today.
func isLoopbackBind(bind string) bool {
	switch bind {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

// AgentCardBuilder builds A2A AgentCards from workspace config.
type AgentCardBuilder struct {
	workspaceDir string
}

// NewAgentCardBuilder creates a new AgentCard builder.
func NewAgentCardBuilder(workspaceDir string) *AgentCardBuilder {
	return &AgentCardBuilder{workspaceDir: workspaceDir}
}

func (b *AgentCardBuilder) cardFile() string {
	return filepath.Join(b.workspaceDir, ".a2a", "agent-card.yaml")
}

// WriteCard persists cfg as <workspaceDir>/.a2a/agent-card.yaml, creating the
// .a2a directory if needed. It is the inverse of Load and backs dynamic
// (inline) agent registration: the scheduler decodes a JSON card from the
// admin API, then calls WriteCard so the agent subprocess can Load it normally
// at startup.
func WriteCard(workspaceDir string, cfg *AgentCardConfig) error {
	dir := filepath.Join(workspaceDir, ".a2a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create .a2a dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal agent-card.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-card.yaml"), data, 0o600); err != nil {
		return fmt.Errorf("write agent-card.yaml: %w", err)
	}
	return nil
}

// Load reads and parses the agent-card.yaml from the workspace.
func (b *AgentCardBuilder) Load() (*AgentCardConfig, error) {
	data, err := os.ReadFile(b.cardFile())
	if err != nil {
		return nil, fmt.Errorf("read agent-card.yaml: %w", err)
	}

	var cfg AgentCardConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse agent-card.yaml: %w", err)
	}

	if cfg.Network.Bind == "" {
		cfg.Network.Bind = "127.0.0.1"
	}
	// The wrapper only ever listens on loopback (see AgentWrapper.Start) —
	// agents carry no ingress auth of their own beyond the internal token,
	// so exposing them off-host is not supported. Reject non-loopback bind
	// values instead of silently ignoring them: a config that LOOKS like it
	// opened the agent to the network but didn't is worse than an error.
	if !isLoopbackBind(cfg.Network.Bind) {
		return nil, fmt.Errorf("network.bind %q: only loopback (127.0.0.1 / localhost / ::1) is supported until scheduler ingress auth lands", cfg.Network.Bind)
	}
	if cfg.Claude.MaxAgentCalls == 0 {
		cfg.Claude.MaxAgentCalls = 5
	}
	if cfg.Filesystem.Enabled && len(cfg.Filesystem.AllowedPaths) == 0 {
		cfg.Filesystem.AllowedPaths = []string{"."}
	}
	if cfg.Runtime.Command == "" {
		if RuntimeProvider(cfg.Runtime) == ProviderCodex {
			cfg.Runtime.Command = "codex"
		} else {
			cfg.Runtime.Command = "claude"
		}
		if cfg.Runtime.Command == "claude" && len(cfg.Runtime.Args) == 0 {
			cfg.Runtime.Args = []string{"-p", "--output-format", "text"}
		}
	}
	if cfg.Runtime.Timeout == "" {
		cfg.Runtime.Timeout = "120s"
	}

	return &cfg, nil
}

// BuildRuntime creates a runtime a2a.AgentCard with endpoint set from the port.
func (b *AgentCardBuilder) BuildRuntime(cfg *AgentCardConfig, port int) *a2a.AgentCard {
	bind := cfg.Network.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	advertise := cfg.Network.Advertise
	if advertise == "" {
		advertise = bind
	}

	skills := make([]a2a.AgentSkill, len(cfg.Skills))
	for i, s := range cfg.Skills {
		skills[i] = a2a.AgentSkill{
			ID:          s.Name,
			Name:        s.Name,
			Description: s.Description,
		}
	}

	card := &a2a.AgentCard{
		Name:               cfg.Name,
		Description:        cfg.Description,
		Version:            cfg.Version,
		URL:                fmt.Sprintf("http://%s:%d/", advertise, port),
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Skills:             skills,
		Capabilities: a2a.AgentCapabilities{
			Streaming: true,
		},
	}

	if cfg.Provider != nil {
		card.Provider = &a2a.AgentProvider{
			Org: cfg.Provider.Name,
			URL: cfg.Provider.URL,
		}
	} else {
		card.Provider = &a2a.AgentProvider{
			Org: "ahsir",
			URL: "https://github.com/wu8685/ahsir",
		}
	}
	if card.Version == "" {
		card.Version = "1.0.0"
	}

	return card
}
