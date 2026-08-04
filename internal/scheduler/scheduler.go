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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wu8685/ahsir/internal/obs"
	ahprocess "github.com/wu8685/ahsir/internal/process"
	"github.com/wu8685/ahsir/internal/registry"
	"github.com/wu8685/ahsir/internal/wrapper"
)

// Scheduler manages the lifecycle of all agents.
type Scheduler struct {
	cfg        *Config
	registry   *registry.Registry
	agents     map[string]*agentProcess
	desired    map[string]AgentConfig
	ledger     *InvocationLedger
	liveEvents *liveEventHub
	httpSrv    *http.Server
	// obsReg is this process's single, explicitly-injected metric registerer
	// (§4.3). Never prometheus.DefaultRegisterer. Served read-only at /metrics.
	obsReg         *obs.Registry
	gatewayMetrics *GatewayMetrics
	mu             sync.Mutex
	running        bool
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

	// idleStopped holds agents that scaled themselves to zero after going idle
	// (issue #6). They remain in `desired` (unlike an explicit StopAgent) so the
	// activator can wake them on next access — the config is stashed here to
	// respawn from. Guarded by s.mu.
	idleStopped map[string]AgentConfig

	// activations single-flights the complete runtime activation, including
	// readiness, for base agents and pooled instance children alike.
	// It has a separate mutex so callers can join an activation while its leader
	// holds s.mu to allocate and publish the process.
	activationMu sync.Mutex
	activations  map[string]*activationCall

	// lifecycles retains scheduler-owned operational state even when an agent
	// has no live registry card (idle, stopped, invalid, or restarting).
	// Guarded by s.mu.
	lifecycles map[string]AgentLifecycleSnapshot

	// pools holds the per-card instance pool for every agent whose card backs
	// more than one concurrent runtime instance (issue #18). Lazily created in
	// poolFor from the agent's desired InstanceCap; absent for single-instance
	// agents, which keep the unchanged one-card-one-worker dispatch path. Guarded
	// by s.mu.
	pools map[string]*instancePool
}

type agentProcess struct {
	cfg             AgentConfig
	cmd             *exec.Cmd
	cancel          context.CancelFunc
	stopping        bool
	restartAttempts int
	internalToken   string
	done            chan struct{} // closed by monitorAgent after cmd.Wait and state cleanup
	failureState    AgentLifecycleState
	failureReason   string
}

type activationCall struct {
	done chan struct{}
	err  error
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

// ErrAgentIncompatible means an idempotent registration found scheduler
// desired state under the same name, but its immutable configuration differs
// from the request. Callers should surface this as HTTP 409 without modifying
// the persisted card or restarting the existing runtime.
var ErrAgentIncompatible = errors.New("agent registration incompatible")

// ErrAgentAlreadyExists preserves the legacy pre-staged-card POST behavior:
// without an inline card there is no requested immutable definition to verify,
// so an existing name remains a conflict rather than an idempotent success.
var ErrAgentAlreadyExists = errors.New("agent already exists")

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
	// Single per-process metric registerer, injected into every collector
	// (§4.3). The Gateway A-group derives entirely from the ledger's begin/
	// finish sink, so we wire it straight onto the ledger here.
	obsReg := obs.NewRegistry()
	gatewayMetrics := NewGatewayMetrics(obsReg)
	ledger.SetGatewayMetrics(gatewayMetrics)
	s := &Scheduler{
		cfg:                  cfg,
		registry:             registry.NewRegistry(heartbeatTimeout),
		agents:               make(map[string]*agentProcess),
		desired:              make(map[string]AgentConfig),
		ledger:               ledger,
		liveEvents:           newLiveEventHub(),
		obsReg:               obsReg,
		gatewayMetrics:       gatewayMetrics,
		supervisor:           defaultSupervisorConfig(),
		agentCommand:         defaultAgentCommand,
		findLocalListener:    defaultFindLocalListener,
		killLocalProcessTree: defaultKillLocalProcessTree,
		idleStopped:          make(map[string]AgentConfig),
		activations:          make(map[string]*activationCall),
		lifecycles:           make(map[string]AgentLifecycleSnapshot),
		pools:                make(map[string]*instancePool),
	}
	for _, agentCfg := range cfg.Agents {
		s.setLifecycleLocked(agentCfg.Name, AgentLifecycleStopped, "configured-not-started", "configured but not started", 0, time.Time{})
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

// MetricsGatherer returns the scheduler's Prometheus gatherer, backing the
// read-only /metrics endpoint. Explicitly injected — never the global default.
func (s *Scheduler) MetricsGatherer() prometheus.Gatherer {
	return s.obsReg.Gatherer()
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
	// Surface the scrape endpoint so static Prometheus config never has to
	// guess the port (§3 lock-in item 2). Agent /metrics ports are logged as
	// each agent starts.
	log.Printf("Metrics endpoint: http://%s/metrics", addr)
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
			var configErr *agentConfigurationError
			if errors.As(err, &configErr) {
				log.Printf("Agent %s not started: %v", agentCfg.Name, configErr)
				continue
			}
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

// EnsureAgent atomically reconciles one dynamically registered Agent. The
// existence/compatibility check, optional card write, and first process spawn
// all run under s.mu, so concurrent identical registrations cannot both write
// or launch. An already-desired compatible Agent is success and is left
// byte-for-byte untouched; a mismatch returns ErrAgentIncompatible before any
// filesystem mutation.
func (s *Scheduler) EnsureAgent(cfg AgentConfig, card *wrapper.AgentCardConfig) (port int, created bool, err error) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return 0, false, fmt.Errorf("scheduler not running")
	}
	if cfg.Name == "" {
		s.mu.Unlock()
		return 0, false, fmt.Errorf("agent name is required")
	}
	if cfg.Workspace == "" {
		s.mu.Unlock()
		return 0, false, fmt.Errorf("agent workspace is required")
	}
	for name, existing := range s.desired {
		if name != cfg.Name && sameCleanPath(existing.Workspace, cfg.Workspace) {
			s.mu.Unlock()
			return 0, false, fmt.Errorf("%w for %q: workspace %q is already desired by %q", ErrAgentIncompatible, cfg.Name, cfg.Workspace, name)
		}
	}

	if existing, ok := s.desired[cfg.Name]; ok {
		if card == nil {
			s.mu.Unlock()
			return 0, false, fmt.Errorf("%w: agent %q already running or desired", ErrAgentAlreadyExists, cfg.Name)
		}
		compatible, detail := compatibleRegistration(existing, cfg)
		if compatible {
			matches, matchErr := wrapper.CardMatches(existing.Workspace, card)
			switch {
			case matchErr != nil:
				compatible = false
				detail = "existing card cannot be verified: " + matchErr.Error()
			case !matches:
				compatible = false
				detail = "inline card differs"
			}
		}
		if !compatible {
			s.mu.Unlock()
			return 0, false, fmt.Errorf("%w for %q: %s", ErrAgentIncompatible, cfg.Name, detail)
		}
		if proc, running := s.agents[cfg.Name]; running {
			s.mu.Unlock()
			if err := s.waitAgentHealthy(proc); err != nil {
				return 0, false, fmt.Errorf("ensure running agent %q ready: %w", cfg.Name, err)
			}
			return proc.cfg.Port, false, nil
		}
		if _, idle := s.idleStopped[cfg.Name]; idle {
			s.mu.Unlock()
			if err := s.ensureAwake(cfg.Name); err != nil {
				return 0, false, err
			}
			s.mu.Lock()
			proc := s.agents[cfg.Name]
			s.mu.Unlock()
			if proc == nil {
				return 0, false, fmt.Errorf("ensure idle-stopped agent %q ready: runtime missing after wake", cfg.Name)
			}
			return proc.cfg.Port, false, nil
		}
		// Desired state with neither a process nor an intentional idle-stop is
		// scheduler/runtime drift. Fill it now under the same lock. A pending
		// supervisor restart will observe s.agents populated and become a no-op.
		if err := s.startAgentLocked(s.ctx, existing, 0); err != nil {
			s.mu.Unlock()
			return 0, false, fmt.Errorf("reconcile missing runtime for %q: %w", cfg.Name, err)
		}
		proc := s.agents[cfg.Name]
		s.mu.Unlock()
		if err := s.waitAgentHealthy(proc); err != nil {
			s.rollbackFailedWake(cfg.Name, proc)
			return 0, false, fmt.Errorf("reconcile missing runtime for %q readiness: %w", cfg.Name, err)
		}
		return proc.cfg.Port, true, nil
	}

	if card != nil {
		if err := wrapper.WriteCard(cfg.Workspace, card); err != nil {
			s.mu.Unlock()
			return 0, false, fmt.Errorf("scaffold inline card: %w", err)
		}
	}
	if err := s.startAgentLocked(s.ctx, cfg, 0); err != nil {
		s.mu.Unlock()
		return 0, false, err
	}
	proc := s.agents[cfg.Name]
	s.mu.Unlock()
	if err := s.waitAgentHealthy(proc); err != nil {
		s.rollbackFailedWake(cfg.Name, proc)
		return 0, false, fmt.Errorf("start agent %q readiness: %w", cfg.Name, err)
	}
	return proc.cfg.Port, true, nil
}

func compatibleRegistration(existing, requested AgentConfig) (bool, string) {
	if existing.InstanceCap() != requested.InstanceCap() {
		return false, fmt.Sprintf("instances=%d, requested=%d", existing.InstanceCap(), requested.InstanceCap())
	}
	if !sameCleanPath(existing.Workspace, requested.Workspace) {
		return false, fmt.Sprintf("workspace=%q, requested=%q", existing.Workspace, requested.Workspace)
	}
	existingWorkdir := existing.Workdir
	if existingWorkdir == "" {
		existingWorkdir = existing.Workspace
	}
	requestedWorkdir := requested.Workdir
	if requestedWorkdir == "" {
		requestedWorkdir = requested.Workspace
	}
	if !sameCleanPath(existingWorkdir, requestedWorkdir) {
		return false, fmt.Sprintf("workdir=%q, requested=%q", existingWorkdir, requestedWorkdir)
	}
	// Port zero means dynamic allocation. Once resolved, desired state stores
	// the concrete port, so a repeated dynamic request must ignore that runtime
	// detail. An explicitly pinned request remains part of compatibility.
	if requested.Port > 0 && existing.Port != requested.Port {
		return false, fmt.Sprintf("port=%d, requested=%d", existing.Port, requested.Port)
	}
	return true, ""
}

// StopAgent tears down a running agent. Idempotent on "not running" —
// returns nil if the name isn't in the map. Files in the workspace are
// preserved (this is dynamic deregistration only). To remove files,
// the caller (CLI) handles that separately.
func (s *Scheduler) StopAgent(name string) error {
	s.mu.Lock()
	proc, ok := s.agents[name]
	if !ok {
		_, wasDesired := s.desired[name]
		_, wasKnown := s.lifecycles[name]
		delete(s.desired, name)
		// An explicitly stopped agent must not be resurrected by the activator,
		// even if it had scaled to zero (spec §4.5: idle-stopped wakes; stopped
		// / archived does not).
		delete(s.idleStopped, name)
		if wasDesired || wasKnown {
			s.setLifecycleLocked(name, AgentLifecycleStopped, "operator-stopped", "stopped by operator", 0, time.Time{})
		}
		s.mu.Unlock()
		return nil
	}
	delete(s.desired, name)
	delete(s.idleStopped, name)
	s.setLifecycleLocked(name, AgentLifecycleStopped, "operator-stopped", "stopped by operator", 0, time.Time{})
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
	if err := validateAgentConfiguration(cfg); err != nil {
		var configErr *agentConfigurationError
		reasonCode := "invalid-agent-card"
		if errors.As(err, &configErr) {
			reasonCode = configErr.reasonCode
		}
		s.desired[cfg.Name] = cfg
		s.setLifecycleLocked(cfg.Name, AgentLifecycleInvalidConfig, reasonCode, err.Error(), 0, time.Time{})
		return err
	}
	if cfg.Port == 0 {
		// Auto-allocation probes each candidate: a port held by a foreign
		// process (not a stale ahsir-agent we can evict) is skipped rather
		// than failing the whole agent start — the allocator's job is to
		// find a *usable* port, not just the next integer.
		candidateCount := s.cfg.PortRange.End - s.cfg.PortRange.Start + 1
		if candidateCount <= 0 {
			return fmt.Errorf("no available ports in range %d-%d", s.cfg.PortRange.Start, s.cfg.PortRange.End)
		}
		allocated := false
		for range candidateCount {
			port, err := s.cfg.AllocatePort()
			if err != nil {
				return err
			}
			if s.portReservedLocked(port) {
				continue
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
			allocated = true
			break
		}
		if !allocated {
			return fmt.Errorf("no available ports in range %d-%d", s.cfg.PortRange.Start, s.cfg.PortRange.End)
		}
	} else {
		// Explicitly pinned port: in-use by a foreign process is a hard
		// error the user must resolve. Check scheduler ownership first: a
		// freshly-published process may not have bound its listener yet, so an
		// OS-only probe leaves a window for two pinned Agents to share a port.
		if s.portReservedByOtherLocked(cfg.Port, cfg.Name) {
			return fmt.Errorf("port %d is reserved by another scheduler agent", cfg.Port)
		}
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
		done:            make(chan struct{}),
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

// portReservedLocked reports whether a dynamic candidate is owned by
// scheduler state. The caller holds s.mu, which keeps choosing the port and
// publishing the new agentProcess atomic with every other scheduler start.
//
// Running processes retain their ports until monitorAgent observes exit.
// Desired agents also retain their ports across crash/health restart backoff,
// except for idle-stopped agents whose process has deliberately released the
// listener. Statically pinned config entries are reserved regardless of config
// order so an earlier dynamic entry cannot take a later pinned port during
// scheduler startup.
func (s *Scheduler) portReservedLocked(port int) bool {
	for _, proc := range s.agents {
		if proc.cfg.Port == port {
			return true
		}
	}
	for name, cfg := range s.desired {
		if _, idle := s.idleStopped[name]; idle {
			continue
		}
		if cfg.Port == port {
			return true
		}
	}
	for _, cfg := range s.cfg.Agents {
		if cfg.Port > 0 && cfg.Port == port {
			return true
		}
	}
	return false
}

// portReservedByOtherLocked is the explicit-port counterpart to
// portReservedLocked. The same name is excluded so a supervisor restart may
// reclaim its desired pinned port after its old process has exited.
func (s *Scheduler) portReservedByOtherLocked(port int, name string) bool {
	for procName, proc := range s.agents {
		if procName != name && proc.cfg.Port == port {
			return true
		}
	}
	for desiredName, cfg := range s.desired {
		if desiredName == name {
			continue
		}
		if _, idle := s.idleStopped[desiredName]; idle {
			continue
		}
		if cfg.Port == port {
			return true
		}
	}
	for _, cfg := range s.cfg.Agents {
		if cfg.Name != name && cfg.Port > 0 && cfg.Port == port {
			return true
		}
	}
	return false
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
	return canonicalPath(a) == canonicalPath(b)
}

// canonicalPath resolves symlinks in an existing path or in its nearest
// existing ancestor. The latter matters for managed workspaces: the Agent
// directory may not exist yet while its configured parent is a symlink.
func canonicalPath(path string) string {
	clean := filepath.Clean(path)
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved)
	}

	cur := clean
	var suffix []string
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return clean
		}
		suffix = append([]string{filepath.Base(cur)}, suffix...)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...))
		}
		cur = parent
	}
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
	if proc.done != nil {
		defer close(proc.done)
	}
	if err != nil {
		log.Printf("Agent %s exited: %v", proc.cfg.Name, err)
	} else {
		log.Printf("Agent %s exited", proc.cfg.Name)
	}
	idle := isIdleStopExit(err)

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

	// Scale-to-zero: a controlled idle self-exit is NOT a crash. Mark the agent
	// idle-stopped (kept in `desired` so the activator can wake it) and skip the
	// supervisor restart entirely — restarting would defeat the whole point of
	// reaping an idle agent (spec §4.3, R3). Cancel the process context to stop
	// its health watcher; a lingering watcher would otherwise "fail" against the
	// dead port and could try to restart it.
	if idle {
		s.idleStopped[proc.cfg.Name] = cfg
		s.setLifecycleLocked(proc.cfg.Name, AgentLifecycleIdle, "scale-to-zero", "idle timeout elapsed; wakeable on next request", 0, time.Time{})
		proc.cancel()
		s.mu.Unlock()
		log.Printf("Agent %s idle-stopped (scaled to zero); will wake on next access", proc.cfg.Name)
		return
	}
	attempt := proc.restartAttempts + 1
	delay := s.restartBackoff(attempt)
	state := AgentLifecycleRestartBackoff
	reasonCode := "process-exit"
	reason := fmt.Sprintf("process exited: %v", err)
	if proc.failureState == AgentLifecycleHealthFailed {
		state = AgentLifecycleHealthFailed
		reasonCode = "health-threshold"
		reason = proc.failureReason
	}
	s.setLifecycleLocked(proc.cfg.Name, state, reasonCode, reason, attempt, time.Now().Add(delay).UTC())
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
	proc.failureState = AgentLifecycleHealthFailed
	proc.failureReason = "health check failed: " + detail
	s.setLifecycleLocked(proc.cfg.Name, AgentLifecycleHealthFailed, "health-threshold", proc.failureReason, proc.restartAttempts, time.Time{})
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
		if _, idle := s.idleStopped[cfg.Name]; idle {
			return
		}
		if err := s.startAgentLocked(s.ctx, currentCfg, attempt); err != nil {
			var configErr *agentConfigurationError
			if errors.As(err, &configErr) {
				log.Printf("Agent %s restart stopped: invalid config: %v", cfg.Name, configErr)
				return
			}
			nextAttempt := attempt + 1
			nextDelay := s.restartBackoff(nextAttempt)
			s.setLifecycleLocked(cfg.Name, AgentLifecycleRestartBackoff, "process-exit", "restart failed: "+err.Error(), nextAttempt, time.Now().Add(nextDelay).UTC())
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
	s.idleStopped = make(map[string]AgentConfig)
	s.pools = make(map[string]*instancePool)
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

// isIdleStopExit reports whether a finished agent process exited via the
// scale-to-zero self-exit path (wrapper.IdleStopExitCode) rather than crashing.
// Only a clean exit status carrying exactly that code counts — a signal kill or
// any other non-zero code is treated as a crash.
func isIdleStopExit(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode() == wrapper.IdleStopExitCode
	}
	return false
}

// IdleStoppedAgents lists agents currently scaled to zero (idle-stopped) and
// awaitable via the activator. Sorted for stable output.
func (s *Scheduler) IdleStoppedAgents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.idleStopped))
	for name := range s.idleStopped {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ensureAwake wakes an idle-stopped agent before a request is forwarded to it
// (the activator, spec §4.4). No-op when the agent is already running or is not
// idle-stopped: an unknown, explicitly-stopped, or archived agent is left alone
// so it does NOT spring back to life on a single access (spec §4.5). Concurrent
// callers single-flight through runActivation (R2) — only the first spawns a
// process; the rest wait for the same readiness result.
func (s *Scheduler) ensureAwake(name string) error {
	return s.runActivation(name, func() error { return s.ensureAwakeOnce(name) })
}

// runActivation single-flights one complete activation, including its health
// check and any rollback. Every concurrent caller observes the leader's exact
// result; a failed call is removed only after cleanup has finished, so the next
// caller can safely retry.
func (s *Scheduler) runActivation(name string, activate func() error) error {
	s.activationMu.Lock()
	if call, ok := s.activations[name]; ok {
		s.activationMu.Unlock()
		<-call.done
		return call.err
	}
	call := &activationCall{done: make(chan struct{})}
	s.activations[name] = call
	s.activationMu.Unlock()

	err := activate()
	s.activationMu.Lock()
	call.err = err
	delete(s.activations, name)
	close(call.done)
	s.activationMu.Unlock()
	return err
}

func (s *Scheduler) ensureAwakeOnce(name string) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	if _, up := s.agents[name]; up {
		s.mu.Unlock()
		return nil
	}
	cfg, idle := s.idleStopped[name]
	if !idle {
		// Not idle-stopped: nothing to wake. The caller's normal dial decides
		// whether this is a live registry agent or a genuine not-found.
		s.mu.Unlock()
		return nil
	}
	// Re-allocate the port: the reaped process's port may linger in TIME_WAIT.
	cfg.Port = 0
	startErr := s.startAgentLocked(s.ctx, cfg, 0)
	var proc *agentProcess
	if startErr == nil {
		delete(s.idleStopped, name)
		proc = s.agents[name]
	}
	s.mu.Unlock()

	if startErr != nil {
		return fmt.Errorf("wake idle-stopped agent %s: %w", name, startErr)
	}
	log.Printf("Agent %s waking from idle-stopped", name)
	if err := s.waitAgentHealthy(proc); err != nil {
		s.rollbackFailedWake(name, proc)
		return fmt.Errorf("wake idle-stopped agent %s: %w", name, err)
	}
	log.Printf("Agent %s awake and healthy", name)
	return nil
}

// rollbackFailedWake removes a process that started but never became healthy
// and restores the desired agent to idle-stopped so the next request can try a
// fresh activation. Marking the process stopping before kill prevents its
// monitor from scheduling a crash restart in parallel with that retry.
func (s *Scheduler) rollbackFailedWake(name string, proc *agentProcess) {
	s.mu.Lock()
	current, running := s.agents[name]
	if running && current == proc {
		proc.stopping = true
		killAgentProcess(proc)
		proc.cancel()
	}
	done := proc.done
	s.mu.Unlock()

	// monitorAgent owns removal from s.agents. Waiting for it preserves the
	// dynamic-port reservation until cmd.Wait confirms the listener process has
	// actually exited; otherwise an immediate retry could race the dying process
	// and falsely probe its old health endpoint.
	if running && current == proc && done != nil {
		<-done
	}

	s.mu.Lock()
	_, replacementRunning := s.agents[name]
	if cfg, desired := s.desired[name]; desired && s.running {
		// The failed process may have exited and queued a supervisor restart
		// before this readiness caller reached rollback. If that replacement is
		// already published, leave it running; marking the name idle-stopped here
		// would create contradictory state and race the next activator.
		if !replacementRunning {
			s.idleStopped[name] = cfg
		}
	}
	s.mu.Unlock()
	if !replacementRunning {
		_ = s.registry.Unregister(name)
	}
}

// waitAgentHealthy polls the agent's /healthz until it responds OK or a wake
// budget elapses. The budget mirrors the supervisor's startup grace plus a few
// health intervals — enough for a cold start + `--resume` to rebind the port.
// The invariant timeouts.chat >= cold_start + runtime.timeout (spec §4.6) is
// what keeps this wait inside the caller's overall chat deadline.
func (s *Scheduler) waitAgentHealthy(proc *agentProcess) error {
	grace := s.supervisor.HealthStartupGrace
	if grace < 0 {
		grace = 0
	}
	interval := s.supervisor.HealthInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	budget := grace + 6*interval
	if budget < 5*time.Second {
		budget = 5 * time.Second
	}
	deadline := time.Now().Add(budget)
	for {
		if ok, _, _ := s.checkAgentHealth(s.ctx, proc.cfg); ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agent %s did not become healthy within %s of wake", proc.cfg.Name, budget)
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(interval):
		}
	}
}

// poolFor returns the instance pool for a pooled agent (InstanceCap > 1),
// creating it on first use, or nil for a single-instance agent (the common
// case) so its dispatch path is left completely unchanged. Only base card names
// are pooled — an instance child name (base#n) has no `desired` entry and falls
// through to nil.
func (s *Scheduler) poolFor(agentName string) *instancePool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pools[agentName]; ok {
		return p
	}
	cfg, ok := s.desired[agentName]
	if !ok || cfg.InstanceCap() <= 1 {
		return nil
	}
	p := newInstancePool(cfg.InstanceCap())
	s.pools[agentName] = p
	return p
}

// resolveInstance maps a chat for agentName+contextID onto the concrete runtime
// instance that should serve it (issue #18). For a single-instance agent it is a
// no-op returning the base name and a no-op release. For a pooled agent it
// acquires an instance from the pool (spreading concurrent sessions across
// isolated workspaces while keeping a contextID pinned to one instance), spawns
// or wakes that instance if it is not the always-present base, and returns the
// instance name plus a release the caller MUST invoke once the turn is done.
func (s *Scheduler) resolveInstance(agentName, contextID string) (target string, release func(), err error) {
	pool := s.poolFor(agentName)
	if pool == nil {
		return agentName, func() {}, nil
	}
	idx := pool.acquire(contextID)
	release = func() { pool.release(idx) }
	if idx == 0 {
		// Ordinal 0 is the base process, already spawned at Start — dial it as
		// today; nothing extra to bring up.
		return agentName, release, nil
	}
	if err := s.startOrWakeInstance(agentName, idx); err != nil {
		release()
		return "", nil, err
	}
	return instanceName(agentName, idx), release, nil
}

// startOrWakeInstance ensures instance idx (idx>0) of a pooled base agent is
// running and healthy: it wakes an idle-stopped instance via the activator,
// waits on an in-flight spawn by another goroutine, or scaffolds the isolated
// instance workspace and spawns a fresh ahsir-agent process. Concurrent callers
// for the same instance single-flight through runActivation, mirroring
// ensureAwake, so exactly one process is started and every waiter observes the
// same readiness or rollback result.
func (s *Scheduler) startOrWakeInstance(base string, idx int) error {
	instName := instanceName(base, idx)
	return s.runActivation(instName, func() error {
		s.mu.Lock()
		if !s.running {
			s.mu.Unlock()
			return fmt.Errorf("scheduler not running")
		}
		if _, up := s.agents[instName]; up {
			s.mu.Unlock()
			return nil
		}
		if _, idle := s.idleStopped[instName]; idle {
			// This call already owns the instance activation singleflight; invoke
			// the one-shot wake directly to avoid recursively joining itself.
			s.mu.Unlock()
			return s.ensureAwakeOnce(instName)
		}
		baseCfg, ok := s.desired[base]
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("agent %q not found", base)
		}
		instCfg := deriveInstanceConfig(baseCfg, idx)
		if err := scaffoldInstanceWorkspace(baseCfg.Workspace, instCfg.Workspace); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("scaffold instance %s: %w", instName, err)
		}
		startErr := s.startAgentLocked(s.ctx, instCfg, 0)
		var proc *agentProcess
		if startErr == nil {
			proc = s.agents[instName]
		}
		s.mu.Unlock()

		if startErr != nil {
			return fmt.Errorf("start instance %s: %w", instName, startErr)
		}
		log.Printf("Agent %s instance #%d started on port %d (workspace=%s)", base, idx, proc.cfg.Port, instCfg.Workspace)
		if err := s.waitAgentHealthy(proc); err != nil {
			s.rollbackFailedWake(instName, proc)
			return fmt.Errorf("start instance %s: %w", instName, err)
		}
		return nil
	})
}

// deriveInstanceConfig produces the AgentConfig for instance idx from its base
// card. The instance gets a suffixed name, an isolated inst-<idx> workspace, a
// freshly allocated port, and a fresh internal token. Workdir is deliberately
// left as the base set it: when the base left Workdir empty (cwd defaults to the
// workspace) the instance's cwd becomes its own inst-<idx> dir — the working-tree
// isolation issue #18 is about. An explicitly shared Workdir is the operator's
// deliberate choice and is preserved.
func deriveInstanceConfig(base AgentConfig, idx int) AgentConfig {
	inst := base
	inst.Name = instanceName(base.Name, idx)
	inst.Workspace = instanceWorkspace(base.Workspace, idx)
	inst.Port = 0
	inst.InternalToken = ""
	return inst
}

// scaffoldInstanceWorkspace makes an isolated instance workspace bootable by
// copying the base card into it. The agent subprocess reads its persona from
// <workspace>/.a2a/agent-card.yaml at startup, so a fresh inst-<n> dir needs the
// same card as its base. sessions.json / transcripts are deliberately NOT copied
// — each instance keeps its own conversation state (that isolation is the point).
// Idempotent: an already-scaffolded instance card is left untouched so an
// operator edit to it survives a respawn.
func scaffoldInstanceWorkspace(baseWorkspace, instWorkspace string) error {
	if baseWorkspace == "" || instWorkspace == "" || sameCleanPath(baseWorkspace, instWorkspace) {
		return nil
	}
	dst := filepath.Join(instWorkspace, ".a2a", "agent-card.yaml")
	if _, err := os.Stat(dst); err == nil {
		return nil // already scaffolded
	}
	src := filepath.Join(baseWorkspace, ".a2a", "agent-card.yaml")
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read base card %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create instance .a2a dir: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("write instance card %s: %w", dst, err)
	}
	return nil
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
	// Instance pool: route this turn onto a concrete runtime instance (issue
	// #18). No-op for single-instance agents (target == agentName). Release marks
	// the turn done so the pool can spread the next concurrent session.
	target, release, err := s.resolveInstance(agentName, contextID)
	if err != nil {
		return "", err
	}
	defer release()

	// Activator: transparently wake an idle-stopped agent before dialing so the
	// caller never sees a scale-to-zero'd agent as "connection refused".
	if err := s.ensureAwake(target); err != nil {
		return "", err
	}
	card, internalToken, ok := s.agentDialTarget(target)
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
//
// Returns a release func the caller MUST invoke once the async turn has settled
// (its background poller reaches a terminal state) so the instance pool keeps
// counting the turn as in-flight until then — an async coder turn runs long, and
// releasing at submission time would let a second concurrent session pile onto
// the same instance. release is never nil.
func (s *Scheduler) ChatWithAgentAsync(agentName, contextID, speaker, message string) (task *a2a.Task, release func(), err error) {
	target, release, err := s.resolveInstance(agentName, contextID)
	if err != nil {
		return nil, func() {}, err
	}
	// From here on, any early return must release — the caller only owns release
	// once we hand back a live task.
	if err := s.ensureAwake(target); err != nil {
		release()
		return nil, func() {}, err
	}
	card, internalToken, ok := s.agentDialTarget(target)
	if !ok {
		release()
		return nil, func() {}, fmt.Errorf("agent %s not found", agentName)
	}

	// Submission is a fast accept (no LLM round-trip) — task-status timeout
	// is the right budget, not the chat timeout.
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeouts.TaskStatusTimeout())
	defer cancel()
	client, err := wrapper.NewAgentClientWithInternalToken(ctx, card, internalToken)
	if err != nil {
		release()
		return nil, func() {}, fmt.Errorf("create client for %s: %w", agentName, err)
	}
	t, err := client.SendMessageNonBlocking(ctx, contextID, speaker, message)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return t, release, nil
}

// settleAsyncInvocation polls the agent's task until it reaches a terminal
// state, then settles the ledger record — keeping `ahsir trace` truthful for
// async turns. Budget: the chat timeout (0 = unbounded, mirroring the
// synchronous path's "no scheduler deadline").
func (s *Scheduler) settleAsyncInvocation(invID, agentName string, taskID string, done func()) {
	if done != nil {
		defer done()
	}
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
	s.mu.Lock()
	_, idleStopped := s.idleStopped[agentName]
	_, managed := s.desired[agentName]
	s.mu.Unlock()
	if idleStopped {
		return s.managedAgentHistory(agentName, contextID)
	}

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
		// A managed runtime may have exited between registry lookup and this
		// read (most commonly scale-to-zero). The transcript is already on disk;
		// prefer it over waking an LLM process just to serve history.
		if managed {
			if turns, diskErr := s.managedAgentHistory(agentName, contextID); diskErr == nil {
				return turns, nil
			}
		}
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
