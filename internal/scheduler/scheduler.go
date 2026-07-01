package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	ahprocess "github.com/wu8685/ahsir/internal/process"
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

	// adminToken gates the control plane (/admin/agents + registry write).
	// Empty means auth is disabled — the degenerate case for bare-constructed
	// test schedulers (no config path, no AHSIR_ADMIN_TOKEN). Set by
	// loadAdminToken, which Start calls.
	adminTok       string
	adminTokSource string

	recoveryDispatch recoveryDispatcher

	findLocalListener    localListenerFinder
	killLocalProcessTree localProcessKiller

	// rooms hosts multi-agent group chats (roundtable mode). Lazily wired in
	// New so the turnFunc can close over the constructed Scheduler.
	rooms *RoomManager
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
type localListenerFinder func(port int) (localProcessInfo, bool, error)
type localProcessKiller func(pid int) error

type localProcessInfo struct {
	PID     int
	Command string
	Cwd     string
}

const continuationPrompt = "You were restarted while working on a previous task in this session. Inspect the existing conversation context and continue the interrupted work from where it left off. If the prior task was already complete, briefly report that no further action is needed."

// errPortInUse marks "a foreign process is listening on this port" so the
// auto-allocation loop in startAgentLocked can skip to the next candidate
// while pinned-port configs still surface it as a hard error.
var errPortInUse = errors.New("port in use")

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
	s := &Scheduler{
		cfg:                  cfg,
		registry:             registry.NewRegistry(heartbeatTimeout),
		agents:               make(map[string]*agentProcess),
		desired:              make(map[string]AgentConfig),
		ledger:               ledger,
		supervisor:           defaultSupervisorConfig(),
		agentCommand:         defaultAgentCommand,
		findLocalListener:    defaultFindLocalListener,
		killLocalProcessTree: defaultKillLocalProcessTree,
	}
	// Roundtable turns reuse the normal per-agent chat path (shared contextId,
	// speaker attribution, ledger) — the room id is the contextId.
	s.rooms = NewRoomManager(
		func(agent, contextID, speaker, message string) (string, error) {
			// Record each room turn in the ledger so `trace` and the console
			// 轨迹 panel see roundtable activity — the gateway chat handler
			// (which normally writes the ledger) isn't in this path.
			inv := s.ledger.Begin(InvocationMetadata{
				Source:    InvocationSourceRoundtable,
				AgentName: agent,
				Method:    "message/send",
				ContextID: contextID,
				UserText:  message,
				Speaker:   speaker,
			})
			reply, err := s.ChatWithAgentAs(agent, contextID, speaker, message)
			if err != nil {
				s.ledger.Fail(inv.ID, err)
				return "", err
			}
			s.ledger.Complete(inv.ID)
			return reply, nil
		},
		func(name string) bool { _, ok := s.registry.Get(name); return ok },
	)

	// Roundtable persistence: prune rooms inactive past the retention window,
	// then restore the rest so a scheduler restart no longer loses group-chat
	// history (mirrors the invocation ledger + per-agent transcript stores).
	if dir := cfg.RoomsDir(); dir != "" {
		store := NewRoomStore(dir)
		if n, err := store.CompactForRetention(time.Now()); err != nil {
			log.Printf("Roundtable retention compaction failed dir=%s err=%v", dir, err)
		} else if n > 0 {
			log.Printf("Roundtable retention: pruned %d inactive room(s) (>30d)", n)
		}
		if restored, err := store.Load(); err != nil {
			log.Printf("Roundtable persistence disabled dir=%s err=%v", dir, err)
		} else {
			s.rooms.SetStore(store, restored)
			if len(restored) > 0 {
				log.Printf("Roundtable: restored %d room(s) from %s", len(restored), dir)
			}
		}
	}

	return s
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

	// Resolve the control-plane admin token before binding so enforcement is
	// live the moment the gateway accepts connections.
	if err := s.loadAdminToken(); err != nil {
		s.abortStartLocked()
		return fmt.Errorf("load admin token: %w", err)
	}
	if s.adminTok == "" {
		log.Printf("Scheduler auth: DISABLED (%s) — control plane is unauthenticated", s.adminTokSource)
	} else {
		log.Printf("Scheduler auth: enabled (token source: %s)", s.adminTokSource)
	}

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

	// Bind synchronously so a registry port conflict surfaces as Start's
	// return value — a scheduler that never listened must not report success.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.abortStartLocked()
		return fmt.Errorf("scheduler listen on %s: %w", addr, err)
	}
	log.Printf("Registry listening on %s", addr)
	// Capture the server locally: the goroutine must not read s.httpSrv (the
	// field is nil'd by abortStartLocked / Stop while the goroutine runs).
	srv := s.httpSrv
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("Registry server: %v", err)
		}
	}()

	// Start each local agent. A failure mid-loop tears down everything
	// started so far (HTTP server included) — Start either yields a fully
	// running scheduler or nothing.
	for _, agentCfg := range s.cfg.Agents {
		if agentCfg.Remote != "" {
			continue
		}
		if err := s.startAgentLocked(ctx, agentCfg, 0); err != nil {
			s.abortStartLocked()
			return fmt.Errorf("start agent %s: %w", agentCfg.Name, err)
		}
	}

	s.running = true
	// Retry any roundtable turn interrupted by the previous crash/restart. Runs
	// in the background per room, waiting for the target agent to re-register
	// before delivering the compensating turn (see ResumePending).
	s.rooms.ResumePending()
	return nil
}

// abortStartLocked unwinds a partially-completed Start: kills any agents
// already spawned, shuts the HTTP server down (if it got as far as
// listening), and cancels the scheduler context. Caller must hold s.mu.
func (s *Scheduler) abortStartLocked() {
	for name, proc := range s.agents {
		proc.stopping = true
		killAgentProcess(proc)
		proc.cancel()
		delete(s.agents, name)
		delete(s.desired, name)
	}
	if s.httpSrv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.httpSrv.Shutdown(shutCtx)
		cancel()
		s.httpSrv = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
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
	// Kill the process group before canceling the CommandContext. If
	// CommandContext kills only the direct child first, we may lose the parent
	// PID needed to address the process group.
	killAgentProcess(proc)
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
	if cfg.Port == 0 {
		// Auto-allocation probes each candidate: a port held by a foreign
		// process (not a stale ahsir-agent we can evict) is skipped rather
		// than failing the whole agent start — the allocator's job is to
		// find a *usable* port, not just the next integer.
		for {
			port, err := s.cfg.AllocatePort()
			if err != nil {
				return err
			}
			// Persist the resolved port into cfg so s.agents[name].cfg.Port
			// reflects the actually-allocated value (callers — admin API,
			// tests — read this).
			cfg.Port = port
			if err := s.evictStaleLocalAgent(cfg); err != nil {
				if errors.Is(err, errPortInUse) {
					log.Printf("Agent %s: skipping port %d (%v)", cfg.Name, port, err)
					continue
				}
				return err
			}
			break
		}
	} else {
		// Explicitly pinned port: in-use by a foreign process is a hard
		// error the user must resolve.
		if err := s.evictStaleLocalAgent(cfg); err != nil {
			return err
		}
	}
	if cfg.InternalToken == "" {
		token, err := newInternalToken()
		if err != nil {
			return fmt.Errorf("create internal token for %s: %w", cfg.Name, err)
		}
		cfg.InternalToken = token
	}
	// Hand the agent the control-plane admin token so its registry heartbeat
	// (POST /agents every ~10s) authenticates against the gated write path.
	// Empty when auth is disabled — the agent then sends no header, which the
	// open registry accepts.
	cfg.AdminToken = s.adminTok

	agentCtx, cancel := context.WithCancel(ctx)

	// Find the ahsir-agent binary
	agentExe := s.agentBinary()
	registryURL := fmt.Sprintf("http://%s:%d", s.cfg.Registry.Host, s.cfg.Registry.Port)

	buildCommand := s.agentCommand
	if buildCommand == nil {
		buildCommand = defaultAgentCommand
	}
	cmd := buildCommand(agentCtx, agentExe, cfg, registryURL)
	ahprocess.PrepareCommand(cmd)
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

	log.Printf("Agent %s started on port %d (pid: %d)", cfg.Name, cfg.Port, cmd.Process.Pid)

	// Monitor process exit
	go s.monitorAgent(proc)
	if s.supervisor.HealthCheckEnabled {
		go s.watchAgentHealth(agentCtx, proc)
	}

	return nil
}

func (s *Scheduler) evictStaleLocalAgent(cfg AgentConfig) error {
	if cfg.Port <= 0 || s.findLocalListener == nil {
		return nil
	}
	info, ok, err := s.findLocalListener(cfg.Port)
	if err != nil {
		return fmt.Errorf("inspect listener on port %d: %w", cfg.Port, err)
	}
	if !ok {
		return nil
	}
	if !isStaleLocalAgent(info, cfg) {
		return fmt.Errorf("port %d already in use by pid=%d command=%q: %w", cfg.Port, info.PID, info.Command, errPortInUse)
	}
	log.Printf("Evicting stale local agent name=%s port=%d pid=%d command=%q", cfg.Name, cfg.Port, info.PID, info.Command)
	kill := s.killLocalProcessTree
	if kill == nil {
		kill = defaultKillLocalProcessTree
	}
	if err := kill(info.PID); err != nil {
		return fmt.Errorf("evict stale local agent pid=%d port=%d: %w", info.PID, cfg.Port, err)
	}
	time.Sleep(100 * time.Millisecond)
	return nil
}

func isStaleLocalAgent(info localProcessInfo, cfg AgentConfig) bool {
	command := strings.ToLower(info.Command)
	if !strings.Contains(command, "ahsir-agent") {
		return false
	}
	workspace := strings.TrimSpace(cfg.Workspace)
	if workspace == "" {
		return true
	}
	cleanWorkspace := filepath.Clean(workspace)
	absWorkspace, err := filepath.Abs(cleanWorkspace)
	if err == nil {
		cleanWorkspace = absWorkspace
	}
	return strings.Contains(info.Command, cleanWorkspace) || sameCleanPath(info.Cwd, cleanWorkspace)
}

func sameCleanPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, err := filepath.Abs(filepath.Clean(a))
	if err == nil {
		a = aa
	}
	bb, err := filepath.Abs(filepath.Clean(b))
	if err == nil {
		b = bb
	}
	return a == b
}

func defaultFindLocalListener(port int) (localProcessInfo, bool, error) {
	out, err := exec.Command("lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN", "-Fp").Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return localProcessInfo{}, false, nil
		}
		return localProcessInfo{}, false, err
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "p") || len(line) == 1 {
			continue
		}
		pid, convErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "p")))
		if convErr != nil {
			continue
		}
		info := localProcessInfo{PID: pid}
		if cmdOut, cmdErr := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output(); cmdErr == nil {
			info.Command = strings.TrimSpace(string(cmdOut))
		}
		if cwd, cwdErr := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd")); cwdErr == nil {
			info.Cwd = cwd
		}
		return info, true, nil
	}
	return localProcessInfo{}, false, nil
}

func defaultKillLocalProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return ahprocess.KillTree(proc)
}

func defaultAgentCommand(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
	args := []string{
		"--workspace", cfg.Workspace,
		"--port", strconv.Itoa(cfg.Port),
		"--registry", registryURL,
	}
	if cfg.Workdir != "" {
		args = append(args, "--workdir", cfg.Workdir)
	}
	if cfg.InternalToken != "" {
		args = append(args, "--internal-token", cfg.InternalToken)
	}
	if cfg.AdminToken != "" {
		args = append(args, "--admin-token", cfg.AdminToken)
	}
	cmd := exec.CommandContext(ctx, agentExe, args...)
	ahprocess.PrepareCommand(cmd)
	return cmd
}

// localURL returns the loopback address the scheduler bound this agent to.
// This is the scheduler's OWN record of where the process lives — unlike the
// registry card's URL, it cannot be overwritten by an unauthenticated
// registration, so it is the only safe destination for the internal token.
func (p *agentProcess) localURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/", p.cfg.Port)
}

// agentDialTarget resolves how to reach an agent for a token-bearing request.
// For a scheduler-managed agent it returns a card pointing at the recorded
// local address plus that agent's internal token. For an agent known only via
// a registry card (e.g. a future remote agent the scheduler did not spawn) it
// returns the card as-is with an EMPTY token — the internal token is never
// sent to an address the scheduler didn't choose. ok is false when the agent
// is unknown.
func (s *Scheduler) agentDialTarget(name string) (card *a2a.AgentCard, internalToken string, ok bool) {
	regCard, found := s.registry.Get(name)
	s.mu.Lock()
	proc, managed := s.agents[name]
	s.mu.Unlock()

	if managed {
		// Clone the registry card (for Name/transport metadata) but force the
		// URL to the scheduler-recorded local address.
		c := &a2a.AgentCard{
			Name:               name,
			Version:            "1.0.0",
			URL:                proc.localURL(),
			PreferredTransport: a2a.TransportProtocolJSONRPC,
		}
		if regCard != nil {
			cp := *regCard
			cp.URL = proc.localURL()
			c = &cp
		}
		return c, proc.internalToken, true
	}
	if found {
		return regCard, "", true
	}
	return nil, "", false
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
	killAgentProcess(proc)
	proc.cancel()
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
		killAgentProcess(proc)
		proc.cancel()
		log.Printf("Agent %s stopped", name)
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.agents = make(map[string]*agentProcess)
	s.desired = make(map[string]AgentConfig)
	s.running = false
}

func killAgentProcess(proc *agentProcess) {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return
	}
	if err := ahprocess.KillTree(proc.cmd.Process); err != nil {
		log.Printf("Agent %s process-tree kill failed pid=%d err=%v", proc.cfg.Name, proc.cmd.Process.Pid, err)
	}
}

// ListAgents returns all registered agents.
func (s *Scheduler) ListAgents() []*a2a.AgentCard {
	return s.registry.List()
}

// AgentConfigFile returns the path to an agent's agent-card.yaml and its raw
// bytes. Read-only — the agent must be in the desired set. The path is returned
// even on read error so callers can surface where the file should live.
func (s *Scheduler) AgentConfigFile(name string) (string, []byte, error) {
	s.mu.Lock()
	cfg, ok := s.desired[name]
	s.mu.Unlock()
	if !ok {
		return "", nil, fmt.Errorf("agent %q not found", name)
	}
	path := filepath.Join(cfg.Workspace, ".a2a", "agent-card.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return path, nil, fmt.Errorf("read agent config: %w", err)
	}
	return path, data, nil
}

// RestartAgent stops then restarts an agent so it re-reads its agent-card.yaml —
// the only way an edited config takes effect (the agent process reads the card
// at startup; the scheduler never caches its contents). The workspace is grabbed
// before StopAgent, which removes the agent from the desired set; then we wait
// for monitorAgent to clear s.agents[name] before starting, so StartAgent
// doesn't see it as already running. Returns the newly allocated port.
func (s *Scheduler) RestartAgent(name string) (int, error) {
	s.mu.Lock()
	cfg, ok := s.desired[name]
	s.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("agent %q not found", name)
	}
	// Re-allocate the port: the old one may linger in TIME_WAIT, and re-reading
	// the card — not port stability — is the point of a restart.
	cfg.Port = 0
	if err := s.StopAgent(name); err != nil {
		return 0, fmt.Errorf("stop %q: %w", name, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		_, stillUp := s.agents[name]
		s.mu.Unlock()
		if !stillUp {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("timeout waiting for %q to stop before restart", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s.StartAgent(cfg)
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
	return s.ChatWithAgentAs(agentName, contextID, "", message)
}

// ChatWithAgentAs is ChatWithAgent plus speaker attribution: a non-empty
// speaker rides the outbound A2A message metadata so the agent can tag the
// turn with who said it (shared-context collaboration). Empty speaker is
// byte-identical to ChatWithAgent.
func (s *Scheduler) ChatWithAgentAs(agentName, contextID, speaker, message string) (string, error) {
	card, internalToken, ok := s.agentDialTarget(agentName)
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

	client, err := wrapper.NewAgentClientWithInternalToken(ctx, card, internalToken)
	if err != nil {
		return "", fmt.Errorf("create client for %s: %w", agentName, err)
	}

	// contextID is propagated when the caller wants session reuse across
	// multiple chats (e.g. CLI users with --context). Empty string means each
	// call is isolated —
	// the agent's executor will auto-generate a fresh contextID for the task.
	return client.SendMessageWithSpeaker(ctx, contextID, speaker, message)
}

// ChatWithAgentAsync submits a turn without waiting for it: the agent
// answers with a submitted task immediately (configuration.blocking=false).
// Callers poll the task via GetTaskStatus / `ahsir status`.
func (s *Scheduler) ChatWithAgentAsync(agentName, contextID, speaker, message string) (*a2a.Task, error) {
	card, internalToken, ok := s.agentDialTarget(agentName)
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}

	// Submission is a fast accept (no LLM round-trip) — task-status timeout
	// is the right budget, not the chat timeout.
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.TaskStatusTimeout())
	defer cancel()
	client, err := wrapper.NewAgentClientWithInternalToken(ctx, card, internalToken)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", agentName, err)
	}
	return client.SendMessageNonBlocking(ctx, contextID, speaker, message)
}

// settleAsyncInvocation polls the agent's task until it reaches a terminal
// state, then settles the ledger record — keeping `ahsir trace` truthful for
// async turns. Budget: the chat timeout (0 = unbounded, mirroring the
// synchronous path's "no scheduler deadline").
func (s *Scheduler) settleAsyncInvocation(invID, agentName string, taskID string) {
	var deadline time.Time
	if t := s.cfg.Timeouts.ChatTimeout(); t > 0 {
		deadline = time.Now().Add(t)
	}
	consecutiveErrs := 0
	for {
		time.Sleep(time.Second)
		if !deadline.IsZero() && time.Now().After(deadline) {
			s.ledger.FailMessage(invID, fmt.Sprintf("async task %s: no terminal state within chat timeout", taskID))
			return
		}
		task, err := s.GetTaskStatus(agentName, taskID)
		if err != nil {
			// Tolerate transient poll failures; a restarted agent (task store
			// is memory-only) makes the task unknowable — settle as failed
			// with the documented degradation pointer.
			consecutiveErrs++
			if consecutiveErrs >= 5 {
				s.ledger.FailMessage(invID, fmt.Sprintf("async task %s: poll failed: %v (agent restarted? check `ahsir history`)", taskID, err))
				return
			}
			continue
		}
		consecutiveErrs = 0
		switch task.Status.State {
		case a2a.TaskStateCompleted:
			s.ledger.Complete(invID)
			return
		case a2a.TaskStateFailed:
			msg := "async task failed"
			if task.Status.Message != nil {
				if txt := wrapper.MessageText(task.Status.Message); txt != "" {
					msg = txt
				}
			}
			s.ledger.FailMessage(invID, msg)
			return
		case a2a.TaskStateCanceled:
			s.ledger.FailMessage(invID, "task canceled")
			return
		}
	}
}

// AgentHistory fetches the per-context transcript from the agent's /history
// endpoint, attaching the scheduler-owned internal token — the transcript
// carries full conversation content, so the wrapper only serves it to the
// scheduler, and ACL checks attach here when ingress auth lands.
//
// Uses the TaskStatus timeout: this is a file read on the agent side, no LLM
// round-trip.
func (s *Scheduler) AgentHistory(agentName, contextID string) ([]wrapper.TranscriptTurn, error) {
	card, internalToken, ok := s.agentDialTarget(agentName)
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}

	endpoint := strings.TrimSuffix(card.URL, "/") + "/history?contextId=" + url.QueryEscape(contextID)
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.TaskStatusTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build history request for %s: %w", agentName, err)
	}
	if internalToken != "" {
		req.Header.Set(wrapper.InternalTokenHeader, internalToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch history from %s: %w", agentName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent %s history: status %d", agentName, resp.StatusCode)
	}
	var turns []wrapper.TranscriptTurn
	if err := json.NewDecoder(resp.Body).Decode(&turns); err != nil {
		return nil, fmt.Errorf("decode history from %s: %w", agentName, err)
	}
	return turns, nil
}

// GetTaskStatus gets a task's status.
//
// Uses cfg.Timeouts.TaskStatus (default 30s) — this is a quick task-store
// read with no LLM round-trip, so it can be tight.
func (s *Scheduler) GetTaskStatus(agentName, taskID string) (*a2a.Task, error) {
	card, internalToken, ok := s.agentDialTarget(agentName)
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.TaskStatusTimeout())
	defer cancel()

	client, err := wrapper.NewAgentClientWithInternalToken(ctx, card, internalToken)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", agentName, err)
	}

	return client.GetTask(ctx, taskID)
}
