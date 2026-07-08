package wrapper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
)

// EchoSession is a deterministic, in-memory Session used for tests and the
// CMA-facade e2e. It runs no subprocess and makes no network calls: every turn
// replies with the user's text echoed back, so a turn is fully reproducible
// without a real LLM. Selected via runtime.provider = "echo" (ProviderEcho).
//
// It implements the full Session interface plus OnSessionIDKnown (the pool's
// optional sessionIDNotifier), so it drops into SessionPool exactly like a
// ClaudeSession — the CMA facade → scheduler → agent path is exercised end to
// end; only the LLM call is replaced.
type EchoSession struct {
	mu               sync.Mutex
	sessionID        string
	closed           bool
	onSessionIDKnown func(string)
}

// NewEchoSession constructs an echo-backed Session. A non-empty resumeID is
// adopted as the session id (so pool resume is a faithful round-trip);
// otherwise a fresh opaque id is generated. cfg is unused — echo needs no
// command, env, or workdir.
func NewEchoSession(_ context.Context, _ SessionConfig, resumeID string) (*EchoSession, error) {
	sid := resumeID
	if sid == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		sid = "echo-" + hex.EncodeToString(b[:])
	}
	return &EchoSession{sessionID: sid}, nil
}

// Stream emits one EventText (the echoed reply) followed by EventTurnDone, then
// closes the channel — the same event contract ClaudeSession honours. The reply
// contains userText verbatim so callers/tests can assert round-trip fidelity.
func (s *EchoSession) Stream(ctx context.Context, userText string) (<-chan Event, error) {
	ch := make(chan Event, 2)
	go func() {
		defer close(ch)
		if err := ctx.Err(); err != nil {
			ch <- EventTurnDone{Err: err}
			return
		}
		ch <- EventText{Text: "echo: " + userText}
		ch <- EventTurnDone{}
	}()
	return ch, nil
}

// Turn drives Stream and returns the aggregated assistant text (mirrors
// ClaudeSession.Turn).
func (s *EchoSession) Turn(ctx context.Context, userText string) (string, error) {
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

func (s *EchoSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *EchoSession) IsHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// Close marks the session unhealthy. Idempotent; there is nothing to release.
func (s *EchoSession) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

// OnSessionIDKnown satisfies the pool's optional sessionIDNotifier. The id is
// known at construction, so the callback fires immediately.
func (s *EchoSession) OnSessionIDKnown(fn func(string)) {
	s.mu.Lock()
	sid := s.sessionID
	s.onSessionIDKnown = fn
	s.mu.Unlock()
	if sid != "" && fn != nil {
		fn(sid)
	}
}
