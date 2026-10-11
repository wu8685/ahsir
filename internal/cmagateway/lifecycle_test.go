package cmagateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/cmagateway/ahsir"
	"github.com/wu8685/ahsir/internal/cmagateway/cma"
)

// Lifecycle DataParts emitted by the agent runtime mid-turn surface as typed
// session.lifecycle CMA events (issue #43).
func TestExecuteTurn_LifecycleEventsRelayed(t *testing.T) {
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/agents":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/a2a/"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// One lifecycle status-update (soft deadline) followed by the
			// terminal task carrying the finalize summary.
			_, _ = w.Write([]byte(`data: {"result":{"kind":"status-update","taskId":"task-1","final":false,"status":{"state":"working","message":{"parts":[{"kind":"data","data":{"ev":"lifecycle","event":"turn.soft_deadline","turn_id":"turn_abc","phase":"running"}}]}}}}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"result":{"kind":"status-update","taskId":"task-1","final":false,"status":{"state":"working","message":{"parts":[{"kind":"data","data":{"ev":"lifecycle","event":"turn.outcome","turn_id":"turn_abc","phase":"idle","kind":"runtime_timeout","trigger":"soft_deadline"}}]}}}}` + "\n\n"))
			writeCompletedStream(w, "final summary")
		default:
			http.NotFound(w, r)
		}
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, false)
	s := New(Config{TurnTimeout: 5 * time.Second}, st, ahsir.New(scheduler.URL, ""))
	rr := sendUserMessage(s, rec.Session.ID, "work")
	if rr.Code != http.StatusOK {
		t.Fatalf("sendEvents status = %d; body=%s", rr.Code, rr.Body.String())
	}
	waitForSessionStatus(t, rec, cma.StatusIdle)

	var lifecycle []cma.Event
	for _, ev := range rec.Snapshot() {
		if ev.Type == cma.EvtSessionLifecycle {
			lifecycle = append(lifecycle, ev)
		}
	}
	if len(lifecycle) != 2 {
		t.Fatalf("session.lifecycle events = %d, want 2; all=%+v", len(lifecycle), rec.Snapshot())
	}
	if lifecycle[0].Lifecycle == nil || lifecycle[0].Lifecycle.Event != "turn.soft_deadline" || lifecycle[0].Lifecycle.TurnID != "turn_abc" {
		t.Fatalf("first lifecycle event wrong: %+v", lifecycle[0].Lifecycle)
	}
	terminal := lifecycle[1].Lifecycle
	if terminal == nil || terminal.Event != "turn.outcome" || terminal.Kind != cma.LifecycleKindRuntimeTimeout || terminal.Trigger != "soft_deadline" {
		t.Fatalf("terminal lifecycle outcome wrong: %+v", terminal)
	}
}

// When the facade's own turn timeout wins the race, the session records a
// typed facade_timeout outcome — clients never parse session.error strings.
func TestExecuteTurn_FacadeTimeoutTypedOutcome(t *testing.T) {
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/agents":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/a2a/"):
			// Hang past the facade turn timeout without writing anything. The
			// fallback timer keeps httptest.Server.Close from blocking on a
			// connection the client already abandoned.
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, false)
	s := New(Config{TurnTimeout: 100 * time.Millisecond}, st, ahsir.New(scheduler.URL, ""))
	rr := sendUserMessage(s, rec.Session.ID, "work")
	if rr.Code != http.StatusOK {
		t.Fatalf("sendEvents status = %d; body=%s", rr.Code, rr.Body.String())
	}
	waitForSessionStatus(t, rec, cma.StatusTerminated)

	var found *cma.LifecycleInfo
	var sawError bool
	for _, ev := range rec.Snapshot() {
		if ev.Type == cma.EvtSessionLifecycle && ev.Lifecycle != nil && ev.Lifecycle.Event == "turn.outcome" {
			found = ev.Lifecycle
		}
		if ev.Type == cma.EvtSessionError {
			sawError = true
		}
	}
	if found == nil || found.Kind != cma.LifecycleKindFacadeTimeout {
		t.Fatalf("typed facade_timeout outcome missing: %+v; events=%+v", found, rec.Snapshot())
	}
	if !sawError {
		t.Fatal("legacy session.error must still be emitted for compatibility")
	}
}
