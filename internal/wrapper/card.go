package wrapper

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/a2aproject/a2a-go/a2a"
	"gopkg.in/yaml.v3"
)

// AgentCardConfig represents the .a2a/agent-card.yaml file structure.
type AgentCardConfig struct {
	Name        string           `yaml:"name"`
	Description string           `yaml:"description"`
	Version     string           `yaml:"version"`
	Provider    *ProviderConfig  `yaml:"provider"`
	Skills      []SkillConfig    `yaml:"skills"`
	// Claude holds agent behavior (system prompt, max delegation depth).
	// The field is named "Claude" for historical reasons; its contents are
	// provider-agnostic — any LLM CLI configured via Runtime can consume them.
	Claude     ClaudeConfig     `yaml:"claude"`
	Runtime    RuntimeConfig    `yaml:"runtime"`
	Network    NetworkConfig    `yaml:"network"`
	Filesystem FilesystemConfig `yaml:"filesystem"`
}

// ProviderConfig maps to a2a.AgentProvider.
type ProviderConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// SkillConfig maps a skill definition.
type SkillConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// ClaudeConfig holds agent-behavior settings (system prompt + max delegation depth).
// Despite the name, the contents are not Claude-specific.
type ClaudeConfig struct {
	SystemPrompt  string `yaml:"systemPrompt"`
	MaxAgentCalls int    `yaml:"maxAgentCalls"`
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
//   - any other value: provider mapping is skipped; user must populate Env
//     (and likely Command) directly.
//
// Value expansion: BaseURL, APIKey, Model, and every value in Env support
// ${VAR} / $VAR expansion via os.ExpandEnv, so secrets can live in shell
// env instead of YAML.
//
// Timeout has the form accepted by time.ParseDuration (e.g. "120s", "2m").
type RuntimeConfig struct {
	Provider string            `yaml:"provider"`
	BaseURL  string            `yaml:"baseURL"`
	APIKey   string            `yaml:"apiKey"`
	Model    string            `yaml:"model"`
	Command  string            `yaml:"command"`
	Args     []string          `yaml:"args"`
	Env      map[string]string `yaml:"env"`
	Timeout  string            `yaml:"timeout"`
}

// FilesystemConfig holds filesystem tool configuration from agent-card.yaml.
type FilesystemConfig struct {
	Enabled      bool     `yaml:"enabled"`
	AllowedPaths []string `yaml:"allowed_paths"`
}

// NetworkConfig holds network settings from the card.
type NetworkConfig struct {
	Bind      string `yaml:"bind"`
	Advertise string `yaml:"advertise"`
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
	if cfg.Claude.MaxAgentCalls == 0 {
		cfg.Claude.MaxAgentCalls = 5
	}
	if cfg.Filesystem.Enabled && len(cfg.Filesystem.AllowedPaths) == 0 {
		cfg.Filesystem.AllowedPaths = []string{"."}
	}
	if cfg.Runtime.Command == "" {
		cfg.Runtime.Command = "claude"
		if len(cfg.Runtime.Args) == 0 {
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
