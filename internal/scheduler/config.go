package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Default timeouts for the gateway / outer-envelope layer. The gateway
// timeout is the upper bound on a single agent chat round-trip and MUST be
// >= every agent's runtime.timeout (in agent-card.yaml); 10 minutes covers
// the per-agent default of 300s with headroom for slow models, hook
// overhead, and chained sub-agent calls. task_status is a quick task-store
// read, no LLM involvement.
const (
	defaultChatTimeout       = 10 * time.Minute
	defaultTaskStatusTimeout = 30 * time.Second
)

// Config represents the ahsir.yaml configuration.
type Config struct {
	Agents    []AgentConfig  `yaml:"agents"`
	Registry  RegistryConfig `yaml:"registry"`
	PortRange PortRange      `yaml:"port_range"`
	Timeouts  TimeoutsConfig `yaml:"timeouts"`

	mu       sync.Mutex
	nextPort int
	path     string
}

// TimeoutsConfig is the single source of truth for the scheduler's
// outer-envelope timeouts. Per-agent LLM subprocess timeout (claude exit
// deadline) lives separately in each agent-card.yaml's runtime.timeout
// because it is intrinsic to that agent's expected response latency.
//
// Invariant: Chat must be >= max(agent runtime.timeout) across all agents,
// otherwise the gateway will kill a request the agent could still complete.
type TimeoutsConfig struct {
	// Chat bounds POST /agents/{name}/chat (the JSON-RPC forward to an
	// agent's A2A server). Default: 10m.
	Chat string `yaml:"chat"`
	// TaskStatus bounds GET /agents/{name}/tasks/{taskID}. Default: 30s.
	TaskStatus string `yaml:"task_status"`
}

// ChatTimeout returns the parsed Chat timeout, or the default if empty/invalid.
// A configured duration of 0 disables the scheduler gateway deadline for chat
// requests, which is useful for explicitly long-running agent work.
func (t TimeoutsConfig) ChatTimeout() time.Duration {
	if t.Chat == "" {
		return defaultChatTimeout
	}
	if d, err := time.ParseDuration(t.Chat); err == nil {
		return d
	}
	return defaultChatTimeout
}

// TaskStatusTimeout returns the parsed TaskStatus timeout, or the default.
func (t TimeoutsConfig) TaskStatusTimeout() time.Duration {
	if t.TaskStatus == "" {
		return defaultTaskStatusTimeout
	}
	if d, err := time.ParseDuration(t.TaskStatus); err == nil {
		return d
	}
	return defaultTaskStatusTimeout
}

// AgentConfig configures a single agent.
type AgentConfig struct {
	Name          string `yaml:"name"`
	Workspace     string `yaml:"workspace"`
	Port          int    `yaml:"port"`
	Remote        string `yaml:"remote,omitempty"`
	InternalToken string `yaml:"-"`
}

// RegistryConfig configures the registry.
type RegistryConfig struct {
	Host              string `yaml:"host"`
	Port              int    `yaml:"port"`
	HeartbeatInterval string `yaml:"heartbeat_interval"`
	HeartbeatTimeout  string `yaml:"heartbeat_timeout"`
}

// PortRange defines the auto-allocation port range.
type PortRange struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

// LoadConfig reads and parses ahsir.yaml.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Registry: RegistryConfig{
			Host:              "127.0.0.1",
			Port:              9800,
			HeartbeatInterval: "10s",
			HeartbeatTimeout:  "30s",
		},
		PortRange: PortRange{
			Start: 9801,
			End:   9900,
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.nextPort = cfg.PortRange.Start
	if abs, err := filepath.Abs(path); err == nil {
		cfg.path = abs
	} else {
		cfg.path = path
	}
	return cfg, nil
}

// InvocationLedgerPath returns the default persistent scheduler ledger path.
// Empty means the config was constructed in memory and should use an in-memory
// ledger unless tests or callers explicitly provide one.
func (c *Config) InvocationLedgerPath() string {
	if c == nil || c.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.path), ".ahsir", "ledger.jsonl")
}

// AllocatePort allocates the next available port from the range.
func (c *Config) AllocatePort() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nextPort > c.PortRange.End {
		return 0, fmt.Errorf("no available ports in range %d-%d", c.PortRange.Start, c.PortRange.End)
	}
	port := c.nextPort
	c.nextPort++
	return port, nil
}
