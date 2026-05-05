package wrapper

import (
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
	// `cat` echoes stdin to stdout — perfect for verifying that Send wires
	// the prompt into the child's stdin (the structural defense against
	// flag-eats-prompt bugs). Don't switch this back to `echo`: echo doesn't
	// read stdin, so the test would silently pass with empty output.
	sm := NewSessionManager(SessionConfig{
		Command: "cat",
		Timeout: 2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sm.Stop()

	prompt := "hello claude"
	out, err := sm.Send(ctx, prompt)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "hello claude") {
		t.Errorf("expected output to contain 'hello claude', got: %s", out)
	}
}

// TestSessionManagerSurfacesNonZeroExit verifies that a CLI exiting non-zero
// produces an error containing the captured stderr. This is the regression
// test for the original "empty agent reply" bug: without stderr capture and
// exit-code checking, the wrapper used to swallow CLI errors and return ""
// as a successful response.
func TestSessionManagerSurfacesNonZeroExit(t *testing.T) {
	sm := NewSessionManager(SessionConfig{
		Command: "sh",
		Args:    []string{"-c", "echo boom-on-stderr >&2; exit 7"},
		Timeout: 2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sm.Stop()

	_, err := sm.Send(ctx, "anything")
	if err == nil {
		t.Fatal("expected non-zero exit to surface as error, got nil")
	}
	if !strings.Contains(err.Error(), "boom-on-stderr") {
		t.Errorf("expected error to include captured stderr, got: %v", err)
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
