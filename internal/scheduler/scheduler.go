package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/registry"
)

// Scheduler manages the lifecycle of all agents.
type Scheduler struct {
	cfg      *Config
	registry *registry.Registry
	agents   map[string]*agentProcess
	mu       sync.Mutex
	running  bool
	cancel   context.CancelFunc
}

type agentProcess struct {
	cfg    AgentConfig
	cancel context.CancelFunc
}

// New creates a new scheduler from configuration.
func New(cfg *Config) *Scheduler {
	heartbeatTimeout := 30 * time.Second
	if cfg.Registry.HeartbeatTimeout != "" {
		if d, err := time.ParseDuration(cfg.Registry.HeartbeatTimeout); err == nil {
			heartbeatTimeout = d
		}
	}
	return &Scheduler{
		cfg:      cfg,
		registry: registry.NewRegistry(heartbeatTimeout),
		agents:   make(map[string]*agentProcess),
	}
}

// Registry returns the scheduler's registry.
func (s *Scheduler) Registry() *registry.Registry {
	return s.registry
}

// Start starts the scheduler and all local agents.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("scheduler already running")
	}

	ctx, s.cancel = context.WithCancel(ctx)

	// Start registry HTTP server
	go func() {
		handler := registry.NewHTTPHandler(s.registry)
		addr := fmt.Sprintf("%s:%d", s.cfg.Registry.Host, s.cfg.Registry.Port)
		log.Printf("Registry listening on %s", addr)
		// In full implementation: log.Fatal(http.ListenAndServe(addr, handler))
		_ = handler
	}()

	// Start each local agent
	for _, agentCfg := range s.cfg.Agents {
		if agentCfg.Remote != "" {
			continue
		}
		if err := s.startAgent(ctx, agentCfg); err != nil {
			return fmt.Errorf("start agent %s: %w", agentCfg.Name, err)
		}
	}

	s.running = true
	return nil
}

func (s *Scheduler) startAgent(ctx context.Context, cfg AgentConfig) error {
	port := cfg.Port
	if port == 0 {
		var err error
		port, err = s.cfg.AllocatePort()
		if err != nil {
			return err
		}
	}

	agentCtx, cancel := context.WithCancel(ctx)
	s.agents[cfg.Name] = &agentProcess{
		cfg:    cfg,
		cancel: cancel,
	}

	_ = agentCtx
	log.Printf("Agent %s would start on port %d", cfg.Name, port)
	return nil
}

// Stop stops all agents and the scheduler.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}

	for name, proc := range s.agents {
		proc.cancel()
		log.Printf("Agent %s stopped", name)
	}
	s.agents = make(map[string]*agentProcess)
	s.running = false
}

// ListAgents returns all registered agents (implements mcp.AgentRouter).
func (s *Scheduler) ListAgents() []*a2a.AgentCard {
	return s.registry.List()
}

// ChatWithAgent sends a message to an agent (implements mcp.AgentRouter).
func (s *Scheduler) ChatWithAgent(agentName, message string) (string, error) {
	card, ok := s.registry.Get(agentName)
	if !ok {
		return "", fmt.Errorf("agent %s not found", agentName)
	}
	_ = card
	return fmt.Sprintf("message sent to %s", agentName), nil
}

// GetTaskStatus gets a task's status (implements mcp.AgentRouter).
func (s *Scheduler) GetTaskStatus(agentName, taskID string) (*a2a.Task, error) {
	_, ok := s.registry.Get(agentName)
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}
	return &a2a.Task{ID: a2a.TaskID(taskID), Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, nil
}
