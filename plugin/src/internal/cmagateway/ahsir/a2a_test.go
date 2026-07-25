package ahsir

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// openStreamBackoff must grow linearly early (so a port still binding after a
// normal restart is retried within a fraction of a second) and cap at
// openStreamMaxBackoff (so the window stretches to cover a cold start without
// the late-attempt waits ballooning).
func TestOpenStreamBackoff_GrowsThenCaps(t *testing.T) {
	if got := openStreamBackoff(1); got != openStreamBaseBackoff {
		t.Errorf("backoff(1) = %v, want %v", got, openStreamBaseBackoff)
	}
	if got := openStreamBackoff(3); got != 3*openStreamBaseBackoff {
		t.Errorf("backoff(3) = %v, want %v", got, 3*openStreamBaseBackoff)
	}
	// Past the cap threshold the wait is clamped, never larger.
	if got := openStreamBackoff(1000); got != openStreamMaxBackoff {
		t.Errorf("backoff(1000) = %v, want cap %v", got, openStreamMaxBackoff)
	}
	// The exact attempt where the linear value first exceeds the cap must clamp.
	overCap := int(openStreamMaxBackoff/openStreamBaseBackoff) + 1
	if got := openStreamBackoff(overCap); got != openStreamMaxBackoff {
		t.Errorf("backoff(%d) = %v, want cap %v", overCap, got, openStreamMaxBackoff)
	}
}

// Regression guard for issue #17: the total wake-retry window must comfortably
// outlast a real cold start (30–90s). If someone shrinks maxAttempts or the
// backoff cap back toward the old ~13.5s window, this fails.
func TestOpenStreamWindow_CoversColdStart(t *testing.T) {
	var total time.Duration
	for attempt := 1; attempt < openStreamMaxAttempts; attempt++ {
		total += openStreamBackoff(attempt)
	}
	if total < 90*time.Second {
		t.Fatalf("total wake window = %v, want >= 90s to cover a cold agent start", total)
	}
}

// openStream must retry transient 502s (agent still cold-starting / port
// rebinding) and succeed once the agent's stream is up, invoking onReschedule
// exactly once on the first retry.
func TestOpenStream_RetriesThenSucceeds(t *testing.T) {
	restore := shrinkOpenStreamWindow(t)
	defer restore()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 { // fail the first two attempts like a proxy that can't reach the agent yet
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"connection refused"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer srv.Close()

	var reschedules int32
	c := New(srv.URL, "")
	resp, err := c.openStream(context.Background(), "cma-x-v1", []byte(`{}`), func() {
		atomic.AddInt32(&reschedules, 1)
	})
	if err != nil {
		t.Fatalf("openStream: %v", err)
	}
	defer resp.Body.Close()

	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("server calls = %d, want 3 (two 502s then success)", got)
	}
	if got := atomic.LoadInt32(&reschedules); got != 1 {
		t.Errorf("onReschedule calls = %d, want exactly 1", got)
	}
}

// After exhausting every attempt against an agent that never comes up,
// openStream must return a terminal error naming the attempt count and the last
// underlying failure.
func TestOpenStream_ExhaustsAttempts(t *testing.T) {
	restore := shrinkOpenStreamWindow(t)
	defer restore()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.openStream(context.Background(), "cma-x-v1", []byte(`{}`), nil)
	if err == nil {
		t.Fatal("expected terminal error after exhausting attempts, got nil")
	}
	if got := atomic.LoadInt32(&calls); int(got) != openStreamMaxAttempts {
		t.Errorf("server calls = %d, want %d (one per attempt)", got, openStreamMaxAttempts)
	}
}

// A non-retryable status (>=300 that isn't 502) must fail immediately without
// consuming the retry budget.
func TestOpenStream_TerminalStatusNoRetry(t *testing.T) {
	restore := shrinkOpenStreamWindow(t)
	defer restore()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if _, err := c.openStream(context.Background(), "cma-x-v1", []byte(`{}`), nil); err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (404 is terminal, no retry)", got)
	}
}

// A context canceled mid-backoff must abort the retry loop promptly with the
// context's error rather than waiting out the window.
func TestOpenStream_ContextCancelDuringBackoff(t *testing.T) {
	// Backoff must be long enough that the deadline lands mid-wait, not after
	// the loop has already exhausted its attempts.
	origAttempts, origBase, origMax := openStreamMaxAttempts, openStreamBaseBackoff, openStreamMaxBackoff
	openStreamMaxAttempts, openStreamBaseBackoff, openStreamMaxBackoff = 40, 100*time.Millisecond, 3*time.Second
	defer func() {
		openStreamMaxAttempts, openStreamBaseBackoff, openStreamMaxBackoff = origAttempts, origBase, origMax
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway) // always cold -> forces a backoff wait
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	c := New(srv.URL, "")
	_, err := c.openStream(ctx, "cma-x-v1", []byte(`{}`), nil)
	if err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// shrinkOpenStreamWindow shrinks the retry knobs so tests exercise the full
// loop without waiting the real ~100s window, restoring them afterward.
func shrinkOpenStreamWindow(t *testing.T) func() {
	t.Helper()
	origAttempts, origBase, origMax := openStreamMaxAttempts, openStreamBaseBackoff, openStreamMaxBackoff
	openStreamMaxAttempts = 4
	openStreamBaseBackoff = time.Millisecond
	openStreamMaxBackoff = 2 * time.Millisecond
	return func() {
		openStreamMaxAttempts, openStreamBaseBackoff, openStreamMaxBackoff = origAttempts, origBase, origMax
	}
}
