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
	// ctx is the scheduler-lifetime context derived inside Start. It is
	// the parent of every agent's per-process context — needed so
	// post-boot StartAgent calls (from the admin API) can spawn children
	// of the same lifecycle as boot-time agents.
	ctx    context.Context
	cancel context.CancelFunc
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
	s.ctx = ctx

	// Wrap the registry handler with a gateway router that intercepts the
	// chat / tasks endpoints first and forwards everything else (the bare
	// /agents and /agents/{name} CRUD) to the registry. We do path parsing
	// manually rather than relying on Go 1.22+ ServeMux pattern routing
	// because the build environment may pin httpmuxgo121=1, in which case
	// `{name}` wildcards become literal strings and never match.
	regHandler := registry.NewHTTPHandler(s.registry)
	gw := newGatewayHandler(s, regHandler)

	addr := fmt.Sprintf("%s:%d", s.cfg.Registry.Host, s.cfg.Registry.Port)
	s.httpSrv = &http.Server{Addr: addr, Handler: gw}
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

// StartAgent spins up a new agent post-Start (callable from the admin
// HTTP endpoint). The scheduler must already be running. cfg.Port=0
// allocates from the configured range; cfg.Name must be unique among
// running agents.
//
// Returns the allocated port so callers (CLI / HTTP admin) can report it.
// Unlike the boot-time startAgent loop, this acquires s.mu so it's safe
// to invoke from a request goroutine.
func (s *Scheduler) StartAgent(cfg AgentConfig) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return 0, fmt.Errorf("scheduler not running")
	}
	if cfg.Name == "" {
		return 0, fmt.Errorf("agent name is required")
	}
	if cfg.Workspace == "" {
		return 0, fmt.Errorf("agent workspace is required")
	}
	if _, exists := s.agents[cfg.Name]; exists {
		return 0, fmt.Errorf("agent %q already running", cfg.Name)
	}

	if err := s.startAgent(s.ctx, cfg); err != nil {
		return 0, err
	}
	return s.agents[cfg.Name].cfg.Port, nil
}

// StopAgent tears down a running agent. Idempotent on "not running" —
// returns nil if the name isn't in the map. Files in the workspace are
// preserved (this is dynamic deregistration only). To remove files,
// the caller (CLI) handles that separately.
func (s *Scheduler) StopAgent(name string) error {
	s.mu.Lock()
	proc, ok := s.agents[name]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	// Cancel ctx first so exec.CommandContext kills the process, then
	// release the lock — the monitor goroutine in startAgent will see
	// the process exit, reacquire s.mu, and delete from the map.
	proc.cancel()
	s.mu.Unlock()

	// Best-effort unregister from registry so subsequent /agents listing
	// doesn't show the now-stopped agent.
	_ = s.registry.Unregister(name)
	return nil
}

// startAgent is the unexported per-agent spawn. Both Start (boot loop)
// and StartAgent (admin endpoint) funnel through it. The caller must
// hold s.mu so the agents map mutation is atomic with the exec.Start.
//
// IMPORTANT: cfg.Port is mutated to record the actually-allocated port
// before being stored in s.agents — callers reading s.agents[name].cfg.Port
// rely on this.
func (s *Scheduler) startAgent(ctx context.Context, cfg AgentConfig) error {
	port := cfg.Port
	if port == 0 {
		var err error
		port, err = s.cfg.AllocatePort()
		if err != nil {
			return err
		}
	}
	// Persist the resolved port into cfg so s.agents[name].cfg.Port reflects
	// the actually-allocated value (callers — admin API, tests — read this).
	cfg.Port = port

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
//
// The forwarding timeout comes from cfg.Timeouts.Chat (default 10m). It MUST
// be >= every agent's runtime.timeout (configured per agent-card.yaml),
// because the scheduler has to wait for the agent's full LLM round-trip
// before getting a reply. The agent itself is still the authoritative
// per-call deadline — the gateway timeout is just an upper bound to avoid
// hanging callers if an agent never responds.
func (s *Scheduler) ChatWithAgent(agentName, contextID, message string) (string, error) {
	card, ok := s.registry.Get(agentName)
	if !ok {
		return "", fmt.Errorf("agent %s not found", agentName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.ChatTimeout())
	defer cancel()

	client, err := wrapper.NewAgentClient(ctx, card)
	if err != nil {
		return "", fmt.Errorf("create client for %s: %w", agentName, err)
	}

	// contextID is propagated when the caller wants session reuse across
	// multiple chats (e.g. CLI users with --context, or MCP tool callers
	// passing a contextId). Empty string means each call is isolated —
	// the agent's executor will auto-generate a fresh contextID for the task.
	return client.SendMessage(ctx, contextID, message)
}

// GetTaskStatus gets a task's status (implements mcp.AgentRouter).
//
// Uses cfg.Timeouts.TaskStatus (default 30s) — this is a quick task-store
// read with no LLM round-trip, so it can be tight.
func (s *Scheduler) GetTaskStatus(agentName, taskID string) (*a2a.Task, error) {
	card, ok := s.registry.Get(agentName)
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.TaskStatusTimeout())
	defer cancel()

	client, err := wrapper.NewAgentClient(ctx, card)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", agentName, err)
	}

	return client.GetTask(ctx, taskID)
}
