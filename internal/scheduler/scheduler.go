package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	desired  map[string]AgentConfig
	ledger   *InvocationLedger
	httpSrv  *http.Server
	mu       sync.Mutex
	running  bool
	// ctx is the scheduler-lifetime context derived inside Start. It is
	// the parent of every agent's per-process context — needed so
	// post-boot StartAgent calls (from the admin API) can spawn children
	// of the same lifecycle as boot-time agents.
	ctx    context.Context
	cancel context.CancelFunc

	supervisor   supervisorConfig
	agentCommand agentCommandBuilder

	recoveryDispatch recoveryDispatcher
}

type agentProcess struct {
	cfg             AgentConfig
	cmd             *exec.Cmd
	cancel          context.CancelFunc
	stopping        bool
	restartAttempts int
	internalToken   string
}

type supervisorConfig struct {
	Enabled                bool
	InitialBackoff         time.Duration
	MaxBackoff             time.Duration
	HealthCheckEnabled     bool
	HealthStartupGrace     time.Duration
	HealthInterval         time.Duration
	HealthTimeout          time.Duration
	HealthFailureThreshold int
}

type agentCommandBuilder func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd
type recoveryDispatcher func(ctx context.Context, agentName, contextID, prompt string) (string, error)

const continuationPrompt = "You were restarted while working on a previous task in this session. Inspect the existing conversation context and continue the interrupted work from where it left off. If the prior task was already complete, briefly report that no further action is needed."

// New creates a new scheduler from configuration.
func New(cfg *Config) *Scheduler {
	heartbeatTimeout := 30 * time.Second
	if cfg.Registry.HeartbeatTimeout != "" {
		if d, err := time.ParseDuration(cfg.Registry.HeartbeatTimeout); err == nil {
			heartbeatTimeout = d
		}
	}
	ledger := NewInvocationLedger()
	if path := cfg.InvocationLedgerPath(); path != "" {
		if fileLedger, err := NewInvocationLedgerFromFile(path); err == nil {
			ledger = fileLedger
		} else {
			log.Printf("Invocation ledger persistence disabled path=%s err=%v", path, err)
		}
	}
	return &Scheduler{
		cfg:          cfg,
		registry:     registry.NewRegistry(heartbeatTimeout),
		agents:       make(map[string]*agentProcess),
		desired:      make(map[string]AgentConfig),
		ledger:       ledger,
		supervisor:   defaultSupervisorConfig(),
		agentCommand: defaultAgentCommand,
	}
}

func defaultSupervisorConfig() supervisorConfig {
	return supervisorConfig{
		Enabled:                true,
		InitialBackoff:         time.Second,
		MaxBackoff:             30 * time.Second,
		HealthCheckEnabled:     true,
		HealthStartupGrace:     5 * time.Second,
		HealthInterval:         5 * time.Second,
		HealthTimeout:          2 * time.Second,
		HealthFailureThreshold: 3,
	}
}

// Registry returns the scheduler's registry.
func (s *Scheduler) Registry() *registry.Registry {
	return s.registry
}

// Invocations returns the in-memory scheduler invocation ledger.
func (s *Scheduler) Invocations() *InvocationLedger {
	return s.ledger
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
		if err := s.startAgentLocked(ctx, agentCfg, 0); err != nil {
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

	if err := s.startAgentLocked(s.ctx, cfg, 0); err != nil {
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
		delete(s.desired, name)
		s.mu.Unlock()
		return nil
	}
	delete(s.desired, name)
	proc.stopping = true
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
func (s *Scheduler) startAgentLocked(ctx context.Context, cfg AgentConfig, restartAttempts int) error {
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
	if cfg.InternalToken == "" {
		token, err := newInternalToken()
		if err != nil {
			return fmt.Errorf("create internal token for %s: %w", cfg.Name, err)
		}
		cfg.InternalToken = token
	}

	agentCtx, cancel := context.WithCancel(ctx)

	// Find the ahsir-agent binary
	agentExe := s.agentBinary()
	registryURL := fmt.Sprintf("http://%s:%d", s.cfg.Registry.Host, s.cfg.Registry.Port)

	buildCommand := s.agentCommand
	if buildCommand == nil {
		buildCommand = defaultAgentCommand
	}
	cmd := buildCommand(agentCtx, agentExe, cfg, registryURL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start agent %s: %w", cfg.Name, err)
	}

	proc := &agentProcess{
		cfg:             cfg,
		cmd:             cmd,
		cancel:          cancel,
		restartAttempts: restartAttempts,
		internalToken:   cfg.InternalToken,
	}
	s.agents[cfg.Name] = proc
	s.desired[cfg.Name] = cfg

	log.Printf("Agent %s started on port %d (pid: %d)", cfg.Name, port, cmd.Process.Pid)

	// Monitor process exit
	go s.monitorAgent(proc)
	if s.supervisor.HealthCheckEnabled {
		go s.watchAgentHealth(agentCtx, proc)
	}

	return nil
}

func defaultAgentCommand(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
	args := []string{
		"--workspace", cfg.Workspace,
		"--port", strconv.Itoa(cfg.Port),
		"--registry", registryURL,
	}
	if cfg.InternalToken != "" {
		args = append(args, "--internal-token", cfg.InternalToken)
	}
	return exec.CommandContext(ctx, agentExe, args...)
}

func newInternalToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Scheduler) agentInternalToken(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if proc, ok := s.agents[name]; ok {
		return proc.internalToken
	}
	return ""
}

func (s *Scheduler) monitorAgent(proc *agentProcess) {
	err := proc.cmd.Wait()
	if err != nil {
		log.Printf("Agent %s exited: %v", proc.cfg.Name, err)
	} else {
		log.Printf("Agent %s exited", proc.cfg.Name)
	}

	s.mu.Lock()
	current, ok := s.agents[proc.cfg.Name]
	if !ok || current != proc {
		s.mu.Unlock()
		return
	}
	delete(s.agents, proc.cfg.Name)

	if proc.stopping || s.ctx == nil || s.ctx.Err() != nil || !s.supervisor.Enabled {
		s.mu.Unlock()
		return
	}
	cfg, desired := s.desired[proc.cfg.Name]
	if !desired {
		s.mu.Unlock()
		return
	}
	attempt := proc.restartAttempts + 1
	delay := s.restartBackoff(attempt)
	log.Printf("Agent %s scheduling restart attempt=%d delay=%s", proc.cfg.Name, attempt, delay)
	s.scheduleRestartLocked(cfg, attempt, delay)
	s.mu.Unlock()
}

func (s *Scheduler) watchAgentHealth(ctx context.Context, proc *agentProcess) {
	startupGrace := s.supervisor.HealthStartupGrace
	if startupGrace < 0 {
		startupGrace = 0
	}
	if startupGrace > 0 {
		timer := time.NewTimer(startupGrace)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}

	interval := s.supervisor.HealthInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	threshold := s.supervisor.HealthFailureThreshold
	if threshold <= 0 {
		threshold = 3
	}

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ok, detail, took := s.checkAgentHealth(ctx, proc.cfg)
		if ok {
			if failures > 0 {
				log.Printf("Agent %s health recovered failures=%d took=%s", proc.cfg.Name, failures, took)
			}
			failures = 0
		} else {
			failures++
			log.Printf("Agent %s health failed consecutive=%d threshold=%d took=%s detail=%s", proc.cfg.Name, failures, threshold, took, detail)
			if failures >= threshold {
				s.restartUnhealthyAgent(proc, detail)
				return
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (s *Scheduler) checkAgentHealth(ctx context.Context, cfg AgentConfig) (bool, string, time.Duration) {
	timeout := s.supervisor.HealthTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.Port)
	start := time.Now()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, err.Error(), time.Since(start)
	}
	resp, err := http.DefaultClient.Do(req)
	took := time.Since(start)
	if err != nil {
		return false, err.Error(), took
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Sprintf("status=%d", resp.StatusCode), took
	}
	return true, "ok", took
}

func (s *Scheduler) restartUnhealthyAgent(proc *agentProcess, detail string) {
	s.mu.Lock()
	current, ok := s.agents[proc.cfg.Name]
	if !ok || current != proc || proc.stopping || s.ctx == nil || s.ctx.Err() != nil || !s.supervisor.Enabled {
		s.mu.Unlock()
		return
	}
	log.Printf("Agent %s health threshold reached; killing process pid=%d detail=%s", proc.cfg.Name, proc.cmd.Process.Pid, detail)
	proc.cancel()
	if proc.cmd != nil && proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}
	s.mu.Unlock()
}

func (s *Scheduler) scheduleRestartLocked(cfg AgentConfig, attempt int, delay time.Duration) {
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.ctx.Done():
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ctx == nil || s.ctx.Err() != nil {
			return
		}
		currentCfg, desired := s.desired[cfg.Name]
		if !desired {
			return
		}
		if _, exists := s.agents[cfg.Name]; exists {
			return
		}
		if err := s.startAgentLocked(s.ctx, currentCfg, attempt); err != nil {
			nextAttempt := attempt + 1
			nextDelay := s.restartBackoff(nextAttempt)
			log.Printf("Agent %s restart failed attempt=%d next_delay=%s err=%v", cfg.Name, attempt, nextDelay, err)
			s.scheduleRestartLocked(currentCfg, nextAttempt, nextDelay)
			return
		}
		log.Printf("Agent %s restarted attempt=%d", cfg.Name, attempt)
		go s.recoverAgentInvocations(s.ctx, cfg.Name)
	}()
}

func (s *Scheduler) recoverAgentInvocations(ctx context.Context, agentName string) {
	records := s.ledger.RecoverableForAgent(agentName)
	if len(records) == 0 {
		log.Printf("Agent %s recovery: no recoverable invocations", agentName)
		return
	}
	dispatch := s.recoveryDispatch
	if dispatch == nil {
		dispatch = func(ctx context.Context, agentName, contextID, prompt string) (string, error) {
			return s.ChatWithAgent(agentName, contextID, prompt)
		}
	}
	for _, rec := range records {
		if rec.ContextID == "" {
			log.Printf("Agent %s recovery: skip invocation=%s reason=empty_context", agentName, rec.ID)
			continue
		}
		log.Printf("Agent %s recovery: dispatch invocation=%s contextID=%s status=%s", agentName, rec.ID, rec.ContextID, rec.Status)
		s.ledger.Recovering(rec.ID)
		if _, err := dispatch(ctx, agentName, rec.ContextID, continuationPrompt); err != nil {
			log.Printf("Agent %s recovery: failed invocation=%s contextID=%s err=%v", agentName, rec.ID, rec.ContextID, err)
			s.ledger.RecoveryFailed(rec.ID, err)
			continue
		}
		log.Printf("Agent %s recovery: recovered invocation=%s contextID=%s", agentName, rec.ID, rec.ContextID)
		s.ledger.Recovered(rec.ID)
	}
}

func (s *Scheduler) restartBackoff(attempt int) time.Duration {
	initial := s.supervisor.InitialBackoff
	if initial <= 0 {
		initial = time.Second
	}
	max := s.supervisor.MaxBackoff
	if max <= 0 {
		max = 30 * time.Second
	}
	delay := initial
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	if delay > max {
		return max
	}
	return delay
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
		proc.stopping = true
		proc.cancel()
		if proc.cmd != nil && proc.cmd.Process != nil {
			proc.cmd.Process.Kill()
		}
		log.Printf("Agent %s stopped", name)
	}
	s.agents = make(map[string]*agentProcess)
	s.desired = make(map[string]AgentConfig)
	s.running = false
}

// ListAgents returns all registered agents.
func (s *Scheduler) ListAgents() []*a2a.AgentCard {
	return s.registry.List()
}

// ChatWithAgent sends a message to an agent.
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

	chatTimeout := s.cfg.Timeouts.ChatTimeout()
	ctx := context.Background()
	var cancel context.CancelFunc
	if chatTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, chatTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	client, err := wrapper.NewAgentClientWithInternalToken(ctx, card, s.agentInternalToken(agentName))
	if err != nil {
		return "", fmt.Errorf("create client for %s: %w", agentName, err)
	}

	// contextID is propagated when the caller wants session reuse across
	// multiple chats (e.g. CLI users with --context). Empty string means each
	// call is isolated —
	// the agent's executor will auto-generate a fresh contextID for the task.
	return client.SendMessage(ctx, contextID, message)
}

// GetTaskStatus gets a task's status.
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

	client, err := wrapper.NewAgentClientWithInternalToken(ctx, card, s.agentInternalToken(agentName))
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", agentName, err)
	}

	return client.GetTask(ctx, taskID)
}
