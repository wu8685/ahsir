package scheduler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ahsir.yaml")
	yamlContent := `
agents:
  - name: backend
    workspace: /tmp/backend
    port: 0
  - name: frontend
    workspace: /tmp/frontend
    port: 9802

registry:
  host: "127.0.0.1"
  port: 9800
  heartbeat_interval: 10s
  heartbeat_timeout: 30s

port_range:
  start: 9801
  end: 9900
`
	os.WriteFile(configPath, []byte(yamlContent), 0644)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "backend" {
		t.Errorf("expected backend, got %s", cfg.Agents[0].Name)
	}
	if cfg.Agents[1].Port != 9802 {
		t.Errorf("expected port 9802, got %d", cfg.Agents[1].Port)
	}
	if cfg.Registry.Port != 9800 {
		t.Errorf("expected registry port 9800, got %d", cfg.Registry.Port)
	}
	if cfg.PortRange.Start != 9801 || cfg.PortRange.End != 9900 {
		t.Errorf("unexpected port range: %+v", cfg.PortRange)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ahsir.yaml")
	yamlContent := `
agents:
  - name: backend
    workspace: /tmp/backend
`
	os.WriteFile(configPath, []byte(yamlContent), 0644)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Registry.Host != "127.0.0.1" {
		t.Errorf("expected default host 127.0.0.1, got %s", cfg.Registry.Host)
	}
	if cfg.Registry.Port != 9800 {
		t.Errorf("expected default port 9800, got %d", cfg.Registry.Port)
	}
	if cfg.Registry.HeartbeatInterval != "10s" {
		t.Errorf("expected default heartbeat_interval 10s, got %s", cfg.Registry.HeartbeatInterval)
	}
	if cfg.PortRange.Start != 9801 {
		t.Errorf("expected default port_start 9801, got %d", cfg.PortRange.Start)
	}
	if cfg.PortRange.End != 9900 {
		t.Errorf("expected default port_end 9900, got %d", cfg.PortRange.End)
	}
}

func TestConfigAllocatePort(t *testing.T) {
	cfg := &Config{
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	port1, err := cfg.AllocatePort()
	if err != nil {
		t.Fatal(err)
	}
	if port1 < 9801 || port1 > 9900 {
		t.Errorf("port %d out of range", port1)
	}

	port2, _ := cfg.AllocatePort()
	if port2 == port1 {
		t.Error("expected different ports")
	}
}
