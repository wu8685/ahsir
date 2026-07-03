package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if cfg.PortRange.Start != 9802 {
		t.Errorf("expected default port_start 9802, got %d", cfg.PortRange.Start)
	}
	if cfg.PortRange.End != 9900 {
		t.Errorf("expected default port_end 9900, got %d", cfg.PortRange.End)
	}
}

func TestLoadConfigPortRangeValidation(t *testing.T) {
	cases := []struct {
		name       string
		portRange  string
		wantErr    bool
		errSubstrs []string
	}{
		{
			name:      "valid range loads",
			portRange: "port_range:\n  start: 9801\n  end: 9900\n",
			wantErr:   false,
		},
		{
			name:       "start greater than end is rejected",
			portRange:  "port_range:\n  start: 9900\n  end: 9801\n",
			wantErr:    true,
			errSubstrs: []string{"9900", "9801"},
		},
		{
			name:       "non-positive start is rejected",
			portRange:  "port_range:\n  start: 0\n  end: 9900\n",
			wantErr:    true,
			errSubstrs: []string{"start", "0"},
		},
		{
			name:       "negative start is rejected",
			portRange:  "port_range:\n  start: -5\n  end: 9900\n",
			wantErr:    true,
			errSubstrs: []string{"start", "-5"},
		},
		{
			name:       "end above max port is rejected",
			portRange:  "port_range:\n  start: 9801\n  end: 70000\n",
			wantErr:    true,
			errSubstrs: []string{"70000", "65535"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "ahsir.yaml")
			yamlContent := "agents: []\n" + tc.portRange
			if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
				t.Fatal(err)
			}

			cfg, err := LoadConfig(configPath)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %s, got nil (cfg=%+v)", tc.name, cfg)
				}
				for _, sub := range tc.errSubstrs {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q does not name %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if cfg.nextPort != cfg.PortRange.Start {
				t.Errorf("nextPort = %d, want %d", cfg.nextPort, cfg.PortRange.Start)
			}
		})
	}
}

func TestLoadConfigSetsInvocationLedgerPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ahsir.yaml")
	yamlContent := `
agents: []
`
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(dir, ".ahsir", "ledger.jsonl")
	if cfg.InvocationLedgerPath() != want {
		t.Fatalf("ledger path = %q, want %q", cfg.InvocationLedgerPath(), want)
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

func TestTimeoutsConfigChatTimeoutZeroDisablesDeadline(t *testing.T) {
	cfg := TimeoutsConfig{Chat: "0s"}
	if got := cfg.ChatTimeout(); got != 0 {
		t.Fatalf("expected chat timeout 0 to mean no scheduler deadline, got %s", got)
	}

	cfg = TimeoutsConfig{}
	if got := cfg.ChatTimeout(); got != 10*time.Minute {
		t.Fatalf("expected empty chat timeout to use default 10m, got %s", got)
	}
}
