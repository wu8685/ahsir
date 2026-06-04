//go:build e2e

// E2E coverage for the multi-agent example. Lives in the same external
// test package as hello_test.go (example_test), gated behind the e2e
// build tag so it never runs in the default test pipeline — explicit
// opt-in via AHSIR_E2E_CLAUDE=1.
//
// Run:
//
//	AHSIR_E2E_CLAUDE=1 go test -tags=e2e -timeout=5m ./example/multi-agent/ -v
//
// Prerequisites:
//   - `bin/ahsir` and `bin/ahsir-agent` built at repo root (the test skips
//     gracefully with a hint if they're missing).
//   - `claude` CLI on PATH.
//   - MODEL_API_KEY env var pointing at a DeepSeek key (or another
//     Anthropic-compatible endpoint configured in the agent cards).
//
// The fixture generates a temp ahsir.yaml + agent cards in t.TempDir so
// each run is hermetic and uses random free ports — multiple e2e runs
// can execute concurrently without colliding with each other or with a
// user's manually-started scheduler.
package example_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// e2eFixture spawns the scheduler subprocess against a generated config and
// exposes helpers for sending A2A messages and inspecting captured logs.
// Cleanup (SIGKILL + reap) is registered via t.Cleanup so individual tests
// don't have to remember.
type e2eFixture struct {
	t            *testing.T
	repoRoot     string
	registryPort int
	teacherPort  int
	studentPort  int

	cmd     *exec.Cmd
	cancel  context.CancelFunc
	logBuf  *syncBuffer
}

// syncBuffer is a goroutine-safe bytes.Buffer. *bytes.Buffer is not safe for
// concurrent writes, and exec.Cmd's stdout/stderr capture is driven from a
// goroutine inside os/exec — without serialization a parallel reader (the
// test's schedulerLog() helper) could race with the writer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// setupE2E enforces the prereq gates, materializes a hermetic test
// workspace, and starts the scheduler subprocess. The returned fixture is
// ready for sendMessage calls — t.Cleanup tears the subprocess down even
// on test failure.
func setupE2E(t *testing.T) *e2eFixture {
	t.Helper()

	if os.Getenv("AHSIR_E2E_CLAUDE") != "1" {
		t.Skip("set AHSIR_E2E_CLAUDE=1 to run real-claude e2e tests")
	}
	if os.Getenv("MODEL_API_KEY") == "" {
		t.Skip("MODEL_API_KEY required for real-claude e2e tests")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}

	repoRoot := findRepoRoot(t)
	ahsirBin := filepath.Join(repoRoot, "bin", "ahsir")
	ahsirAgentBin := filepath.Join(repoRoot, "bin", "ahsir-agent")
	for _, b := range []string{ahsirBin, ahsirAgentBin} {
		if _, err := os.Stat(b); err != nil {
			t.Skipf("missing %s — from repo root run: go build -o bin/ahsir ./cmd/ahsir && go build -o bin/ahsir-agent ./cmd/ahsir-agent", b)
		}
	}

	// Three free ports: registry + teacher + student. Bound + immediately
	// released so the scheduler can claim them. Race window is small in
	// practice (no other process snipes between release and scheduler
	// listen) — accept it; if it ever flakes we'll switch to a real
	// reservation scheme.
	registryPort, teacherPort, studentPort := allocateFreePorts(t, 3)

	// Generate the temp workspace + cards. Agents are configured WITHOUT
	// filesystem access so the test doesn't depend on any host-specific
	// directories (the production example/multi-agent cards include
	// /Users/wuke/workspace/brain-spark which only exists on the author's
	// laptop).
	tmp := t.TempDir()
	teacherWS := filepath.Join(tmp, "workspaces", "teacher")
	studentWS := filepath.Join(tmp, "workspaces", "student")
	writeAgentCard(t, teacherWS, teacherCardYAML)
	writeAgentCard(t, studentWS, studentCardYAML)

	cfgPath := filepath.Join(tmp, "ahsir.yaml")
	writeSchedulerConfig(t, cfgPath, registryPort, teacherPort, teacherWS, studentWS)

	// Spawn the scheduler. Use a context we control so t.Cleanup can
	// cancel + kill deterministically.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, ahsirBin, "start", cfgPath)
	cmd.Env = os.Environ() // inherit PATH, MODEL_API_KEY, etc.
	// Put the scheduler into its own process group. Without this, killing
	// the scheduler leaves its children (ahsir-agent → claude) alive
	// holding the inherited stdout/stderr pipes — cmd.Wait then blocks
	// indefinitely on its internal pipe-drain goroutines. With a process
	// group we kill the whole tree on cleanup.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logBuf := &syncBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf

	// Run from repo root so any relative paths the scheduler emits in
	// log lines are interpretable. The scheduler discovers ahsir-agent
	// via os.Executable(), not cwd, so this is purely for log clarity.
	cmd.Dir = repoRoot

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start scheduler: %v", err)
	}

	t.Cleanup(func() {
		cancel()
		// Negative pid signals the process GROUP, killing scheduler +
		// all ahsir-agent + all claude subprocesses in one go. Failing
		// to use the group form was the cause of indefinite hangs in
		// cmd.Wait — children kept the inherited pipe ends open.
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		// Bounded wait so a stuck pipe-drain goroutine can't hang the
		// test framework's cleanup phase.
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Logf("scheduler subprocess did not exit within 5s after group kill")
		}
	})

	// Wait for both agents to be listening. 30s is generous — claude
	// startup itself can take ~1-3s, and the agent has to register with
	// the scheduler heartbeat first.
	for _, p := range []int{teacherPort, studentPort} {
		if err := waitForPort(p, 30*time.Second); err != nil {
			t.Fatalf("agent on port %d not ready: %v\nscheduler log:\n%s", p, err, logBuf.String())
		}
	}

	return &e2eFixture{
		t:            t,
		repoRoot:     repoRoot,
		registryPort: registryPort,
		teacherPort:  teacherPort,
		studentPort:  studentPort,
		cmd:          cmd,
		cancel:       cancel,
		logBuf:       logBuf,
	}
}

// sendMessage POSTs an A2A message/send to the given agent port and returns
// the final agent reply text (last 'agent'-role entry in result.history).
// On JSON-RPC error, returns a descriptive error. The full request timeout
// is bounded so a stuck claude doesn't hang the test indefinitely.
func (f *e2eFixture) sendMessage(agentPort int, messageID, contextID, text string) (string, error) {
	f.t.Helper()
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"messageId": messageID,
				"contextId": contextID,
				"role":      "user",
				"parts":     []map[string]any{{"kind": "text", "text": text}},
			},
		},
		"id": 1,
	}
	body, _ := json.Marshal(payload)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	url := fmt.Sprintf("http://127.0.0.1:%d/", agentPort)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	var jr struct {
		Result struct {
			History []struct {
				Role  string `json:"role"`
				Parts []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"history"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return "", fmt.Errorf("parse JSON-RPC: %w; raw: %s", err, string(raw))
	}
	if jr.Error != nil {
		return "", fmt.Errorf("JSON-RPC error %d %s: %v", jr.Error.Code, jr.Error.Message, jr.Error.Data)
	}

	// The final agent reply is the last entry with role=agent.
	for i := len(jr.Result.History) - 1; i >= 0; i-- {
		msg := jr.Result.History[i]
		if msg.Role != "agent" {
			continue
		}
		for _, part := range msg.Parts {
			if part.Kind == "text" && part.Text != "" {
				return part.Text, nil
			}
		}
	}
	return "", fmt.Errorf("no agent text reply in history; raw: %s", string(raw))
}

// schedulerLog returns the cumulative stdout+stderr captured from the
// scheduler subprocess (which also tees agent subprocess output into its
// own streams). Useful for asserting log markers like
// "[student → teacher] A2A_CALL" to prove a delegation actually happened.
func (f *e2eFixture) schedulerLog() string { return f.logBuf.String() }

// --- helpers ---

// findRepoRoot walks up from this test file's directory looking for go.mod.
// The test file lives at example/multi-agent/e2e_test.go, so the root is
// two parents up — but we walk generically so refactors of the directory
// layout don't silently break the test.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — can't locate test source")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

// allocateFreePorts binds N sockets to :0 to get OS-assigned ports, then
// closes them and returns the numbers. The next process to listen on those
// ports may race with anyone else binding to them, but for a hermetic test
// this window is acceptable.
func allocateFreePorts(t *testing.T, n int) (int, int, int) {
	t.Helper()
	if n != 3 {
		t.Fatalf("allocateFreePorts: expected n=3, got %d (helper is hardcoded for the three-port layout: registry, teacher, student)", n)
	}
	ports := make([]int, 0, n)
	listeners := make([]net.Listener, 0, n)
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range listeners {
		_ = l.Close()
	}
	return ports[0], ports[1], ports[2]
}

// waitForPort polls a TCP connect until the agent's listener is up.
func waitForPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port %d not listening within %v", port, timeout)
}

func writeAgentCard(t *testing.T, workspace, body string) {
	t.Helper()
	dir := filepath.Join(workspace, ".a2a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-card.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write agent card: %v", err)
	}
}

// writeSchedulerConfig emits a minimal ahsir.yaml at path. teacherWS and
// studentWS are absolute paths to the per-agent workspace dirs (relative
// paths in the config would be interpreted relative to cmd.Dir, which is
// noisy across test runs). port_range is wide enough to allow the
// scheduler's port allocator some slack even though we pin to one start.
func writeSchedulerConfig(t *testing.T, path string, registryPort, agentPortStart int, teacherWS, studentWS string) {
	t.Helper()
	content := fmt.Sprintf(`agents:
  - name: teacher
    workspace: %s
    port: 0
  - name: student
    workspace: %s
    port: 0

registry:
  host: "127.0.0.1"
  port: %d
  heartbeat_interval: 10s
  heartbeat_timeout: 30s

mcp: {}

timeouts:
  chat: 10m
  task_status: 30s

port_range:
  start: %d
  end: %d
`, teacherWS, studentWS, registryPort, agentPortStart, agentPortStart+20)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write ahsir.yaml: %v", err)
	}
}

// teacherCardYAML is the minimal teacher card for e2e: no filesystem access,
// no delegation, just answer questions. Filesystem is deliberately disabled
// so the test doesn't require any particular host directories.
const teacherCardYAML = `name: teacher
description: e2e teacher agent
version: "1.0.0"
provider:
  name: ahsir
  url: https://github.com/wu8685/ahsir
skills:
  - name: teaching
    description: answer questions concisely
claude:
  systemPrompt: |
    You are a teacher. Answer the user's question in one concise sentence.
  maxAgentCalls: 0
runtime:
  command: claude
  args: []
  timeout: 300s
  provider: deepseek
  baseURL: https://api.deepseek.com/anthropic
  apiKey: "${MODEL_API_KEY}"
  model: deepseek-v4-pro
filesystem:
  enabled: false
network:
  bind: "127.0.0.1"
`

// studentCardYAML is the minimal student card for e2e: delegate every user
// question to the teacher via the A2A_CALL marker, then relay the answer.
// The prompt is deliberately blunt so DeepSeek-v4-pro doesn't shortcut
// the delegation (it tends to answer directly when the instruction is
// loose).
const studentCardYAML = `name: student
description: e2e student agent
version: "1.0.0"
provider:
  name: ahsir
  url: https://github.com/wu8685/ahsir
skills:
  - name: learning
    description: delegate questions to the teacher
claude:
  systemPrompt: |
    You are a student. For every user question, you MUST delegate to the
    teacher agent using exactly this format:

    ---A2A_CALL---
    {"agent": "teacher", "task": "<the user's question, verbatim>"}
    ---END---

    Then in your follow-up turn, relay the teacher's answer back to the user.
    Do not answer the question yourself.

    Available agents:
    - teacher: teaching
  maxAgentCalls: 3
runtime:
  command: claude
  args: []
  timeout: 300s
  provider: deepseek
  baseURL: https://api.deepseek.com/anthropic
  apiKey: "${MODEL_API_KEY}"
  model: deepseek-v4-pro
filesystem:
  enabled: false
network:
  bind: "127.0.0.1"
`

// --- tests ---

// TestStudentDelegatesToTeacher_E2E validates the headline multi-agent
// path with a real LLM: a curl-equivalent to the student must produce
// an A2A_CALL to the teacher, the teacher must answer, and the student
// must relay that answer back. Asserts both the response content AND
// the scheduler log markers — content alone isn't enough because the
// student model might happen to know the answer and bypass the teacher.
func TestStudentDelegatesToTeacher_E2E(t *testing.T) {
	fix := setupE2E(t)

	reply, err := fix.sendMessage(
		fix.studentPort,
		"msg-e2e-1",
		"e2e-conv-1",
		"What is a goroutine? Answer in one sentence.",
	)
	if err != nil {
		t.Fatalf("sendMessage to student: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	t.Logf("student reply: %q", reply)

	if !strings.Contains(strings.ToLower(reply), "goroutine") {
		t.Errorf("reply doesn't mention 'goroutine': %q", reply)
	}

	// Verify the delegation actually fired. Without these assertions, a
	// student that ignores the A2A_CALL instruction and answers directly
	// would still produce a passing reply — but the multi-agent path
	// wasn't really exercised.
	logs := fix.schedulerLog()
	for _, marker := range []string{
		"[student] receive",        // request reached student
		"[student → teacher]",      // student dispatched
		"[teacher] receive",        // teacher actually invoked
		"[student ← teacher] reply", // teacher response relayed
	} {
		if !strings.Contains(logs, marker) {
			t.Errorf("scheduler log missing marker %q — multi-agent path not exercised\n--- log ---\n%s", marker, logs)
		}
	}
}
