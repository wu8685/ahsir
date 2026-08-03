package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	ahprocess "github.com/wu8685/ahsir/internal/process"
	"github.com/wu8685/ahsir/internal/wrapper"
)

// testLifecycleDeadline bounds "wait for a process-lifecycle effect to land"
// polls and the scheduler-lifetime context fed to Start in these tests. It is
// deliberately generous: the assertions are about BEHAVIOUR (an agent
// restarts, reuses its port, a child exits), not latency. Under `-race` and a
// CPU-starved parallel suite, fork/exec of a shell agent plus goroutine
// scheduling can take well over a second, so a tight 2s deadline produced a
// flaky "got 0 lines" failure. Waits return as soon as the condition holds,
// so a large ceiling costs nothing on a healthy machine and only absorbs
// scheduling jitter under load. The scheduler is still torn down by
// `defer sch.Stop()`, so this ctx timeout is a safety net, not the cleanup
// mechanism.
const testLifecycleDeadline = 30 * time.Second

// freePort finds an available TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// mockA2AServer starts an httptest server that speaks basic A2A JSON-RPC.
func mockA2AServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			ID      string          `json:"id"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "message/send":
			task := a2a.NewSubmittedTask(a2a.TaskInfo{}, nil)
			task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
			task.History = []*a2a.Message{
				a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "response from agent"}),
			}
			result, _ := json.Marshal(task)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"result":  json.RawMessage(result),
				"id":      req.ID,
			})
		case "tasks/get":
			var params struct {
				ID string `json:"id"`
			}
			json.Unmarshal(req.Params, &params)
			task := &a2a.Task{
				ID:     a2a.TaskID(params.ID),
				Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			}
			result, _ := json.Marshal(task)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"result":  json.RawMessage(result),
				"id":      req.ID,
			})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"error":   map[string]interface{}{"code": -32601, "message": "Method not found"},
				"id":      req.ID,
			})
		}
	}))
	return server, server.URL
}

func TestNewScheduler(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
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
			Port: freePort(t),
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
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)

	_, err := sch.ChatWithAgent("nonexistent", "", "hello")
	if err == nil {
		t.Error("expected error for non-existent agent")
	}

	// Start a mock A2A server and register it
	mockSrv, mockURL := mockA2AServer(t)
	defer mockSrv.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "test-agent",
		Version:            "1.0.0",
		URL:                mockURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})
	resp, err := sch.ChatWithAgent("test-agent", "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if resp == "" {
		t.Error("expected non-empty response")
	}
}

func TestSchedulerChatWithAgentAddsInternalToken(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)

	var sawToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Ahsir-Internal-Token")
		if sawToken != "scheduler-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeTestA2AReply(t, w, "token accepted")
	}))
	defer upstream.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})
	sch.agents["teacher"] = &agentProcess{
		cfg:           AgentConfig{Name: "teacher", Port: portOfURL(t, upstream.URL)},
		internalToken: "scheduler-token",
	}

	resp, err := sch.ChatWithAgent("teacher", "ctx-token", "hello")
	if err != nil {
		t.Fatalf("ChatWithAgent: %v", err)
	}
	if !strings.Contains(resp, "token accepted") {
		t.Fatalf("response = %q", resp)
	}
	if sawToken != "scheduler-token" {
		t.Fatalf("chat token header = %q, want scheduler-token", sawToken)
	}
}

func TestSchedulerChatWithAgentZeroChatTimeoutDoesNotExpireImmediately(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		Timeouts:  TimeoutsConfig{Chat: "0s"},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)

	mockSrv, mockURL := mockA2AServer(t)
	defer mockSrv.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "test-agent",
		Version:            "1.0.0",
		URL:                mockURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	resp, err := sch.ChatWithAgent("test-agent", "", "hello")
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
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()

	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify registry is actually listening
	time.Sleep(50 * time.Millisecond)
	conn, err := net.DialTimeout("tcp", sch.httpSrv.Addr, 500*time.Millisecond)
	if err != nil {
		t.Fatal("registry not listening:", err)
	}
	conn.Close()

	sch.Stop()
}

func TestDefaultAgentCommandStartsProcessGroup(t *testing.T) {
	cmd := defaultAgentCommand(context.Background(), "ahsir-agent", AgentConfig{
		Name:      "teacher",
		Workspace: t.TempDir(),
		Port:      freePort(t),
	}, "http://127.0.0.1:9800")

	if !ahprocess.CommandStartsProcessGroup(cmd) {
		t.Fatalf("default agent command should start an isolated process group")
	}
}

func TestStopAgentKillsAgentProcessTree(t *testing.T) {
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      freePort(t),
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.agentCommand = treeAgentCommand(childPIDPath)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	childPID := waitForPIDFile(t, childPIDPath)
	defer killPID(childPID)
	if !pidExists(childPID) {
		t.Fatalf("child pid %d should exist before StopAgent", childPID)
	}

	if err := sch.StopAgent("worker"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(testLifecycleDeadline)
	for pidExists(childPID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidExists(childPID) {
		t.Fatalf("child pid %d still exists after StopAgent", childPID)
	}
}

func TestStartAgentEvictsStaleLocalAgentBeforeStart(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      port,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	var evictedPID int
	sch.findLocalListener = func(gotPort int) (localProcessInfo, bool, error) {
		if gotPort != port {
			t.Fatalf("listener lookup port=%d, want %d", gotPort, port)
		}
		return localProcessInfo{
			PID:     12345,
			Command: "/tmp/ahsir-agent --workspace " + dir,
			Cwd:     dir,
		}, true, nil
	}
	sch.killLocalProcessTree = func(pid int) error {
		evictedPID = pid
		return nil
	}
	sch.agentCommand = healthAgentCommand(filepath.Join(dir, "starts.log"), "healthy", 0)

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	if evictedPID != 12345 {
		t.Fatalf("evictedPID=%d, want 12345", evictedPID)
	}
}

func TestStartAgentRejectsUnrelatedListener(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      port,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.findLocalListener = func(int) (localProcessInfo, bool, error) {
		return localProcessInfo{
			PID:     12345,
			Command: "/usr/sbin/other-service",
			Cwd:     "/tmp/other",
		}, true, nil
	}
	sch.killLocalProcessTree = func(pid int) error {
		t.Fatalf("unrelated listener should not be killed, pid=%d", pid)
		return nil
	}
	sch.agentCommand = healthAgentCommand(filepath.Join(dir, "starts.log"), "healthy", 0)

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	err := sch.Start(ctx)
	if err == nil {
		defer sch.Stop()
		t.Fatal("expected unrelated listener error")
	}
	if !strings.Contains(err.Error(), "port") || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSchedulerRestartsLocalAgentAfterUnexpectedExit(t *testing.T) {
	dir := t.TempDir()
	countPath := filepath.Join(dir, "starts.log")
	scriptPath := filepath.Join(dir, "agent.sh")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit 0\n", countPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	portStart := freePort(t)

	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      0,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: portStart, End: portStart + 20},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.supervisor.InitialBackoff = 10 * time.Millisecond
	sch.supervisor.MaxBackoff = 10 * time.Millisecond
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		return exec.CommandContext(ctx, scriptPath,
			"--workspace", cfg.Workspace,
			"--port", fmt.Sprint(cfg.Port),
			"--registry", registryURL,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	lines := waitForLines(t, countPath, 2, testLifecycleDeadline)
	firstPort := argValue(lines[0], "--port")
	secondPort := argValue(lines[1], "--port")
	if firstPort == "" || secondPort == "" {
		t.Fatalf("missing --port in restart args: %q", lines)
	}
	if firstPort != secondPort {
		t.Fatalf("restart should reuse port: first %s second %s", firstPort, secondPort)
	}
}

func TestSchedulerReleasedDynamicPortCanBeReusedByLaterAgent(t *testing.T) {
	dynamicPort := freePort(t)
	registryPort := freePort(t)
	for registryPort == dynamicPort {
		registryPort = freePort(t)
	}
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: registryPort},
		PortRange: PortRange{Start: dynamicPort, End: dynamicPort},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	firstDir := t.TempDir()
	if port, err := sch.StartAgent(AgentConfig{Name: "first", Workspace: firstDir}); err != nil {
		t.Fatalf("start first dynamic agent: %v", err)
	} else if port != dynamicPort {
		t.Fatalf("first agent port = %d, want %d", port, dynamicPort)
	}
	if err := sch.StopAgent("first"); err != nil {
		t.Fatalf("stop first dynamic agent: %v", err)
	}
	deadline := time.Now().Add(testLifecycleDeadline)
	for {
		sch.mu.Lock()
		_, stillRunning := sch.agents["first"]
		sch.mu.Unlock()
		if !stillRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for first dynamic agent to exit")
		}
		time.Sleep(10 * time.Millisecond)
	}

	secondDir := t.TempDir()
	port, err := sch.StartAgent(AgentConfig{Name: "second", Workspace: secondDir})
	if err != nil {
		t.Fatalf("start second agent on released dynamic port: %v", err)
	}
	if port != dynamicPort {
		t.Fatalf("second agent port = %d, want released port %d", port, dynamicPort)
	}
}

func TestSchedulerConcurrentDistinctAgentsAllocateDistinctDynamicPorts(t *testing.T) {
	portStart := freePort(t)
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: portStart, End: portStart + 1},
	}
	cfg.nextPort = portStart
	sch := New(cfg)
	sch.running = true
	sch.ctx = context.Background()
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}
	defer sch.Stop()

	ports := make([]int, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range ports {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ports[i], errs[i] = sch.StartAgent(AgentConfig{Name: fmt.Sprintf("worker-%d", i), Workspace: t.TempDir()})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("start worker-%d: %v", i, err)
		}
	}
	if ports[0] == ports[1] {
		t.Fatalf("concurrent dynamic starts shared port %d", ports[0])
	}
}

func TestSchedulerExplicitPinnedPortRejectsSchedulerReservationBeforeBind(t *testing.T) {
	port := freePort(t)
	sch := New(&Config{PortRange: PortRange{Start: port, End: port + 1}})
	sch.running = true
	sch.ctx = context.Background()
	sch.agents["first"] = &agentProcess{cfg: AgentConfig{Name: "first", Workspace: t.TempDir(), Port: port}}
	sch.desired["first"] = sch.agents["first"].cfg
	// Model the window after cmd.Start/publication but before the first process
	// binds its listener. OS probing alone reports the port as free here.
	sch.findLocalListener = func(int) (localProcessInfo, bool, error) {
		return localProcessInfo{}, false, nil
	}
	var starts atomic.Int32
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}

	_, err := sch.StartAgent(AgentConfig{Name: "second", Workspace: t.TempDir(), Port: port})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("same pinned port error = %v, want scheduler reservation conflict", err)
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("same pinned port spawned %d processes, want 0", got)
	}
}

func TestSameCleanPathResolvesSymlinkedParentForManagedWorkspace(t *testing.T) {
	realParent := t.TempDir()
	aliasParent := filepath.Join(t.TempDir(), "managed-root-alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	// The managed Agent directory has not been scaffolded yet; ownership still
	// has to recognize that both names resolve beneath the same physical root.
	realWorkspace := filepath.Join(realParent, "cma-worker-v1")
	aliasWorkspace := filepath.Join(aliasParent, "cma-worker-v1")
	if !sameCleanPath(realWorkspace, aliasWorkspace) {
		t.Fatalf("managed workspace aliases were not canonicalized: %q vs %q", realWorkspace, aliasWorkspace)
	}
}

func TestSchedulerDynamicPortSkipsForeignListener(t *testing.T) {
	start := freePort(t)
	sch := New(&Config{PortRange: PortRange{Start: start, End: start + 1}, nextPort: start})
	sch.running = true
	sch.ctx = context.Background()
	sch.findLocalListener = func(port int) (localProcessInfo, bool, error) {
		if port == start {
			return localProcessInfo{PID: 42, Command: "/usr/sbin/foreign-service"}, true, nil
		}
		return localProcessInfo{}, false, nil
	}
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}
	defer sch.Stop()

	port, err := sch.StartAgent(AgentConfig{Name: "worker", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if port != start+1 {
		t.Fatalf("dynamic port = %d, want %d after skipping foreign listener", port, start+1)
	}
}

func TestSchedulerFullyReservedDynamicRangeFailsWithoutSpawn(t *testing.T) {
	portStart := freePort(t)
	cfg := &Config{PortRange: PortRange{Start: portStart, End: portStart + 1}}
	cfg.nextPort = portStart
	sch := New(cfg)
	sch.running = true
	sch.ctx = context.Background()
	sch.agents["held-a"] = &agentProcess{cfg: AgentConfig{Name: "held-a", Port: portStart}}
	sch.agents["held-b"] = &agentProcess{cfg: AgentConfig{Name: "held-b", Port: portStart + 1}}
	var starts atomic.Int32
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}

	_, err := sch.StartAgent(AgentConfig{Name: "blocked", Workspace: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no available ports") {
		t.Fatalf("fully reserved range error = %v, want no available ports", err)
	}
	if got := starts.Load(); got != 0 {
		t.Fatalf("fully reserved range spawned %d processes, want 0", got)
	}
}

func TestSchedulerEarlierDynamicAgentDoesNotTakePinnedConfiguredPort(t *testing.T) {
	pinnedPort := freePort(t)
	registryPort := freePort(t)
	for registryPort == pinnedPort || registryPort == pinnedPort+1 {
		registryPort = freePort(t)
	}
	cfg := &Config{
		Agents: []AgentConfig{
			{Name: "dynamic", Workspace: t.TempDir(), Port: 0},
			{Name: "pinned", Workspace: t.TempDir(), Port: pinnedPort},
		},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: registryPort},
		PortRange: PortRange{Start: pinnedPort, End: pinnedPort + 1},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	sch.mu.Lock()
	dynamicGot := sch.agents["dynamic"].cfg.Port
	pinnedGot := sch.agents["pinned"].cfg.Port
	sch.mu.Unlock()
	if pinnedGot != pinnedPort {
		t.Fatalf("pinned agent port = %d, want %d", pinnedGot, pinnedPort)
	}
	if dynamicGot == pinnedPort {
		t.Fatalf("earlier dynamic agent took later configured agent's pinned port %d", pinnedPort)
	}
}

func TestSchedulerRestartTriggersContinuationForFailedInvocation(t *testing.T) {
	dir := t.TempDir()
	countPath := filepath.Join(dir, "starts.log")
	scriptPath := filepath.Join(dir, "agent.sh")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit 0\n", countPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	portStart := freePort(t)

	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      0,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: portStart, End: portStart + 20},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.supervisor.InitialBackoff = 10 * time.Millisecond
	sch.supervisor.MaxBackoff = 10 * time.Millisecond
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		return exec.CommandContext(ctx, scriptPath,
			"--workspace", cfg.Workspace,
			"--port", fmt.Sprint(cfg.Port),
			"--registry", registryURL,
		)
	}

	rec := sch.Invocations().Begin(InvocationMetadata{
		Source:    InvocationSourceA2AProxy,
		AgentName: "worker",
		ContextID: "ctx-restart-continuation",
		MessageID: "msg-before-crash",
		UserText:  "continue me after restart",
	})
	sch.Invocations().FailMessage(rec.ID, "agent exited before completion")

	type recoveryCall struct {
		agent   string
		context string
		prompt  string
	}
	calls := make(chan recoveryCall, 1)
	sch.recoveryDispatch = func(ctx context.Context, agentName, contextID, prompt string) (string, error) {
		calls <- recoveryCall{agent: agentName, context: contextID, prompt: prompt}
		return "continued", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, countPath, 2, testLifecycleDeadline)
	select {
	case call := <-calls:
		if call.agent != "worker" {
			t.Fatalf("recovery agent = %q", call.agent)
		}
		if call.context != "ctx-restart-continuation" {
			t.Fatalf("recovery context = %q", call.context)
		}
		if !strings.Contains(call.prompt, "continue the interrupted work") {
			t.Fatalf("unexpected continuation prompt: %q", call.prompt)
		}
	case <-time.After(testLifecycleDeadline):
		t.Fatal("timeout waiting for restart continuation dispatch")
	}

	// The recovery callback signals `calls` from INSIDE dispatch; the ledger
	// is marked Recovered only after dispatch returns. Poll for the terminal
	// state instead of reading a single snapshot at the dispatch instant,
	// which races that write.
	waitForLedgerStatusByID(t, sch, rec.ID, InvocationStatusRecovered, testLifecycleDeadline)
}

// waitForLedgerStatusByID polls the ledger until the record with id reaches
// the wanted status, or fails after timeout.
func waitForLedgerStatusByID(t *testing.T, sch *Scheduler, id string, status InvocationStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, rec := range sch.Invocations().Snapshot() {
			if rec.ID == id && rec.Status == status {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s to reach %q; snapshot=%+v", id, status, sch.Invocations().Snapshot())
}

func TestSchedulerRecoverySendsContinuationForInterruptedContext(t *testing.T) {
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)

	rec := sch.Invocations().Begin(InvocationMetadata{
		Source:    InvocationSourceChatGateway,
		AgentName: "worker",
		ContextID: "ctx-recover",
		UserText:  "long task",
	})
	sch.Invocations().FailMessage(rec.ID, "agent exited")

	var gotAgent, gotContext, gotPrompt string
	sch.recoveryDispatch = func(ctx context.Context, agentName, contextID, prompt string) (string, error) {
		gotAgent = agentName
		gotContext = contextID
		gotPrompt = prompt
		return "continued", nil
	}

	sch.recoverAgentInvocations(context.Background(), "worker")

	if gotAgent != "worker" {
		t.Fatalf("agent = %q", gotAgent)
	}
	if gotContext != "ctx-recover" {
		t.Fatalf("contextID = %q", gotContext)
	}
	if !strings.Contains(gotPrompt, "continue the interrupted work") {
		t.Fatalf("unexpected continuation prompt: %q", gotPrompt)
	}
	snapshot := sch.Invocations().Snapshot()
	assertLedgerStatus(t, snapshot, rec.ID, InvocationStatusRecovered, "")
}

func TestSchedulerRecoverySkipsRecordsWithoutContextID(t *testing.T) {
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)

	rec := sch.Invocations().Begin(InvocationMetadata{
		Source:    InvocationSourceChatGateway,
		AgentName: "worker",
		UserText:  "no context",
	})
	sch.Invocations().FailMessage(rec.ID, "agent exited")

	var calls int
	sch.recoveryDispatch = func(ctx context.Context, agentName, contextID, prompt string) (string, error) {
		calls++
		return "", nil
	}

	sch.recoverAgentInvocations(context.Background(), "worker")

	if calls != 0 {
		t.Fatalf("expected no continuation prompt for empty contextID, got %d calls", calls)
	}
	snapshot := sch.Invocations().Snapshot()
	assertLedgerStatus(t, snapshot, rec.ID, InvocationStatusFailed, "agent exited")
}

func TestSchedulerRecoveryMarksContinuationFailure(t *testing.T) {
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)

	rec := sch.Invocations().Begin(InvocationMetadata{
		Source:    InvocationSourceChatGateway,
		AgentName: "worker",
		ContextID: "ctx-fail-recovery",
	})
	sch.Invocations().FailMessage(rec.ID, "agent exited")

	sch.recoveryDispatch = func(ctx context.Context, agentName, contextID, prompt string) (string, error) {
		return "", fmt.Errorf("agent not ready")
	}

	sch.recoverAgentInvocations(context.Background(), "worker")

	snapshot := sch.Invocations().Snapshot()
	assertLedgerStatus(t, snapshot, rec.ID, InvocationStatusRecoveryFailed, "agent not ready")
}

func TestSchedulerStopAgentDoesNotRestartLocalAgent(t *testing.T) {
	dir := t.TempDir()
	countPath := filepath.Join(dir, "starts.log")
	scriptPath := filepath.Join(dir, "agent.sh")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nsleep 5\n", countPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	portStart := freePort(t)

	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      0,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: portStart, End: portStart + 20},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.supervisor.InitialBackoff = 10 * time.Millisecond
	sch.supervisor.MaxBackoff = 10 * time.Millisecond
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		return exec.CommandContext(ctx, scriptPath,
			"--workspace", cfg.Workspace,
			"--port", fmt.Sprint(cfg.Port),
			"--registry", registryURL,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, countPath, 1, testLifecycleDeadline)
	if err := sch.StopAgent("worker"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	lines := readLines(t, countPath)
	if len(lines) != 1 {
		t.Fatalf("StopAgent should not restart agent, starts=%d lines=%q", len(lines), lines)
	}
}

func TestSchedulerRestartsLocalAgentAfterHealthFailures(t *testing.T) {
	dir := t.TempDir()
	countPath := filepath.Join(dir, "starts.log")
	portStart := freePort(t)

	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      0,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: portStart, End: portStart + 20},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.supervisor.InitialBackoff = 10 * time.Millisecond
	sch.supervisor.MaxBackoff = 10 * time.Millisecond
	sch.supervisor.HealthStartupGrace = 150 * time.Millisecond
	sch.supervisor.HealthInterval = 20 * time.Millisecond
	sch.supervisor.HealthTimeout = 100 * time.Millisecond
	sch.supervisor.HealthFailureThreshold = 2
	sch.agentCommand = healthAgentCommand(countPath, "unhealthy", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	lines := waitForLines(t, countPath, 2, 3*time.Second)
	firstPort := argValue(lines[0], "--port")
	secondPort := argValue(lines[1], "--port")
	if firstPort == "" || secondPort == "" {
		t.Fatalf("missing --port in restart args: %q", lines)
	}
	if firstPort != secondPort {
		t.Fatalf("health restart should reuse port: first %s second %s", firstPort, secondPort)
	}
}

func TestSchedulerDoesNotRestartLocalAgentAfterTransientHealthFailures(t *testing.T) {
	dir := t.TempDir()
	countPath := filepath.Join(dir, "starts.log")
	portStart := freePort(t)

	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      0,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: portStart, End: portStart + 20},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.supervisor.InitialBackoff = 10 * time.Millisecond
	sch.supervisor.MaxBackoff = 10 * time.Millisecond
	sch.supervisor.HealthStartupGrace = 150 * time.Millisecond
	sch.supervisor.HealthInterval = 20 * time.Millisecond
	sch.supervisor.HealthTimeout = 100 * time.Millisecond
	sch.supervisor.HealthFailureThreshold = 3
	sch.agentCommand = healthAgentCommand(countPath, "transient", 2)

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, countPath, 1, testLifecycleDeadline)
	time.Sleep(350 * time.Millisecond)
	lines := readLines(t, countPath)
	if len(lines) != 1 {
		t.Fatalf("transient health failures should not restart agent, starts=%d lines=%q", len(lines), lines)
	}
}

func TestSchedulerStopAgentCancelsHealthWatcher(t *testing.T) {
	dir := t.TempDir()
	countPath := filepath.Join(dir, "starts.log")
	portStart := freePort(t)

	cfg := &Config{
		Agents: []AgentConfig{{
			Name:      "worker",
			Workspace: dir,
			Port:      0,
		}},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: portStart, End: portStart + 20},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	sch.supervisor.HealthStartupGrace = 200 * time.Millisecond
	sch.supervisor.HealthInterval = 20 * time.Millisecond
	sch.supervisor.HealthTimeout = 10 * time.Millisecond
	sch.supervisor.HealthFailureThreshold = 1
	sch.agentCommand = healthAgentCommand(countPath, "unhealthy", 0)

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, countPath, 1, testLifecycleDeadline)
	if err := sch.StopAgent("worker"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	lines := readLines(t, countPath)
	if len(lines) != 1 {
		t.Fatalf("StopAgent should cancel health watcher, starts=%d lines=%q", len(lines), lines)
	}
}

func healthAgentCommand(logPath, mode string, transientFailures int) agentCommandBuilder {
	return func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0],
			"-test.run=TestSchedulerHealthAgentHelperProcess",
			"--",
			"--workspace", cfg.Workspace,
			"--port", fmt.Sprint(cfg.Port),
			"--registry", registryURL,
		)
		cmd.Env = append(os.Environ(),
			"AHSIR_TEST_HEALTH_AGENT=1",
			"AHSIR_TEST_HEALTH_LOG="+logPath,
			"AHSIR_TEST_HEALTH_MODE="+mode,
			"AHSIR_TEST_HEALTH_TRANSIENT_FAILURES="+strconv.Itoa(transientFailures),
		)
		return cmd
	}
}

func treeAgentCommand(childPIDPath string) agentCommandBuilder {
	return func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0],
			"-test.run=TestSchedulerAgentTreeHelperProcess",
			"--",
			childPIDPath,
		)
		cmd.Env = append(os.Environ(), "AHSIR_TEST_TREE_AGENT=1")
		return cmd
	}
}

func TestSchedulerAgentTreeHelperProcess(t *testing.T) {
	if os.Getenv("AHSIR_TEST_TREE_AGENT") != "1" {
		return
	}
	args := argsAfterDashDash(os.Args)
	if len(args) != 1 {
		os.Exit(2)
	}
	child := exec.Command("sleep", "1000")
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(args[0], []byte(strconv.Itoa(child.Process.Pid)), 0o644); err != nil {
		_ = child.Process.Kill()
		os.Exit(2)
	}
	_ = child.Wait()
	os.Exit(0)
}

func TestSchedulerHealthAgentHelperProcess(t *testing.T) {
	if os.Getenv("AHSIR_TEST_HEALTH_AGENT") != "1" {
		return
	}
	agentArgs := argsAfterDashDash(os.Args)
	logPath := os.Getenv("AHSIR_TEST_HEALTH_LOG")
	if logPath != "" {
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
	mode := os.Getenv("AHSIR_TEST_HEALTH_MODE")
	transientFailures, _ := strconv.Atoi(os.Getenv("AHSIR_TEST_HEALTH_TRANSIENT_FAILURES"))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if mode == "unhealthy" {
			http.Error(w, "unhealthy", http.StatusInternalServerError)
			return
		}
		if transientFailures > 0 {
			transientFailures--
			http.Error(w, "warming", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		os.Exit(2)
	}
	os.Exit(0)
}

func argsAfterDashDash(args []string) []string {
	for i, arg := range args {
		if arg == "--" && i+1 < len(args) {
			return args[i+1:]
		}
	}
	return nil
}

func argFromFields(fields []string, flag string) string {
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func waitForLines(t *testing.T, path string, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		lines := readLines(t, path)
		if len(lines) >= want {
			return lines
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines := readLines(t, path)
	t.Fatalf("timeout waiting for %d lines in %s, got %d: %q", want, path, len(lines), lines)
	return nil
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(testLifecycleDeadline)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil {
				t.Fatalf("parse pid file: %v", convErr)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for pid file %s", path)
	return 0
}

func pidExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil
}

func killPID(pid int) {
	if pid > 0 {
		_ = exec.Command("kill", "-KILL", strconv.Itoa(pid)).Run()
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func argValue(line, flag string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func TestSchedulerGetTaskStatus(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)

	// Start a mock A2A server and register it
	mockSrv, mockURL := mockA2AServer(t)
	defer mockSrv.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "test-agent",
		URL:                mockURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	task, err := sch.GetTaskStatus("test-agent", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(task.ID) != "task-1" {
		t.Errorf("expected task-1, got %s", task.ID)
	}
}

// TestIntegrationFullFlow tests the full lifecycle: start scheduler with registry,
// register a mock agent via HTTP, send messages via A2A, and query task status.
func TestIntegrationFullFlow(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	// Give the registry server time to start
	time.Sleep(50 * time.Millisecond)

	// Start a mock A2A agent server
	mockSrv, mockURL := mockA2AServer(t)
	defer mockSrv.Close()

	registryURL := fmt.Sprintf("http://%s:%d", cfg.Registry.Host, cfg.Registry.Port)

	// Step 1: Register agent via HTTP
	card := a2a.AgentCard{
		Name:               "integration-agent",
		Description:        "Integration test agent",
		Version:            "1.0.0",
		URL:                mockURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Skills:             []a2a.AgentSkill{{Name: "testing"}},
	}
	cardData, _ := json.Marshal(card)
	resp, err := http.Post(registryURL+"/agents", "application/json", bytes.NewReader(cardData))
	if err != nil {
		t.Fatalf("register agent via HTTP: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	// Step 2: List agents via HTTP
	resp, err = http.Get(registryURL + "/agents")
	if err != nil {
		t.Fatalf("list agents via HTTP: %v", err)
	}
	var agents []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0]["name"] != "integration-agent" {
		t.Errorf("expected integration-agent, got %v", agents[0]["name"])
	}

	// Step 3: Get agent via HTTP
	resp, err = http.Get(registryURL + "/agents/integration-agent")
	if err != nil {
		t.Fatalf("get agent via HTTP: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	// Step 4: Chat with agent via scheduler
	msg, err := sch.ChatWithAgent("integration-agent", "", "hello integration")
	if err != nil {
		t.Fatalf("chat with agent: %v", err)
	}
	if msg == "" {
		t.Error("expected non-empty response from chat")
	}
	t.Logf("Chat response: %s", msg)

	// Step 5: Get task status via scheduler
	task, err := sch.GetTaskStatus("integration-agent", "task-integration-1")
	if err != nil {
		t.Fatalf("get task status: %v", err)
	}
	if string(task.ID) != "task-integration-1" {
		t.Errorf("expected task-integration-1, got %s", task.ID)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("expected completed, got %s", task.Status.State)
	}

	// Step 6: Verify agent is listed via the AgentRouter interface
	listed := sch.ListAgents()
	if len(listed) != 1 {
		t.Fatalf("expected 1 agent via ListAgents, got %d", len(listed))
	}
	if listed[0].Name != "integration-agent" {
		t.Errorf("expected integration-agent, got %s", listed[0].Name)
	}

	t.Log("Integration test completed successfully")
}

// --- token-leak fix (specs/2026-06-08-auth-baseline.md) ---

// TestManagedAgentTokenGoesToLocalAddressNotCardURL pins the core auth fix:
// the scheduler sends its internal token to the local address it RECORDED for
// a managed agent, never to the registry card's URL (which an unauthenticated
// POST /agents can overwrite to an attacker sink).
func TestManagedAgentTokenGoesToLocalAddressNotCardURL(t *testing.T) {
	// Sink stands in for an attacker endpoint the card URL was overwritten to.
	sinkGotToken := make(chan string, 4)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkGotToken <- r.Header.Get(wrapper.InternalTokenHeader)
		writeTestA2AReply(t, w, "sink reply")
	}))
	defer sink.Close()

	// The real local agent the scheduler "spawned". Its address is what
	// agentProcess records; this is where the token must go.
	localGotToken := make(chan string, 4)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		localGotToken <- r.Header.Get(wrapper.InternalTokenHeader)
		writeTestA2AReply(t, w, "local reply")
	}))
	defer local.Close()
	localPort := portOfURL(t, local.URL)

	sch, gwURL := newTestScheduler(t)
	// Registry card URL points at the SINK (simulating an overwrite).
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                sink.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})
	// The scheduler's own record says teacher is a managed local process on
	// localPort with a known internal token.
	sch.agents["teacher"] = &agentProcess{
		cfg:           AgentConfig{Name: "teacher", Port: localPort},
		internalToken: "real-token",
	}

	// chat → must hit local, not sink.
	if _, err := sch.ChatWithAgent("teacher", "ctx-leak", "hi"); err != nil {
		t.Fatalf("chat: %v", err)
	}
	select {
	case got := <-localGotToken:
		if got != "real-token" {
			t.Fatalf("local agent token = %q, want real-token", got)
		}
	case got := <-sinkGotToken:
		t.Fatalf("TOKEN LEAKED to sink (card URL): %q", got)
	}

	// A2A proxy → same: target the local address, token to local only.
	_ = postA2AMessage(t, gwURL+"/a2a/teacher", "proxy hi")
	select {
	case got := <-localGotToken:
		if got != "real-token" {
			t.Fatalf("proxy local token = %q, want real-token", got)
		}
	case got := <-sinkGotToken:
		t.Fatalf("PROXY TOKEN LEAKED to sink: %q", got)
	}
}

// TestCardOnlyAgentGetsNoInternalToken: an agent the scheduler did NOT spawn
// (registry card only — e.g. a future remote agent) is reachable at its card
// URL but must NOT receive the scheduler's internal token.
func TestCardOnlyAgentGetsNoInternalToken(t *testing.T) {
	gotToken := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken <- r.Header.Get(wrapper.InternalTokenHeader)
		writeTestA2AReply(t, w, "remote reply")
	}))
	defer upstream.Close()

	sch, _ := newTestScheduler(t)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "remote",
		Version:            "1.0.0",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})
	// No entry in sch.agents → not scheduler-managed.

	if _, err := sch.ChatWithAgent("remote", "ctx-remote", "hi"); err != nil {
		t.Fatalf("chat: %v", err)
	}
	got := <-gotToken
	if got != "" {
		t.Fatalf("card-only agent received an internal token %q, want none", got)
	}
}

func portOfURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %s: %v", raw, err)
	}
	return p
}

// --- admin token to agents (specs/2026-06-08-auth-baseline.md) ---

// TestDefaultAgentCommandIncludesAdminToken: the spawned agent receives the
// control-plane token via --admin-token so its registry heartbeat can pass.
func TestDefaultAgentCommandIncludesAdminToken(t *testing.T) {
	cmd := defaultAgentCommand(context.Background(), "ahsir-agent",
		AgentConfig{Name: "w", Workspace: "/tmp/w", Port: 9901, InternalToken: "itok", AdminToken: "atok"},
		"http://127.0.0.1:9800")
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--admin-token atok") {
		t.Fatalf("agent command missing --admin-token: %q", joined)
	}
}
