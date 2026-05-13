package wrapper

import (
	"context"
	"errors"
	"testing"
	"time"
)

// OneshotSession contract: each Turn forks a fresh agent invocation. The
// underlying sender is the seam we mock; production code wires it to
// SessionManager.Send.

func TestOneshotSession_Turn_HappyPath(t *testing.T) {
	sender := func(ctx context.Context, prompt string) (string, error) {
		return "hello", nil
	}
	s := &OneshotSession{sender: sender}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := s.Turn(ctx, "ping")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello" {
		t.Errorf("expected 'hello', got %q", out)
	}
}

func TestOneshotSession_Turn_Error(t *testing.T) {
	wantErr := errors.New("boom")
	sender := func(ctx context.Context, prompt string) (string, error) {
		return "", wantErr
	}
	s := &OneshotSession{sender: sender}
	defer s.Close()

	out, err := s.Turn(context.Background(), "ping")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr, got %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output on error, got %q", out)
	}
}

func TestOneshotSession_Stream_EmitsAndCloses(t *testing.T) {
	sender := func(ctx context.Context, prompt string) (string, error) {
		return "hi there", nil
	}
	s := &OneshotSession{sender: sender}
	defer s.Close()

	ch, err := s.Stream(context.Background(), "ping")
	if err != nil {
		t.Fatal(err)
	}

	var events []Event
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (text + turn-done), got %d: %+v", len(events), events)
	}
	tx, ok := events[0].(EventText)
	if !ok {
		t.Fatalf("expected EventText first, got %T", events[0])
	}
	if tx.Text != "hi there" {
		t.Errorf("expected text 'hi there', got %q", tx.Text)
	}
	done, ok := events[1].(EventTurnDone)
	if !ok {
		t.Fatalf("expected EventTurnDone second, got %T", events[1])
	}
	if done.Err != nil {
		t.Errorf("expected nil Err on success, got %v", done.Err)
	}
}

// TestOneshotSession_Stream_PropagatesError verifies that a sender error is
// delivered via EventTurnDone.Err and no EventText is emitted for empty output.
func TestOneshotSession_Stream_PropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	sender := func(ctx context.Context, prompt string) (string, error) {
		return "", wantErr
	}
	s := &OneshotSession{sender: sender}
	defer s.Close()

	ch, err := s.Stream(context.Background(), "ping")
	if err != nil {
		t.Fatal(err)
	}

	var events []Event
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event (turn-done only), got %d: %+v", len(events), events)
	}
	done, ok := events[0].(EventTurnDone)
	if !ok {
		t.Fatalf("expected EventTurnDone, got %T", events[0])
	}
	if !errors.Is(done.Err, wantErr) {
		t.Errorf("expected wantErr wrapped in EventTurnDone.Err, got %v", done.Err)
	}
}

func TestOneshotSession_SessionID_Empty(t *testing.T) {
	s := &OneshotSession{sender: func(context.Context, string) (string, error) { return "", nil }}
	if id := s.SessionID(); id != "" {
		t.Errorf("expected empty sessionID for oneshot, got %q", id)
	}
}

// Close is a no-op for OneshotSession (nothing to release) but must be
// idempotent so callers can defer it safely.
func TestOneshotSession_Close_Idempotent(t *testing.T) {
	s := &OneshotSession{sender: func(context.Context, string) (string, error) { return "", nil }}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
