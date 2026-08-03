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
	Name      string `yaml:"name"`
	Workspace string `yaml:"workspace"`
	// Workdir is the agent's working directory (the claude CLI cwd and the base
	// for relative filesystem.allowed_paths). It defaults to Workspace when
	// empty. Decoupling it lets several agents — each with its own private
	// Workspace (where .a2a/ card+sessions+transcripts live) — share one cwd,
	// e.g. a common project/knowledge directory.
	Workdir string `yaml:"workdir,omitempty"`
	Port    int    `yaml:"port"`
	// Instances caps how many concurrent runtime instances this one agent card
	// may back (issue #18). Each instance beyond the first is a separate
	// ahsir-agent process with its own isolated workspace (inst-<n>), so
	// concurrent sessions dispatched to the same card no longer clobber a shared
	// working tree. Zero or 1 means the historical "one card = one worker"
	// behavior — a single instance, byte-identical to before.
	Instances     int    `yaml:"instances,omitempty"`
	Remote        string `yaml:"remote,omitempty"`
	InternalToken string `yaml:"-"`
	// AdminToken is the scheduler's control-plane token, handed to the agent
	// so its registry heartbeat authenticates. Runtime-only, never persisted.
	AdminToken string `yaml:"-"`
}

// InstanceCap returns the effective concurrent-instance cap for this agent:
// at least 1. Zero/negative Instances (the unset default) collapses to a single
// instance, preserving the historical one-card-one-worker behavior.
func (c AgentConfig) InstanceCap() int {
	if c.Instances < 1 {
		return 1
	}
	return c.Instances
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

// validate rejects a nonsensical port_range at config-load time so a
// misconfiguration surfaces up front with an actionable message, rather than
// much later at runtime as an opaque "no available ports in range" error from
// AllocatePort. A port must be a positive TCP port number, and the range must
// be non-empty (start <= end).
func (p PortRange) validate() error {
	if p.Start <= 0 {
		return fmt.Errorf("invalid port_range: start %d must be a positive port number", p.Start)
	}
	if p.End > 65535 {
		return fmt.Errorf("invalid port_range: end %d exceeds the maximum port number 65535", p.End)
	}
	if p.Start > p.End {
		return fmt.Errorf("invalid port_range: start %d is greater than end %d", p.Start, p.End)
	}
	return nil
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
			Start: 9802, // 9800 = registry, 9801 = web console (`ahsir ui`); agents start at 9802
			End:   9900,
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.PortRange.validate(); err != nil {
		return nil, err
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

// RoomsDir is the directory holding persisted roundtable rooms — one
// <roomID>.jsonl per room (mirrors the per-agent transcript store). Empty when
// the config has no file path (in-memory config), which disables persistence.
func (c *Config) RoomsDir() string {
	if c == nil || c.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.path), ".ahsir", "rooms")
}

// ManagedAgentWorkspace returns the workspace directory allocated for a
// dynamically registered agent that supplied an inline card but no workspace
// of its own (the cma-service style: one ahsir agent per CMA agent version).
// The .a2a/ card + sessions + transcripts live under here. Empty when the
// config is in-memory (no file path), in which case the caller must require an
// explicit workspace.
func (c *Config) ManagedAgentWorkspace(name string) string {
	dir := c.ManagedAgentsDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

// ManagedAgentsDir is the root under which managed agent workspaces live —
// .ahsir/agents/<name>. It is the discovery surface for archived/offline agents:
// a workspace here whose agent is no longer registered still holds replayable
// transcripts. Empty when the config is in-memory (no file path).
func (c *Config) ManagedAgentsDir() string {
	if c == nil || c.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(c.path), ".ahsir", "agents")
}

// AllocatePort returns the next candidate from the dynamic port range. The
// cursor wraps so released ports can be considered again; the Scheduler owns
// availability checks and bounds each scan to one complete pass over the
// range.
func (c *Config) AllocatePort() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.PortRange.validate(); err != nil {
		return 0, err
	}
	if c.nextPort < c.PortRange.Start || c.nextPort > c.PortRange.End {
		c.nextPort = c.PortRange.Start
	}
	port := c.nextPort
	if c.nextPort == c.PortRange.End {
		c.nextPort = c.PortRange.Start
	} else {
		c.nextPort++
	}
	return port, nil
}
