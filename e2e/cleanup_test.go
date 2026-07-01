//go:build e2e

package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestCleanupSchedulerProcessReapsDetachedAgentGroup(t *testing.T) {
	port := freeE2ECleanupPort(t)
	debugPath := filepath.Join(t.TempDir(), "helper.log")
	cmd := exec.Command(os.Args[0],
		"-test.run=TestE2ECleanupHelperProcess",
	)
	cmd.Env = append(os.Environ(),
		"AHSIR_E2E_CLEANUP_HELPER=1",
		"AHSIR_E2E_CLEANUP_MODE=scheduler",
		"AHSIR_E2E_CLEANUP_PORT="+strconv.Itoa(port),
		"AHSIR_E2E_CLEANUP_DEBUG="+debugPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper scheduler: %v", err)
	}

	waitForCleanupPort(t, port, 2*time.Second, debugPath)

	cleanupSchedulerProcess(cmd, func() {}, []int{port}, t.Logf)

	deadline := time.Now().Add(2 * time.Second)
	for cleanupPortOpen(port) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if cleanupPortOpen(port) {
		t.Fatalf("cleanup left detached agent listener on port %d", port)
	}
}

func TestE2ECleanupHelperProcess(t *testing.T) {
	if os.Getenv("AHSIR_E2E_CLEANUP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("AHSIR_E2E_CLEANUP_MODE")
	port := os.Getenv("AHSIR_E2E_CLEANUP_PORT")
	cleanupDebugf("mode=%s port=%s", mode, port)
	switch mode {
	case "scheduler":
		if port == "" {
			os.Exit(2)
		}
		agent := exec.Command(os.Args[0],
			"-test.run=TestE2ECleanupHelperProcess",
		)
		agent.Env = append(os.Environ(),
			"AHSIR_E2E_CLEANUP_HELPER=1",
			"AHSIR_E2E_CLEANUP_MODE=agent",
			"AHSIR_E2E_CLEANUP_PORT="+port,
			"AHSIR_E2E_CLEANUP_DEBUG="+os.Getenv("AHSIR_E2E_CLEANUP_DEBUG"),
		)
		agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := agent.Start(); err != nil {
			cleanupDebugf("agent start err=%v", err)
			os.Exit(2)
		}
		cleanupDebugf("agent started pid=%d", agent.Process.Pid)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM)
		<-sigCh
		// Intentionally exit without killing the detached agent. The fixture
		// cleanup must reap this separate process group by discovering the
		// listener on the generated agent port.
		os.Exit(0)
	case "agent":
		if port == "" {
			os.Exit(2)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			cleanupDebugf("agent listen err=%v", err)
			os.Exit(2)
		}
		cleanupDebugf("agent listening port=%s", port)
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				cleanupDebugf("agent accept err=%v", err)
				os.Exit(2)
			}
			_ = conn.Close()
		}
	default:
		os.Exit(2)
	}
}

func freeE2ECleanupPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForCleanupPort(t *testing.T, port int, timeout time.Duration, debugPath string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cleanupPortOpen(port) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	debug, _ := os.ReadFile(debugPath)
	t.Fatalf("timeout waiting for helper listener on port %d\nhelper log:\n%s", port, debug)
}

func cleanupPortOpen(port int) bool {
	return len(listenerPIDs(port)) > 0
}

func cleanupDebugf(format string, args ...any) {
	path := os.Getenv("AHSIR_E2E_CLEANUP_DEBUG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, format+"\n", args...)
}
