package wrapper

import (
	"context"
	"strings"
	"testing"
)

func TestEchoSession(t *testing.T) {
	ctx := context.Background()
	s, err := NewEchoSession(ctx, SessionConfig{}, "")
	if err != nil {
		t.Fatalf("NewEchoSession: %v", err)
	}
	if s.SessionID() == "" {
		t.Fatal("SessionID() empty; want a stable non-empty id")
	}
	if !s.IsHealthy() {
		t.Fatal("IsHealthy() false before Close()")
	}

	// Turn echoes the input verbatim.
	got, err := s.Turn(ctx, "hello agent")
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if !strings.Contains(got, "hello agent") {
		t.Fatalf("Turn reply %q does not contain the prompt", got)
	}

	// Stream yields exactly one EventText (containing the input) then a
	// terminal EventTurnDone, then closes.
	ch, err := s.Stream(ctx, "ping xyz")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var texts []string
	var sawDone bool
	for ev := range ch {
		switch e := ev.(type) {
		case EventText:
			if sawDone {
				t.Fatal("EventText delivered after EventTurnDone")
			}
			texts = append(texts, e.Text)
		case EventTurnDone:
			if e.Err != nil {
				t.Fatalf("EventTurnDone.Err = %v; want nil", e.Err)
			}
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("channel closed without EventTurnDone")
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "ping xyz") {
		t.Fatalf("EventText stream = %v; want one part containing the prompt", texts)
	}

	// Close is idempotent and flips health.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if s.IsHealthy() {
		t.Fatal("IsHealthy() true after Close()")
	}
}

// TestEchoSessionCancelledContext: a pre-cancelled context yields a done event
// carrying the context error and no text.
func TestEchoSessionCancelledContext(t *testing.T) {
	s, _ := NewEchoSession(context.Background(), SessionConfig{}, "resume-123")
	if s.SessionID() != "resume-123" {
		t.Fatalf("SessionID() = %q; want the adopted resumeID", s.SessionID())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch, err := s.Stream(ctx, "anything")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for ev := range ch {
		if d, ok := ev.(EventTurnDone); ok && d.Err == nil {
			t.Fatal("cancelled turn reported no error")
		}
	}
}
