package wrapper

import (
	"context"
	"strings"
	"sync"
)

// OneshotSession adapts the legacy per-request fork-and-exec model
// (SessionManager.Send) to the Session interface. Each Turn invokes the
// sender once; no state persists across turns.
//
// Purpose: keeps existing wrapper behavior unchanged while the Session
// abstraction is plumbed through Executor. Step 2 introduces ClaudeSession
// (stream-json, long-running) as the real provider; OneshotSession remains
// as a fallback / test double.
type OneshotSession struct {
	// sender performs one round-trip with the underlying runtime. Production
	// code wires this to SessionManager.Send; tests inject a fake.
	sender func(ctx context.Context, prompt string) (string, error)

	mu     sync.Mutex
	closed bool
}

// NewOneshotSession constructs a OneshotSession over an arbitrary sender
// function. Production code typically passes SessionManager.Send; tests and
// alternative backends pass their own. Each Turn invokes sender once.
func NewOneshotSession(sender func(ctx context.Context, prompt string) (string, error)) *OneshotSession {
	return &OneshotSession{sender: sender}
}

// Stream invokes the sender in a goroutine and feeds its result through the
// returned channel as a single EventText (when output is non-empty) followed
// by EventTurnDone. The channel is closed after EventTurnDone.
func (s *OneshotSession) Stream(ctx context.Context, userText string) (<-chan Event, error) {
	ch := make(chan Event, 2)
	go func() {
		defer close(ch)
		out, err := s.sender(ctx, userText)
		if out != "" {
			ch <- EventText{Text: out}
		}
		ch <- EventTurnDone{Err: err}
	}()
	return ch, nil
}

// Turn drains Stream and aggregates EventText into a single string. The
// returned error reflects EventTurnDone.Err — partial text accumulated
// before the error is preserved in the returned string.
func (s *OneshotSession) Turn(ctx context.Context, userText string) (string, error) {
	ch, err := s.Stream(ctx, userText)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	var doneErr error
	for ev := range ch {
		switch e := ev.(type) {
		case EventText:
			sb.WriteString(e.Text)
		case EventTurnDone:
			doneErr = e.Err
		}
	}
	return sb.String(), doneErr
}

// SessionID returns "" — oneshot mode has no persistent runtime session.
// ClaudeSession (Step 2) is where session_id comes from.
func (s *OneshotSession) SessionID() string { return "" }

// Close is a no-op (nothing to release) but tracks the closed flag so
// concurrent Close calls don't race on future state additions.
func (s *OneshotSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
