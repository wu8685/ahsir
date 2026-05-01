package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestNewScheduler(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: 9800,
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	if sch.Registry() == nil {
		t.Error("expected non-nil registry")
	}
}

func TestSchedulerListAgents(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: 9800,
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	agents := sch.ListAgents()
	if agents == nil {
		t.Error("expected non-nil agent list")
	}
}

func TestSchedulerChatWithAgent(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: 9800,
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)

	_, err := sch.ChatWithAgent("nonexistent", "hello")
	if err == nil {
		t.Error("expected error for non-existent agent")
	}

	sch.Registry().Register(&a2a.AgentCard{
		Name:    "test-agent",
		Version: "1.0.0",
		URL:     "http://127.0.0.1:9801/",
	})
	resp, err := sch.ChatWithAgent("test-agent", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if resp == "" {
		t.Error("expected non-empty response")
	}
}

func TestSchedulerStartStop(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: 9800,
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	sch.Stop()
}

func TestSchedulerGetTaskStatus(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: 9800,
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	sch.Registry().Register(&a2a.AgentCard{
		Name: "test-agent",
		URL:  "http://127.0.0.1:9801/",
	})

	task, err := sch.GetTaskStatus("test-agent", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(task.ID) != "task-1" {
		t.Errorf("expected task-1, got %s", task.ID)
	}
}
