package scheduler

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// Config represents the ahsir.yaml configuration.
type Config struct {
	Agents    []AgentConfig  `yaml:"agents"`
	Registry  RegistryConfig `yaml:"registry"`
	MCP       MCPConfig      `yaml:"mcp"`
	PortRange PortRange      `yaml:"port_range"`

	mu       sync.Mutex
	nextPort int
}

// AgentConfig configures a single agent.
type AgentConfig struct {
	Name      string `yaml:"name"`
	Workspace string `yaml:"workspace"`
	Port      int    `yaml:"port"`
	Remote    string `yaml:"remote,omitempty"`
}

// RegistryConfig configures the registry.
type RegistryConfig struct {
	Host              string `yaml:"host"`
	Port              int    `yaml:"port"`
	HeartbeatInterval string `yaml:"heartbeat_interval"`
	HeartbeatTimeout  string `yaml:"heartbeat_timeout"`
}

// MCPConfig configures the MCP server.
type MCPConfig struct{}

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
	return cfg, nil
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
