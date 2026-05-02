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
	// Use echo to test: echo outputs its arguments
	sm := NewSessionManager(SessionConfig{
		Command: "echo",
		Timeout: 2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sm.Stop()

	// Send message and read response — prompt is passed as last CLI arg
	prompt := "hello claude"
	outputCh, err := sm.Send(ctx, prompt)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	for line := range outputCh {
		buf.WriteString(line)
	}

	if !strings.Contains(buf.String(), "hello claude") {
		t.Errorf("expected output to contain 'hello claude', got: %s", buf.String())
	}
}

func TestSessionManagerCrashRecovery(t *testing.T) {
	sm := NewSessionManager(SessionConfig{
		Command: "echo",
		Args:    []string{"test"},
	})
	ctx := context.Background()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

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
