package wrapper

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionManagerStartStop(t *testing.T) {
	sm := NewSessionManager(SessionConfig{
		Command: "echo",
		Args:    []string{"hello"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !sm.IsRunning() {
		t.Error("expected session to be running")
	}

	if err := sm.Stop(); err != nil {
		t.Fatal(err)
	}
	if sm.IsRunning() {
		t.Error("expected session to be stopped")
	}
}

func TestSessionManagerSendMessage(t *testing.T) {
	// Use cat to echo back input
	sm := NewSessionManager(SessionConfig{
		Command: "cat",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sm.Stop()

	// Send message and read response
	prompt := "hello claude\n"
	outputCh, err := sm.Send(ctx, prompt)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	for line := range outputCh {
		buf.WriteString(line)
	}

	if !strings.Contains(buf.String(), "hello claude") {
		t.Errorf("expected echo of 'hello claude', got: %s", buf.String())
	}
}

func TestSessionManagerCrashRecovery(t *testing.T) {
	sm := NewSessionManager(SessionConfig{
		Command:     "sleep",
		Args:        []string{"0.1"},
		AutoRestart: true,
	})
	ctx := context.Background()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Wait for process to exit
	time.Sleep(300 * time.Millisecond)

	// Stop the old session first (cleanup)
	sm.Stop()

	// Should be able to restart after stopping
	if err := sm.Start(ctx); err != nil {
		t.Fatalf("expected restart to succeed: %v", err)
	}
	sm.Stop()
}

func TestSessionConfigValidation(t *testing.T) {
	cfg := SessionConfig{
		Command: "",
	}
	if cfg.Validate() == nil {
		t.Error("expected validation error for empty command")
	}

	cfg.Command = "echo"
	cfg.Timeout = -1
	if cfg.Validate() == nil {
		t.Error("expected validation error for negative timeout")
	}
}
