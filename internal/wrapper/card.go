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
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Version     string            `yaml:"version"`
	Provider    *ProviderConfig   `yaml:"provider"`
	Skills      []SkillConfig     `yaml:"skills"`
	Claude      ClaudeConfig      `yaml:"claude"`
	Network     NetworkConfig     `yaml:"network"`
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

// ClaudeConfig holds Claude-specific settings from the card.
type ClaudeConfig struct {
	SystemPrompt  string `yaml:"systemPrompt"`
	MaxAgentCalls int    `yaml:"maxAgentCalls"`
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
		Name:        cfg.Name,
		Description: cfg.Description,
		Version:     cfg.Version,
		URL:         fmt.Sprintf("http://%s:%d/", advertise, port),
		Skills:      skills,
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
