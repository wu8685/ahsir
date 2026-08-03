package cmagateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/cmagateway/ahsir"
	"github.com/wu8685/ahsir/internal/cmagateway/cma"
	"github.com/wu8685/ahsir/internal/cmagateway/store"
	"github.com/wu8685/ahsir/internal/cmagateway/translate"
)

// A persisted CMA session is still desired state after the facade and
// scheduler restart. Dispatch must restore its missing scheduler runtime
// before the first A2A request, rather than consuming the event and terminating
// the session with "agent not found".
func TestSendEvents_PreflightsPersistedAgentBeforeAcceptingUserMessage(t *testing.T) {
	registerStarted := make(chan struct{})
	releaseRegister := make(chan struct{})
	var registerCalls, streamCalls atomic.Int32
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/agents":
			registerCalls.Add(1)
			close(registerStarted)
			<-releaseRegister
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/a2a/"):
			streamCalls.Add(1)
			writeCompletedStream(w, "recovered")
		default:
			http.NotFound(w, r)
		}
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, false)
	s := New(Config{TurnTimeout: time.Second}, st, ahsir.New(scheduler.URL, ""))
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- sendUserMessage(s, rec.Session.ID, "continue") }()

	select {
	case <-registerStarted:
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("sendEvents did not synchronously reconcile; events=%+v", rec.Snapshot())
	}
	if events := rec.Snapshot(); len(events) != 0 {
		t.Fatalf("events appended before reconciliation completed: %+v", events)
	}
	select {
	case rr := <-response:
		t.Fatalf("sendEvents returned %d before reconciliation completed", rr.Code)
	default:
	}
	close(releaseRegister)
	rr := <-response
	if rr.Code != http.StatusOK {
		t.Fatalf("sendEvents status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	waitForSessionStatus(t, rec, cma.StatusIdle)

	if got := registerCalls.Load(); got != 1 {
		t.Fatalf("scheduler registrations = %d, want 1 before dispatch", got)
	}
	if got := streamCalls.Load(); got != 1 {
		t.Fatalf("A2A dispatches = %d, want 1", got)
	}
	if rec.Status() != cma.StatusIdle {
		t.Fatalf("session status = %q, want idle; events=%+v", rec.Status(), rec.Snapshot())
	}
}

// Even after a successful pre-dispatch reconciliation, scheduler state can
// disappear in the race before the A2A request. A pre-stream 404 is safe to
// recover, but the recovery must be bounded to one reconcile plus one retry.
func TestExecuteTurn_AgentNotFoundReconcilesAndRetriesOnce(t *testing.T) {
	var registerCalls, streamCalls atomic.Int32
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/agents":
			registerCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/a2a/"):
			if streamCalls.Add(1) == 1 {
				w.Header().Set("X-Ahsir-Error-Code", "scheduler_agent_not_found")
				http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
				return
			}
			writeCompletedStream(w, "retried")
		default:
			http.NotFound(w, r)
		}
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, false)
	s := New(Config{TurnTimeout: time.Second}, st, ahsir.New(scheduler.URL, ""))
	rr := sendUserMessage(s, rec.Session.ID, "continue")
	if rr.Code != http.StatusOK {
		t.Fatalf("sendEvents status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	waitForSessionStatus(t, rec, cma.StatusIdle)

	if got := registerCalls.Load(); got != 2 {
		t.Fatalf("scheduler registrations = %d, want initial reconcile + one 404 reconcile", got)
	}
	if got := streamCalls.Load(); got != 2 {
		t.Fatalf("A2A dispatches = %d, want initial attempt + one retry", got)
	}
	if rec.Status() != cma.StatusIdle {
		t.Fatalf("session status = %q, want idle; events=%+v", rec.Status(), rec.Snapshot())
	}
}

func TestExecuteTurn_AgentNotFoundRetryIsBounded(t *testing.T) {
	var registerCalls, streamCalls atomic.Int32
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/agents":
			registerCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/a2a/"):
			streamCalls.Add(1)
			w.Header().Set("X-Ahsir-Error-Code", "scheduler_agent_not_found")
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, false)
	s := New(Config{TurnTimeout: time.Second}, st, ahsir.New(scheduler.URL, ""))
	rr := sendUserMessage(s, rec.Session.ID, "continue")
	if rr.Code != http.StatusOK {
		t.Fatalf("sendEvents status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	waitForSessionStatus(t, rec, cma.StatusTerminated)

	if got := registerCalls.Load(); got != 2 {
		t.Fatalf("scheduler registrations = %d, want exactly 2", got)
	}
	if got := streamCalls.Load(); got != 2 {
		t.Fatalf("A2A dispatches = %d, want exactly 2", got)
	}
	if rec.Status() != cma.StatusTerminated {
		t.Fatalf("session status = %q, want terminated", rec.Status())
	}
	assertStructuredTerminalError(t, rec, "agent_persisted", "version=4", rec.AhsirName, "reconciliation=succeeded")
}

// An existing session pins an agent version, but archiving that CMA agent
// revokes it as desired state. A later event on the persisted session must not
// recreate or invoke the scheduler runtime.
func TestExecuteTurn_ArchivedPersistedAgentIsNotResurrected(t *testing.T) {
	var schedulerCalls atomic.Int32
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		schedulerCalls.Add(1)
		http.Error(w, "unexpected scheduler call", http.StatusInternalServerError)
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, true)
	s := New(Config{TurnTimeout: time.Second}, st, ahsir.New(scheduler.URL, ""))
	rr := sendUserMessage(s, rec.Session.ID, "must not run")

	if rr.Code < 400 {
		t.Fatalf("sendEvents status = %d, want rejection for archived agent; body=%s", rr.Code, rr.Body.String())
	}
	if got := schedulerCalls.Load(); got != 0 {
		t.Fatalf("scheduler calls = %d, want 0 for archived agent", got)
	}
	if events := rec.Snapshot(); len(events) != 0 {
		t.Fatalf("archived rejection appended events: %+v", events)
	}
}

// The synchronous preflight and queued turn are separated by a scheduling
// boundary. Re-check the Store in executeTurn so an archive in that window
// cannot resurrect or invoke the runtime.
func TestExecuteTurn_RechecksArchivedAgentBeforeDispatch(t *testing.T) {
	var schedulerCalls atomic.Int32
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		schedulerCalls.Add(1)
		http.Error(w, "unexpected scheduler call", http.StatusInternalServerError)
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, false)
	if _, ok := st.ArchiveAgent(rec.Session.Agent.ID, time.Now().UTC()); !ok {
		t.Fatal("archive persisted agent: not found")
	}
	s := New(Config{TurnTimeout: time.Second}, st, ahsir.New(scheduler.URL, ""))
	s.executeTurn(rec, "must not run")

	if got := schedulerCalls.Load(); got != 0 {
		t.Fatalf("scheduler calls = %d, want 0 after archive race", got)
	}
	if rec.Status() != cma.StatusTerminated {
		t.Fatalf("session status = %q, want terminated", rec.Status())
	}
	assertStructuredTerminalError(t, rec, "agent_persisted", "version=4", rec.AhsirName, "reconciliation=skipped")
}

func TestSendEvents_ReconcileFailureDoesNotAppendOrEnqueue(t *testing.T) {
	var streamCalls atomic.Int32
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/agents" {
			http.Error(w, `{"error":"no available runtime"}`, http.StatusInternalServerError)
			return
		}
		streamCalls.Add(1)
		http.Error(w, "unexpected dispatch", http.StatusInternalServerError)
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, false)
	s := New(Config{TurnTimeout: time.Second}, st, ahsir.New(scheduler.URL, ""))
	rr := sendUserMessage(s, rec.Session.ID, "must not be accepted")

	if rr.Code < 400 {
		t.Fatalf("sendEvents status = %d, want synchronous reconciliation failure; body=%s", rr.Code, rr.Body.String())
	}
	if got := streamCalls.Load(); got != 0 {
		t.Fatalf("A2A dispatches = %d, want 0 after failed preflight", got)
	}
	if events := rec.Snapshot(); len(events) != 0 {
		t.Fatalf("failed preflight appended events: %+v", events)
	}
	if rec.Status() != cma.StatusIdle {
		t.Fatalf("session status = %q, want unchanged idle", rec.Status())
	}
}

func TestSendEvents_IncompatibleRegistrationPreservesConflictStatus(t *testing.T) {
	scheduler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/agents" {
			http.Error(w, "unexpected dispatch", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"agent registration incompatible: inline card differs"}`))
	}))
	defer scheduler.Close()

	st, rec := persistedSession(t, false)
	s := New(Config{TurnTimeout: time.Second}, st, ahsir.New(scheduler.URL, ""))
	rr := sendUserMessage(s, rec.Session.ID, "must not be accepted")
	if rr.Code != http.StatusConflict {
		t.Fatalf("sendEvents status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if events := rec.Snapshot(); len(events) != 0 {
		t.Fatalf("409 preflight appended events: %+v", events)
	}
}

func persistedSession(t *testing.T, archived bool) (*store.Store, *store.SessionRecord) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cma-state.json")
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	agent := &cma.Agent{
		Type: "agent", ID: "agent_persisted", Version: 4, Name: "persisted",
		Model: cma.ModelConfig{ID: "test-model"}, Metadata: map[string]string{},
		Tools: []cma.ToolDef{}, Skills: []cma.SkillRef{}, MCPServers: []cma.MCPServer{},
		CreatedAt: now, UpdatedAt: now,
	}
	if archived {
		agent.ArchivedAt = &now
	}
	if err := st.PutAgentVersion(agent); err != nil {
		t.Fatal(err)
	}
	rec := &store.SessionRecord{
		Session: &cma.Session{
			Type: "session", ID: "sesn_persisted", Status: cma.StatusIdle,
			Agent: sessionAgentFrom(agent), Resources: []any{}, VaultIDs: []string{},
			Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now,
		},
		AhsirName: translate.AhsirAgentName(agent.ID, agent.Version),
		ContextID: "ctx_persisted",
	}
	if err := st.PutSession(rec); err != nil {
		t.Fatal(err)
	}

	// Re-open the file to prove this is the restart path, not merely an
	// in-memory object graph assembled by the test.
	restarted, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	restartedRec, ok := restarted.Session(rec.Session.ID)
	if !ok {
		t.Fatal("persisted session missing after store restart")
	}
	return restarted, restartedRec
}

func writeCompletedStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`data: {"result":{"kind":"task","id":"task-1","status":{"state":"completed","message":{"parts":[{"kind":"text","text":"` + text + `"}]}}}}` + "\n\n"))
}

func sendUserMessage(s *Server, sessionID, text string) *httptest.ResponseRecorder {
	body := `{"events":[{"type":"user.message","content":[{"type":"text","text":"` + text + `"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/events", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func waitForSessionStatus(t *testing.T, rec *store.SessionRecord, want string) {
	t.Helper()
	wantEvent := cma.EvtSessionStatusIdle
	if want == cma.StatusTerminated {
		wantEvent = cma.EvtSessionStatusTerminate
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events := rec.Snapshot()
		for _, ev := range events {
			if ev.Type == wantEvent && rec.Status() == want && rec.TurnsIdle() {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session status = %q, want %q; events=%+v", rec.Status(), want, rec.Snapshot())
}

func assertStructuredTerminalError(t *testing.T, rec *store.SessionRecord, parts ...string) {
	t.Helper()
	for _, ev := range rec.Snapshot() {
		if ev.Error == nil {
			continue
		}
		for _, part := range parts {
			if !strings.Contains(ev.Error.Message, part) {
				t.Fatalf("terminal error %q does not contain %q", ev.Error.Message, part)
			}
		}
		return
	}
	t.Fatalf("session has no terminal error event: %+v", rec.Snapshot())
}
