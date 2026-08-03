package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/registry"
	"github.com/wu8685/ahsir/internal/wrapper"
)

// idleAgentCommand builds a spawnable fake agent for scale-to-zero tests. The
// first instance serves /healthz, then after idleAfterMs creates a marker file
// and exits with exitCode. A later instance that finds the marker (i.e. one the
// activator woke) serves /healthz forever, so wake/health assertions are stable.
func idleAgentCommand(logPath, markerPath string, idleAfterMs, exitCode int) agentCommandBuilder {
	return func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0],
			"-test.run=TestSchedulerIdleAgentHelperProcess",
			"--",
			"--workspace", cfg.Workspace,
			"--port", fmt.Sprint(cfg.Port),
			"--registry", registryURL,
		)
		cmd.Env = append(os.Environ(),
			"AHSIR_TEST_IDLE_AGENT=1",
			"AHSIR_TEST_IDLE_LOG="+logPath,
			"AHSIR_TEST_IDLE_MARKER="+markerPath,
			"AHSIR_TEST_IDLE_AFTER_MS="+strconv.Itoa(idleAfterMs),
			"AHSIR_TEST_IDLE_EXIT_CODE="+strconv.Itoa(exitCode),
		)
		return cmd
	}
}

func TestSchedulerIdleAgentHelperProcess(t *testing.T) {
	if os.Getenv("AHSIR_TEST_IDLE_AGENT") != "1" {
		return
	}
	agentArgs := argsAfterDashDash(os.Args)
	if logPath := os.Getenv("AHSIR_TEST_IDLE_LOG"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintln(f, strings.Join(agentArgs, " "))
		_ = f.Close()
	}
	port := argFromFields(agentArgs, "--port")
	if port == "" {
		os.Exit(2)
	}
	markerPath := os.Getenv("AHSIR_TEST_IDLE_MARKER")
	idleAfterMs, _ := strconv.Atoi(os.Getenv("AHSIR_TEST_IDLE_AFTER_MS"))
	exitCode, _ := strconv.Atoi(os.Getenv("AHSIR_TEST_IDLE_EXIT_CODE"))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Root handler: the scheduler's A2A proxy forwards to the agent's base URL
	// (http://127.0.0.1:<port>/). Answering it lets a proxy-path wake test
	// (issue #20) assert the dispatch reached the *woken* runtime, not a dead
	// cached port.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"ok":true},"id":"t"}`))
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	woken := false
	if markerPath != "" {
		if _, err := os.Stat(markerPath); err == nil {
			woken = true
		}
	}
	if woken {
		// The activator brought us back — stay resident until killed.
		<-sigCh
		os.Exit(0)
	}

	// First instance: run briefly, then scale to zero (or crash).
	select {
	case <-sigCh:
		os.Exit(0)
	case <-time.After(time.Duration(idleAfterMs) * time.Millisecond):
	}
	if markerPath != "" {
		_ = os.WriteFile(markerPath, []byte("woken"), 0o644)
	}
	os.Exit(exitCode)
}

// waitForIdleStopped blocks until `name` appears in the scheduler's
// idle-stopped set (or fails after timeout).
func waitForIdleStopped(t *testing.T, sch *Scheduler, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, n := range sch.IdleStoppedAgents() {
			if n == name {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %q to become idle-stopped", name)
}

func newIdleTestScheduler(t *testing.T, dir string, cmd agentCommandBuilder) *Scheduler {
	t.Helper()
	portStart := freePort(t)
	if portStart > 65535-40 {
		portStart -= 40
	}
	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      0,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: portStart, End: portStart + 40},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.supervisor.InitialBackoff = 10 * time.Millisecond
	sch.supervisor.MaxBackoff = 10 * time.Millisecond
	// Tighten the wake health poll so waitAgentHealthy returns quickly once the
	// woken process binds.
	sch.supervisor.HealthStartupGrace = 50 * time.Millisecond
	sch.supervisor.HealthInterval = 20 * time.Millisecond
	sch.agentCommand = cmd
	return sch
}

// TestSchedulerIdleExitMarksIdleStoppedNoRestart: an agent that self-exits with
// IdleStopExitCode is marked idle-stopped and NOT restarted (spec §4.3, R3).
func TestSchedulerIdleExitMarksIdleStoppedNoRestart(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 80, wrapper.IdleStopExitCode))

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, logPath, 1, testLifecycleDeadline)
	waitForIdleStopped(t, sch, "worker", testLifecycleDeadline)

	// Give the supervisor ample time to (wrongly) restart — it must not.
	time.Sleep(600 * time.Millisecond)
	lines := readLines(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("idle-stopped agent was restarted: got %d start lines, want 1 (%q)", len(lines), lines)
	}
	sch.mu.Lock()
	_, up := sch.agents["worker"]
	sch.mu.Unlock()
	if up {
		t.Fatal("idle-stopped agent should not be in the running set")
	}
}

// TestSchedulerNonIdleExitStillRestarts contrasts the idle path: a plain
// non-zero (crash) exit still goes through the supervisor restart, and the
// agent is NOT marked idle-stopped.
func TestSchedulerNonIdleExitStillRestarts(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	// exitCode 1 = crash. The marker makes the restarted instance stay up, so we
	// land on exactly 2 starts — proof a restart happened.
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 50, 1))

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, logPath, 2, testLifecycleDeadline)
	if got := sch.IdleStoppedAgents(); len(got) != 0 {
		t.Fatalf("crashed agent must not be idle-stopped, got %v", got)
	}
}

// TestSchedulerActivatorWakesIdleStopped: after an agent scales to zero, the
// activator brings it back and confirms it healthy (spec §4.4, R5 continuity is
// via the preserved workspace/sessionID which the woken process resumes).
func TestSchedulerActivatorWakesIdleStopped(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 80, wrapper.IdleStopExitCode))

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, logPath, 1, testLifecycleDeadline)
	waitForIdleStopped(t, sch, "worker", testLifecycleDeadline)

	if err := sch.ensureAwake("worker"); err != nil {
		t.Fatalf("ensureAwake failed: %v", err)
	}
	// Woken: a second start, back in the running set, no longer idle-stopped.
	_ = waitForLines(t, logPath, 2, testLifecycleDeadline)
	if got := sch.IdleStoppedAgents(); len(got) != 0 {
		t.Fatalf("agent still idle-stopped after wake: %v", got)
	}
	sch.mu.Lock()
	_, up := sch.agents["worker"]
	sch.mu.Unlock()
	if !up {
		t.Fatal("woken agent not in the running set")
	}
}

// TestSchedulerActivatorReusesOnlyDynamicPortAfterIdleStop locks the dynamic
// range to one port. Once the initial process has scaled to zero that port is
// no longer in use, so the activator must be able to lease it again instead of
// treating the monotonically-advanced allocation cursor as permanent
// exhaustion.
func TestSchedulerActivatorReusesOnlyDynamicPortAfterIdleStop(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	dynamicPort := freePort(t)
	registryPort := freePort(t)
	for registryPort == dynamicPort {
		registryPort = freePort(t)
	}
	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      0,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: registryPort},
		PortRange: PortRange{Start: dynamicPort, End: dynamicPort},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.supervisor.HealthStartupGrace = 50 * time.Millisecond
	sch.supervisor.HealthInterval = 20 * time.Millisecond
	sch.agentCommand = idleAgentCommand(logPath, marker, 80, wrapper.IdleStopExitCode)

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, logPath, 1, testLifecycleDeadline)
	waitForIdleStopped(t, sch, "worker", testLifecycleDeadline)
	if err := sch.ensureAwake("worker"); err != nil {
		t.Fatalf("wake should reuse released dynamic port %d: %v", dynamicPort, err)
	}

	lines := waitForLines(t, logPath, 2, testLifecycleDeadline)
	if got := argValue(lines[1], "--port"); got != strconv.Itoa(dynamicPort) {
		t.Fatalf("woken agent port = %q, want released port %d; starts=%q", got, dynamicPort, lines)
	}
}

func TestSchedulerRepeatedIdleWakeExceedsDynamicRangeSize(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	dynamicPort := freePort(t)
	registryPort := freePort(t)
	for registryPort == dynamicPort {
		registryPort = freePort(t)
	}
	cfg := &Config{
		Agents:    []AgentConfig{{Name: "worker", Workspace: dir}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: registryPort},
		PortRange: PortRange{Start: dynamicPort, End: dynamicPort},
	}
	cfg.nextPort = dynamicPort
	sch := New(cfg)
	sch.supervisor.HealthStartupGrace = 50 * time.Millisecond
	sch.supervisor.HealthInterval = 20 * time.Millisecond
	// With no marker every process scales to zero, including woken processes.
	sch.agentCommand = idleAgentCommand(logPath, "", 80, wrapper.IdleStopExitCode)
	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	const wakeCount = 3 // greater than the one-port range size
	for i := 0; i < wakeCount; i++ {
		waitForIdleStopped(t, sch, "worker", testLifecycleDeadline)
		if err := sch.ensureAwake("worker"); err != nil {
			t.Fatalf("wake %d: %v", i+1, err)
		}
	}
	lines := waitForLines(t, logPath, wakeCount+1, testLifecycleDeadline)
	for i, line := range lines {
		if got := argValue(line, "--port"); got != strconv.Itoa(dynamicPort) {
			t.Fatalf("start %d port=%q, want reused %d", i+1, got, dynamicPort)
		}
	}
}

// TestSchedulerActivatorSingleFlight: concurrent wakes of one idle-stopped agent
// spawn exactly one new process (R2 — no double-start, no port race).
func TestSchedulerActivatorSingleFlight(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 80, wrapper.IdleStopExitCode))

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, logPath, 1, testLifecycleDeadline)
	waitForIdleStopped(t, sch, "worker", testLifecycleDeadline)

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = sch.ensureAwake("worker")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent wake %d failed: %v", i, err)
		}
	}
	// Exactly one respawn: 1 (initial) + 1 (single-flight wake) = 2 start lines.
	time.Sleep(200 * time.Millisecond)
	if lines := readLines(t, logPath); len(lines) != 2 {
		t.Fatalf("single-flight violated: got %d start lines, want 2 (%q)", len(lines), lines)
	}
}

// A wake readiness rollback may restore idle-stopped after the failed process
// has already exited and queued a supervisor restart. That pending restart must
// observe the intentional idle state and become a no-op, or it races the next
// activator and can double-start the Agent.
func TestScheduledRestartDoesNotResurrectIdleStoppedAgent(t *testing.T) {
	dir := t.TempDir()
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(filepath.Join(dir, "starts.log"), filepath.Join(dir, "marker"), 1000, wrapper.IdleStopExitCode))
	var starts atomic.Int32
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}
	sch.mu.Lock()
	sch.ctx = context.Background()
	sch.running = true
	cfg := AgentConfig{Name: "worker", Workspace: dir, Port: sch.cfg.PortRange.Start}
	sch.desired["worker"] = cfg
	sch.idleStopped["worker"] = cfg
	sch.scheduleRestartLocked(cfg, 1, 10*time.Millisecond)
	sch.mu.Unlock()
	time.Sleep(80 * time.Millisecond)
	if got := starts.Load(); got != 0 {
		t.Fatalf("pending supervisor restart resurrected idle-stopped Agent %d time(s)", got)
	}
}

func TestFailedWakeRollbackDoesNotMarkReplacementIdle(t *testing.T) {
	dir := t.TempDir()
	sch := newIdleTestScheduler(t, dir, nil)
	sch.running = true
	sch.ctx = context.Background()
	cfg := AgentConfig{Name: "worker", Workspace: dir, Port: sch.cfg.PortRange.Start}
	oldDone := make(chan struct{})
	close(oldDone)
	oldProc := &agentProcess{cfg: cfg, done: oldDone, cancel: func() {}}
	replacement := &agentProcess{cfg: cfg, cancel: func() {}}
	sch.desired["worker"] = cfg
	sch.agents["worker"] = replacement
	sch.registry.Register(&a2a.AgentCard{Name: "worker", URL: "http://127.0.0.1:1"})

	sch.rollbackFailedWake("worker", oldProc)
	if _, idle := sch.idleStopped["worker"]; idle {
		t.Fatal("rollback of failed old process marked a live supervisor replacement idle-stopped")
	}
	if got := sch.agents["worker"]; got != replacement {
		t.Fatal("rollback disturbed the supervisor replacement process")
	}
	if _, ok := sch.registry.Get("worker"); !ok {
		t.Fatal("rollback unregistered the live supervisor replacement")
	}
}

func TestSchedulerActivatorSingleFlightPropagatesStartFailure(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: port, End: port},
	}
	cfg.nextPort = port
	sch := New(cfg)
	sch.running = true
	sch.ctx = context.Background()
	wakeCfg := AgentConfig{Name: "worker", Workspace: dir, Port: port}
	sch.desired["worker"] = wakeCfg
	sch.idleStopped["worker"] = wakeCfg
	sch.findLocalListener = func(int) (localProcessInfo, bool, error) {
		return localProcessInfo{}, false, nil
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int32
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		if starts.Add(1) == 1 {
			close(entered)
		}
		<-release
		return exec.CommandContext(ctx, filepath.Join(dir, "missing-ahsir-agent"))
	}

	const callers = 6
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	go func() {
		defer wg.Done()
		errs[0] = sch.ensureAwake("worker")
	}()
	<-entered
	for i := 1; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = sch.ensureAwake("worker")
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("concurrent wake %d returned nil after leader start failure", i)
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("start attempts = %d, want one shared failed wake attempt", got)
	}
}

func TestSchedulerActivatorHealthFailureRemainsRetryable(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	port := freePort(t)
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: port, End: port},
	}
	cfg.nextPort = port
	sch := New(cfg)
	sch.running = true
	sch.ctx = context.Background()
	defer sch.Stop()
	sch.supervisor.HealthCheckEnabled = false
	sch.supervisor.HealthStartupGrace = 0
	sch.supervisor.HealthInterval = 10 * time.Millisecond
	wakeCfg := AgentConfig{Name: "worker", Workspace: dir, Port: port}
	sch.desired["worker"] = wakeCfg
	sch.idleStopped["worker"] = wakeCfg
	sch.findLocalListener = func(int) (localProcessInfo, bool, error) {
		return localProcessInfo{}, false, nil
	}

	var starts atomic.Int32
	healthyCommand := healthAgentCommand(logPath, "healthy", 0)
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		if starts.Add(1) == 1 {
			// Starts successfully but never binds /healthz.
			return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
		}
		return healthyCommand(ctx, agentExe, cfg, registryURL)
	}

	if err := sch.ensureAwake("worker"); err == nil {
		t.Fatal("first wake unexpectedly succeeded without a health endpoint")
	}
	sch.mu.Lock()
	_, stillRunning := sch.agents["worker"]
	_, retryable := sch.idleStopped["worker"]
	sch.mu.Unlock()
	if stillRunning {
		t.Error("failed wake left an agent in the running map")
	}
	if !retryable {
		t.Error("failed wake did not restore idle-stopped state")
	}

	if err := sch.ensureAwake("worker"); err != nil {
		t.Fatalf("retry wake after health failure: %v", err)
	}
	if got := starts.Load(); got != 2 {
		t.Fatalf("start attempts = %d, want failed attempt plus retry", got)
	}
	sch.mu.Lock()
	_, running := sch.agents["worker"]
	_, idle := sch.idleStopped["worker"]
	sch.mu.Unlock()
	if !running || idle {
		t.Fatalf("successful retry state: running=%v idleStopped=%v", running, idle)
	}

}

func TestSchedulerPooledFreshSpawnSingleFlightIncludesReadinessAndRollback(t *testing.T) {
	dir := t.TempDir()
	writeInstanceTestCard(t, dir)
	port := freePort(t)
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: port, End: port + 2},
	}
	cfg.nextPort = port
	sch := New(cfg)
	sch.running = true
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	sch.ctx = firstCtx
	sch.supervisor.HealthCheckEnabled = false
	sch.supervisor.HealthStartupGrace = 0
	sch.supervisor.HealthInterval = 10 * time.Millisecond
	sch.desired["worker"] = AgentConfig{Name: "worker", Workspace: dir, Instances: 2}

	entered := make(chan struct{})
	var starts atomic.Int32
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		if starts.Add(1) == 1 {
			close(entered)
		}
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}

	const callers = 5
	errs := make([]error, callers)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs[0] = sch.startOrWakeInstance("worker", 1)
	}()
	<-entered
	for i := 1; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = sch.startOrWakeInstance("worker", 1)
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	cancelFirst() // fail readiness promptly; rollback must be shared by all waiters
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("pooled caller %d returned success before readiness", i)
		}
		if err.Error() != errs[0].Error() {
			t.Errorf("pooled caller %d error = %q, want shared %q", i, err, errs[0])
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("pooled failed readiness started %d processes, want 1", got)
	}
	sch.mu.Lock()
	_, running := sch.agents["worker#1"]
	_, retryable := sch.idleStopped["worker#1"]
	sch.mu.Unlock()
	if running || !retryable {
		t.Fatalf("pooled rollback state: running=%v idleStopped=%v, want false/true", running, retryable)
	}

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	sch.ctx = secondCtx
	sch.agentCommand = healthAgentCommand(filepath.Join(dir, "pooled-retry.log"), "healthy", 0)
	if err := sch.startOrWakeInstance("worker", 1); err != nil {
		t.Fatalf("pooled retry after readiness rollback: %v", err)
	}
	sch.mu.Lock()
	proc := sch.agents["worker#1"]
	sch.mu.Unlock()
	if proc == nil {
		t.Fatal("pooled retry returned success without a running child")
	}
	sch.mu.Lock()
	proc.stopping = true
	sch.mu.Unlock()
	killAgentProcess(proc)
	proc.cancel()
}

func TestRollbackFailedWakeRetainsPortReservationUntilMonitorExit(t *testing.T) {
	sch := New(&Config{})
	sch.running = true
	sch.ctx = context.Background()
	done := make(chan struct{})
	proc := &agentProcess{
		cfg:    AgentConfig{Name: "worker", Workspace: t.TempDir(), Port: 9801},
		cancel: func() {},
		done:   done,
	}
	sch.agents["worker"] = proc
	sch.desired["worker"] = proc.cfg

	returned := make(chan struct{})
	go func() {
		sch.rollbackFailedWake("worker", proc)
		close(returned)
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-returned:
		t.Fatal("rollback returned before monitor confirmed process exit")
	default:
	}
	sch.mu.Lock()
	got := sch.agents["worker"]
	sch.mu.Unlock()
	if got != proc {
		t.Fatal("rollback released agents/port reservation before process exit")
	}

	// Simulate monitorAgent completing Wait and releasing the reservation.
	sch.mu.Lock()
	delete(sch.agents, "worker")
	sch.mu.Unlock()
	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("rollback did not finish after monitor exit")
	}
	sch.mu.Lock()
	_, idle := sch.idleStopped["worker"]
	sch.mu.Unlock()
	if !idle {
		t.Fatal("rollback did not restore idle-stopped state after monitor exit")
	}
}

// TestSchedulerStoppedAgentNotWoken: an explicitly stopped (or archived) agent
// is never resurrected by the activator (spec §4.5).
func TestSchedulerStoppedAgentNotWoken(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 80, wrapper.IdleStopExitCode))

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, logPath, 1, testLifecycleDeadline)
	waitForIdleStopped(t, sch, "worker", testLifecycleDeadline)

	// Explicit stop drops it from desired + idle-stopped.
	if err := sch.StopAgent("worker"); err != nil {
		t.Fatal(err)
	}
	if got := sch.IdleStoppedAgents(); len(got) != 0 {
		t.Fatalf("stopped agent should not remain idle-stopped: %v", got)
	}
	if err := sch.ensureAwake("worker"); err != nil {
		t.Fatalf("ensureAwake on a stopped agent should be a no-op, got %v", err)
	}
	if _, err := sch.ChatWithAgentAs("worker", "", "", "hi"); err == nil {
		t.Fatal("expected 'not found' chatting a stopped agent, got nil error")
	}
	// No respawn happened.
	time.Sleep(150 * time.Millisecond)
	if lines := readLines(t, logPath); len(lines) != 1 {
		t.Fatalf("stopped agent was woken: got %d start lines, want 1 (%q)", len(lines), lines)
	}
}

// TestSchedulerA2AProxyWakesIdleStopped is the regression for issue #20: a
// dispatch through the public /a2a/{agent} proxy after an idle scale-to-zero
// must transparently re-spawn the runtime and route to the fresh port, instead
// of dialing the dead cached endpoint and returning 502/404. This is the path
// Hetairoi's autonomous loop uses, so the very first dispatch after any quiet
// period exercised exactly this bug.
func TestSchedulerA2AProxyWakesIdleStopped(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 80, wrapper.IdleStopExitCode))

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	// Expose the scheduler gateway exactly as production does, so the POST below
	// travels the real handleA2AProxy path (not an in-process shortcut).
	regHandler := registry.NewHTTPHandler(sch.Registry())
	gw := newGatewayHandler(sch, regHandler)
	proxySrv := httptest.NewServer(gw)
	defer proxySrv.Close()

	_ = waitForLines(t, logPath, 1, testLifecycleDeadline)
	waitForIdleStopped(t, sch, "worker", testLifecycleDeadline)

	// Dispatch through the proxy while the agent is scaled to zero.
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","id":"t","params":{"message":{"role":"user","parts":[{"kind":"text","text":"wake"}]}}}`)
	resp, err := http.Post(proxySrv.URL+"/a2a/worker", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /a2a/worker: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	// Before the fix the proxy dialed the dead cached port → 502 (or 404 when
	// the stale card had already been evicted). After the fix the woken runtime
	// answers 200.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy dispatch to idle-stopped agent failed to wake it: status=%d body=%s", resp.StatusCode, respBody)
	}

	// Proof of a genuine re-spawn: a 2nd start line, back in the running set,
	// no longer idle-stopped.
	_ = waitForLines(t, logPath, 2, testLifecycleDeadline)
	if got := sch.IdleStoppedAgents(); len(got) != 0 {
		t.Fatalf("agent still idle-stopped after proxy wake: %v", got)
	}
	sch.mu.Lock()
	_, up := sch.agents["worker"]
	sch.mu.Unlock()
	if !up {
		t.Fatal("woken agent not in the running set after proxy dispatch")
	}
}
