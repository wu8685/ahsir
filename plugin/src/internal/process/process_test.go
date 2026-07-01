package process

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPrepareCommandAssignsNewProcessGroup(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sleep", "10")

	PrepareCommand(cmd)

	if !CommandStartsProcessGroup(cmd) {
		t.Fatalf("expected command to start in a new process group")
	}
}

func TestHasExplicitEnvDetectsKey(t *testing.T) {
	env := []string{"PATH=/bin", "CODEX_HOME=/tmp/codex-home"}

	if !HasExplicitEnv(env, "CODEX_HOME") {
		t.Fatalf("expected CODEX_HOME to be detected")
	}
	if HasExplicitEnv(env, "HOME") {
		t.Fatalf("did not expect HOME to be detected")
	}
}

func TestKillTreeStopsChildProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 1000 & echo $! > '"+pidFile+"'; wait")
	PrepareCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}

	childPID := waitForPIDFile(t, pidFile)
	if !processExists(childPID) {
		t.Fatalf("child pid %d does not exist before kill", childPID)
	}

	if err := KillTree(cmd.Process); err != nil {
		t.Fatalf("KillTree: %v", err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for processExists(childPID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(childPID) {
		t.Fatalf("child pid %d still exists after KillTree", childPID)
	}
}

func TestKillTreeStopsChildProcessWithoutOwnedProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 1000 & echo $! > '"+pidFile+"'; wait")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}

	childPID := waitForPIDFile(t, pidFile)
	if !processExists(childPID) {
		t.Fatalf("child pid %d does not exist before kill", childPID)
	}
	defer killProcess(childPID)

	if err := KillTree(cmd.Process); err != nil {
		t.Fatalf("KillTree: %v", err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for processExists(childPID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(childPID) {
		t.Fatalf("child pid %d still exists after KillTree without owned process group", childPID)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
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
	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run()
	return err == nil
}

func killProcess(pid int) {
	if pid > 0 {
		_ = exec.Command("kill", "-KILL", strconv.Itoa(pid)).Run()
	}
}
