package scheduler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/registry"
	"github.com/wu8685/ahsir/internal/wrapper"
)

// Scheduler manages the lifecycle of all agents.
type Scheduler struct {
	cfg      *Config
	registry *registry.Registry
	agents   map[string]*agentProcess
	httpSrv  *http.Server
	mu       sync.Mutex
	running  bool
	cancel   context.CancelFunc
}

type agentProcess struct {
	cfg    AgentConfig
	cmd    *exec.Cmd
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
	handler := registry.NewHTTPHandler(s.registry)
	addr := fmt.Sprintf("%s:%d", s.cfg.Registry.Host, s.cfg.Registry.Port)
	s.httpSrv = &http.Server{Addr: addr, Handler: handler}
	go func() {
		log.Printf("Registry listening on %s", addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Registry server: %v", err)
		}
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

	// Find the ahsir-agent binary
	agentExe := s.agentBinary()
	registryURL := fmt.Sprintf("http://%s:%d", s.cfg.Registry.Host, s.cfg.Registry.Port)

	cmd := exec.CommandContext(agentCtx, agentExe,
		"--workspace", cfg.Workspace,
		"--port", strconv.Itoa(port),
		"--registry", registryURL,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start agent %s: %w", cfg.Name, err)
	}

	s.agents[cfg.Name] = &agentProcess{
		cfg:    cfg,
		cmd:    cmd,
		cancel: cancel,
	}

	log.Printf("Agent %s started on port %d (pid: %d)", cfg.Name, port, cmd.Process.Pid)

	// Monitor process exit
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("Agent %s exited: %v", cfg.Name, err)
		}
		s.mu.Lock()
		delete(s.agents, cfg.Name)
		s.mu.Unlock()
	}()

	return nil
}

// agentBinary returns the path to the ahsir-agent binary.
func (s *Scheduler) agentBinary() string {
	exePath, err := os.Executable()
	if err != nil {
		return "ahsir-agent"
	}
	return filepath.Join(filepath.Dir(exePath), "ahsir-agent")
}

// Stop stops all agents and the scheduler.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}

	// Shut down registry HTTP server
	if s.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpSrv.Shutdown(ctx); err != nil {
			log.Printf("Registry shutdown: %v", err)
		}
	}

	for name, proc := range s.agents {
		proc.cancel()
		if proc.cmd != nil && proc.cmd.Process != nil {
			proc.cmd.Process.Kill()
		}
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := wrapper.NewAgentClient(ctx, card)
	if err != nil {
		return "", fmt.Errorf("create client for %s: %w", agentName, err)
	}

	return client.SendMessage(ctx, message)
}

// GetTaskStatus gets a task's status (implements mcp.AgentRouter).
func (s *Scheduler) GetTaskStatus(agentName, taskID string) (*a2a.Task, error) {
	card, ok := s.registry.Get(agentName)
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := wrapper.NewAgentClient(ctx, card)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", agentName, err)
	}

	return client.GetTask(ctx, taskID)
}
