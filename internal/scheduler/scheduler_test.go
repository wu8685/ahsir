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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

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
		cfg:           AgentConfig{Name: "teacher"},
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	lines := waitForLines(t, countPath, 2, 2*time.Second)
	firstPort := argValue(lines[0], "--port")
	secondPort := argValue(lines[1], "--port")
	if firstPort == "" || secondPort == "" {
		t.Fatalf("missing --port in restart args: %q", lines)
	}
	if firstPort != secondPort {
		t.Fatalf("restart should reuse port: first %s second %s", firstPort, secondPort)
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, countPath, 2, 2*time.Second)
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
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for restart continuation dispatch")
	}

	assertLedgerStatus(t, sch.Invocations().Snapshot(), rec.ID, InvocationStatusRecovered, "")
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, countPath, 1, 2*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, countPath, 1, 2*time.Second)
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, countPath, 1, 2*time.Second)
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
