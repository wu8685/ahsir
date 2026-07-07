package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
