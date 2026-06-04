package wrapper

import (
	"context"
	"strings"
	"sync"
)

// OneshotSession is a fork-per-Turn Session implementation: each Turn
// invokes the configured sender once and no state persists across turns.
//
// Production wires ClaudeSession (long-running, stream-json) — OneshotSession
// is retained as a lightweight Session impl for tests that want to inject a
// fake sender function without spinning up the full protocol machinery, and
// as a reference implementation for future provider backends that don't
// support session continuity.
type OneshotSession struct {
	sender func(ctx context.Context, prompt string) (string, error)

	mu     sync.Mutex
	closed bool
}

// NewOneshotSession constructs a OneshotSession over an arbitrary sender
// function (e.g. SessionManager.Send for real fork-exec, or a mock in tests).
// Each Turn invokes sender once.
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

// IsHealthy returns false once Close has been called. OneshotSession has
// no long-lived underlying process to fail, so the only "unhealthy" state
// is the explicitly-closed one.
func (s *OneshotSession) IsHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// Close is a no-op (nothing to release) but tracks the closed flag so
// concurrent Close calls don't race on future state additions.
func (s *OneshotSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
