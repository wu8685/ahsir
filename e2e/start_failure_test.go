//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStartFailsFastOnPinnedPortConflict_E2E pins the startup-failure UX for
// an explicitly pinned port: when a foreign process already holds it,
// `ahsir start` must exit non-zero promptly with a readable error — not come
// up half-running with a silent dead agent.
func TestStartFailsFastOnPinnedPortConflict_E2E(t *testing.T) {
	requireClaudeE2E(t)
	repoRoot := findRepoRoot(t)
	ahsirBin := filepath.Join(repoRoot, "bin", "ahsir")
	if _, err := os.Stat(ahsirBin); err != nil {
		t.Skipf("missing %s — from repo root run: go build -o bin/ahsir ./cmd/ahsir", ahsirBin)
	}

	// Occupy a port with a listener owned by THIS test process — definitely
	// not a stale ahsir-agent, so eviction must refuse to touch it.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	blockedPort := blocker.Addr().(*net.TCPAddr).Port

	registryPort, _, _ := allocateFreePorts(t, 3)

	tmp := t.TempDir()
	ws := filepath.Join(tmp, "workspaces", "teacher")
	writeAgentCard(t, ws, teacherCardYAML)
	cfgPath := filepath.Join(tmp, "ahsir.yaml")
	cfg := fmt.Sprintf(`agents:
  - name: teacher
    workspace: %s
    port: %d

registry:
  host: "127.0.0.1"
  port: %d

port_range:
  start: %d
  end: %d
`, ws, blockedPort, registryPort, blockedPort+1, blockedPort+50)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ahsirBin, "start", cfgPath)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})

	if err == nil {
		t.Fatalf("ahsir start must exit non-zero when a pinned port is held by a foreign process\n--- output ---\n%s", out.String())
	}
	if !strings.Contains(out.String(), "already in use") {
		t.Errorf("startup error must tell the user the port is in use, got:\n%s", out.String())
	}
	// Rollback contract: the failed start must not leave the gateway
	// listening — `ahsir start` either fully runs or fully stops.
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", registryPort), 300*time.Millisecond); err == nil {
		conn.Close()
		t.Error("registry port still listening after failed start — abort rollback is broken")
	}
}

// TestStartSkipsOccupiedAutoPort_E2E pins the happy half of the same story:
// with AUTO port allocation (port: 0), a foreign listener inside the port
// range must be skipped transparently — the user's `ahsir start` succeeds
// and the agent lands on the next free port.
func TestStartSkipsOccupiedAutoPort_E2E(t *testing.T) {
	requireClaudeE2E(t)
	repoRoot := findRepoRoot(t)
	ahsirBin := filepath.Join(repoRoot, "bin", "ahsir")
	ahsirAgentBin := filepath.Join(repoRoot, "bin", "ahsir-agent")
	for _, b := range []string{ahsirBin, ahsirAgentBin} {
		if _, err := os.Stat(b); err != nil {
			t.Skipf("missing %s — from repo root run: go build -o bin/ahsir ./cmd/ahsir && go build -o bin/ahsir-agent ./cmd/ahsir-agent", b)
		}
	}

	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	blockedPort := blocker.Addr().(*net.TCPAddr).Port

	registryPort, _, _ := allocateFreePorts(t, 3)

	tmp := t.TempDir()
	ws := filepath.Join(tmp, "workspaces", "teacher")
	writeAgentCard(t, ws, teacherCardYAML)
	cfgPath := filepath.Join(tmp, "ahsir.yaml")
	// port: 0 → auto-allocate from a range that STARTS at the blocked port.
	cfg := fmt.Sprintf(`agents:
  - name: teacher
    workspace: %s
    port: 0

registry:
  host: "127.0.0.1"
  port: %d

port_range:
  start: %d
  end: %d
`, ws, registryPort, blockedPort, blockedPort+50)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, ahsirBin, "start", cfgPath)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logBuf := &syncBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start scheduler: %v", err)
	}
	t.Cleanup(func() {
		cleanupSchedulerProcess(cmd, cancel, []int{blockedPort + 1}, t.Logf)
	})

	// The teacher must come up registered + online despite the blocked
	// first candidate. Poll the public registry listing.
	deadline := time.Now().Add(30 * time.Second)
	online := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/agents", registryPort))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), `"teacher"`) {
				online = true
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !online {
		t.Fatalf("teacher never registered — auto port allocation didn't recover from the blocked port\n--- log ---\n%s", logBuf.String())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, fmt.Sprintf("skipping port %d", blockedPort)) {
		t.Errorf("expected 'skipping port %d' marker, got:\n%s", blockedPort, logs)
	}
	// The exact landing port is whatever the allocator found free next (it
	// may legitimately skip more than once — e.g. when the registry itself
	// occupies a port inside the range). Assert only that the teacher came
	// up on some port other than the blocked one.
	if !strings.Contains(logs, "Agent teacher started on port ") {
		t.Errorf("expected teacher start marker, got:\n%s", logs)
	}
	if strings.Contains(logs, fmt.Sprintf("started on port %d ", blockedPort)) {
		t.Errorf("teacher must not land on the blocked port %d:\n%s", blockedPort, logs)
	}
}
