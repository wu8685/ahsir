package cmagateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/cmagateway/ahsir"
	"github.com/wu8685/ahsir/internal/cmagateway/cma"
	"github.com/wu8685/ahsir/internal/cmagateway/store"
	"github.com/wu8685/ahsir/internal/cmagateway/translate"
)

// ----- shared fakes -----

// fakeGradeResult is one queued grader response.
type fakeGradeResult struct {
	grade *cma.OutcomeGrade
	err   error
}

// fakeGrader is a scripted OutcomeGrader. Each Grade call pops one queued
// result; an empty queue is a test bug and fails the call. When blockUntilCtx
// is set, Grade blocks until its context is cancelled (for interrupt tests).
type fakeGrader struct {
	mu            sync.Mutex
	queue         []fakeGradeResult
	reqs          []GradeRequest
	blockUntilCtx bool
}

func (f *fakeGrader) Grade(ctx context.Context, req GradeRequest) (*cma.OutcomeGrade, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	if f.blockUntilCtx {
		f.mu.Unlock()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if len(f.queue) == 0 {
		f.mu.Unlock()
		return nil, fmt.Errorf("fake grader: unexpected Grade call (iteration %d)", req.Iteration)
	}
	res := f.queue[0]
	f.queue = f.queue[1:]
	f.mu.Unlock()
	return res.grade, res.err
}

func (f *fakeGrader) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeGrader) lastReq() GradeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func satisfiedGrade(explanation string) *cma.OutcomeGrade {
	return &cma.OutcomeGrade{
		Verdict:     cma.OutcomeVerdictSatisfied,
		Explanation: explanation,
		Criteria:    []cma.OutcomeCriterion{{Criterion: "c1", Passed: true}},
	}
}

func revisionGrade(explanation, failingCriterion string) *cma.OutcomeGrade {
	return &cma.OutcomeGrade{
		Verdict:     cma.OutcomeVerdictNeedsRevision,
		Explanation: explanation,
		Criteria:    []cma.OutcomeCriterion{{Criterion: failingCriterion, Passed: false, Feedback: explanation}},
	}
}

// streamCapture records the raw JSON-RPC bodies posted to /a2a/{name}.
type streamCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *streamCapture) add(b string) {
	c.mu.Lock()
	c.bodies = append(c.bodies, b)
	c.mu.Unlock()
}

func (c *streamCapture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.bodies))
	copy(out, c.bodies)
	return out
}

// scriptedScheduler serves the three scheduler routes the facade touches:
// /admin/agents (register), /a2a/{name} (message/stream + tasks/cancel), and
// /agents/{name}/chat (the grader's synchronous chat).
type scriptedScheduler struct {
	*httptest.Server
	streams *streamCapture

	mu            sync.Mutex
	chatBodies    []string
	chatReplies   []string
	defaultReply  string
	blockStream   chan struct{} // closed to release a blocked stream
	cancelCh      chan struct{} // closed on tasks/cancel
	streamReplies []string      // queued reply texts for message/stream
}

func newScriptedScheduler(t *testing.T) *scriptedScheduler {
	t.Helper()
	ss := &scriptedScheduler{streams: &streamCapture{}, defaultReply: "deliverable"}
	ss.Server = httptest.NewServer(http.HandlerFunc(ss.route))
	t.Cleanup(ss.Server.Close)
	return ss
}

func (ss *scriptedScheduler) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/admin/agents":
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/a2a/"):
		ss.routeA2A(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/agents/") && strings.HasSuffix(r.URL.Path, "/chat"):
		ss.routeChat(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/agents/"), "/chat"))
	default:
		http.NotFound(w, r)
	}
}

func (ss *scriptedScheduler) routeA2A(w http.ResponseWriter, r *http.Request) {
	var rpc struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	body := readBody(r)
	_ = json.Unmarshal(body, &rpc)
	if rpc.Method == "tasks/cancel" {
		ss.mu.Lock()
		ch := ss.cancelCh
		ss.mu.Unlock()
		if ch != nil {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{}}`))
		return
	}
	ss.streams.add(string(body))
	ss.mu.Lock()
	block := ss.blockStream
	replies := ss.streamReplies
	def := ss.defaultReply
	ss.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
			return
		}
	}
	reply := def
	if len(replies) > 0 {
		ss.mu.Lock()
		reply = ss.streamReplies[0]
		ss.streamReplies = ss.streamReplies[1:]
		ss.mu.Unlock()
	}
	writeCompletedStream(w, reply)
}

func (ss *scriptedScheduler) routeChat(w http.ResponseWriter, r *http.Request, agent string) {
	body := readBody(r)
	ss.mu.Lock()
	ss.chatBodies = append(ss.chatBodies, agent+"\n"+string(body))
	replies := ss.chatReplies
	ss.mu.Unlock()
	reply := `{"verdict":"satisfied","criteria":[{"criterion":"c1","passed":true}],"explanation":"ok"}`
	if len(replies) > 0 {
		ss.mu.Lock()
		reply = ss.chatReplies[0]
		ss.chatReplies = ss.chatReplies[1:]
		ss.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"response": reply})
}

func readBody(r *http.Request) []byte {
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return []byte(sb.String())
}

// ----- helpers -----

func newOutcomeSession(t *testing.T) (*store.Store, *store.SessionRecord) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "cma-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	agent := &cma.Agent{
		Type: "agent", ID: "agent_oc", Version: 1, Name: "oc",
		Model: cma.ModelConfig{ID: "test-model"}, Metadata: map[string]string{},
		Tools: []cma.ToolDef{}, Skills: []cma.SkillRef{}, MCPServers: []cma.MCPServer{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.PutAgentVersion(agent); err != nil {
		t.Fatal(err)
	}
	rec := &store.SessionRecord{
		Session: &cma.Session{
			Type: "session", ID: "sesn_oc", Status: cma.StatusIdle,
			Agent: sessionAgentFrom(agent), Resources: []any{}, VaultIDs: []string{},
			Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now,
		},
		AhsirName: translate.AhsirAgentName(agent.ID, agent.Version),
		ContextID: "ctx_oc",
	}
	if err := st.PutSession(rec); err != nil {
		t.Fatal(err)
	}
	return st, rec
}

func withFakeGrader(s *Server, g OutcomeGrader) {
	s.graderFor = func(*cma.Agent) OutcomeGrader { return g }
}

func defineOutcomeBody(desc, rubric string, maxIter int) string {
	ev := map[string]any{
		"type":           cma.EvtUserDefineOutcome,
		"description":    desc,
		"rubric":         map[string]string{"type": "text", "content": rubric},
		"max_iterations": maxIter,
	}
	b, _ := json.Marshal(map[string]any{"events": []any{ev}})
	return string(b)
}

func sendDefineOutcome(s *Server, sessionID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/events", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func sendInterrupt(s *Server, sessionID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/events",
		strings.NewReader(`{"events":[{"type":"user.interrupt"}]}`))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func getSession(t *testing.T, s *Server, sessionID string) cma.Session {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sessionID, nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET session status = %d: %s", rr.Code, rr.Body.String())
	}
	var sess cma.Session
	if err := json.Unmarshal(rr.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	return sess
}

func listEvents(t *testing.T, s *Server, sessionID string) []cma.Event {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET events status = %d: %s", rr.Code, rr.Body.String())
	}
	var page cma.List[cma.Event]
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Data
}

func eventsOfType(events []cma.Event, typ string) []cma.Event {
	var out []cma.Event
	for _, ev := range events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

// waitForOutcome polls the read model until the latest outcome reaches one of
// the wanted results.
func waitForOutcome(t *testing.T, s *Server, sessionID string, want ...string) cma.OutcomeEvaluation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess := getSession(t, s, sessionID)
		if n := len(sess.OutcomeEvaluations); n > 0 {
			oc := sess.OutcomeEvaluations[n-1]
			for _, w := range want {
				if oc.Result == w {
					return oc
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	sess := getSession(t, s, sessionID)
	t.Fatalf("outcome never reached %v; read model=%+v", want, sess.OutcomeEvaluations)
	return cma.OutcomeEvaluation{}
}

func newOutcomeServer(st *store.Store, schedulerURL string) *Server {
	return New(Config{TurnTimeout: 5 * time.Second}, st, ahsir.New(schedulerURL, ""))
}

// ----- flow tests -----

func TestDefineOutcome_Satisfied(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	grader := &fakeGrader{queue: []fakeGradeResult{{grade: satisfiedGrade("all criteria met")}}}
	withFakeGrader(s, grader)

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("build the report", "# Rubric\n- c1", 0))
	if rr.Code != http.StatusOK {
		t.Fatalf("define_outcome status = %d: %s", rr.Code, rr.Body.String())
	}
	oc := waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultSatisfied)

	if !strings.HasPrefix(oc.OutcomeID, "outc_") {
		t.Fatalf("outcome_id = %q, want outc_ prefix", oc.OutcomeID)
	}
	if oc.MaxIterations != 3 {
		t.Fatalf("max_iterations = %d, want resolved default 3", oc.MaxIterations)
	}
	if oc.Iteration != 0 {
		t.Fatalf("iteration = %d, want 0", oc.Iteration)
	}
	if oc.Deliverable != "deliverable" {
		t.Fatalf("deliverable = %q, want executor reply", oc.Deliverable)
	}
	if oc.Explanation != "all criteria met" {
		t.Fatalf("explanation = %q", oc.Explanation)
	}
	if len(oc.Criteria) != 1 || !oc.Criteria[0].Passed {
		t.Fatalf("criteria = %+v, want one passing criterion", oc.Criteria)
	}
	if oc.CompletedAt == nil {
		t.Fatal("completed_at not set on terminal outcome")
	}

	events := listEvents(t, s, rec.Session.ID)
	// Inbound event is persisted with the generated outcome_id echoed back.
	defs := eventsOfType(events, cma.EvtUserDefineOutcome)
	if len(defs) != 1 || defs[0].OutcomeID != oc.OutcomeID {
		t.Fatalf("define_outcome echo missing outcome_id: %+v", defs)
	}
	if defs[0].MaxIterations != 3 || defs[0].Description != "build the report" {
		t.Fatalf("echo = %+v, want resolved max_iterations and description", defs[0])
	}
	starts := eventsOfType(events, cma.EvtSpanOutcomeEvaluationStart)
	if len(starts) != 1 || starts[0].Iteration == nil || *starts[0].Iteration != 0 {
		t.Fatalf("evaluation start events = %+v", starts)
	}
	if got := eventsOfType(events, cma.EvtSpanOutcomeEvaluationOngoing); len(got) == 0 {
		t.Fatal("no outcome_evaluation_ongoing heartbeat emitted")
	}
	ends := eventsOfType(events, cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 1 {
		t.Fatalf("evaluation end events = %d, want 1", len(ends))
	}
	end := ends[0]
	if end.Result != cma.OutcomeResultSatisfied || end.Explanation != "all criteria met" {
		t.Fatalf("end = %+v", end)
	}
	if end.OutcomeEvaluationStartID == nil || *end.OutcomeEvaluationStartID != starts[0].ID {
		t.Fatalf("end.outcome_evaluation_start_id does not link the start event: %+v", end)
	}
	if end.Usage == nil {
		t.Fatal("end.usage must be emitted (zero-valued) per the SDK model")
	}
	if rec.Status() != cma.StatusIdle {
		t.Fatalf("session status = %q, want idle", rec.Status())
	}
}

func TestDefineOutcome_NeedsRevisionThenSatisfied(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	grader := &fakeGrader{queue: []fakeGradeResult{
		{grade: revisionGrade("fix the axis label", "axis labeled")},
		{grade: satisfiedGrade("now correct")},
	}}
	withFakeGrader(s, grader)

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("plot", "# Rubric\n- axis labeled", 4))
	if rr.Code != http.StatusOK {
		t.Fatalf("define_outcome status = %d: %s", rr.Code, rr.Body.String())
	}
	oc := waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultSatisfied)
	if oc.Iteration != 1 {
		t.Fatalf("iteration = %d, want 1 after one revision", oc.Iteration)
	}

	streams := sched.streams.all()
	if len(streams) != 2 {
		t.Fatalf("executor dispatches = %d, want 2 (initial + revision)", len(streams))
	}
	if !strings.Contains(streams[1], "fix the axis label") {
		t.Fatalf("revision turn did not carry grader feedback:\n%s", streams[1])
	}
	if !strings.Contains(streams[0], "# Rubric") {
		t.Fatalf("initial turn did not carry the rubric:\n%s", streams[0])
	}

	ends := eventsOfType(listEvents(t, s, rec.Session.ID), cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 2 {
		t.Fatalf("end events = %d, want 2", len(ends))
	}
	if ends[0].Result != cma.OutcomeResultNeedsRevision || *ends[0].Iteration != 0 {
		t.Fatalf("first end = %+v, want needs_revision at iteration 0", ends[0])
	}
	if ends[1].Result != cma.OutcomeResultSatisfied || *ends[1].Iteration != 1 {
		t.Fatalf("second end = %+v, want satisfied at iteration 1", ends[1])
	}
	if got := grader.calls(); got != 2 {
		t.Fatalf("grader calls = %d, want 2", got)
	}
}

func TestDefineOutcome_MaxIterationsReached(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	grader := &fakeGrader{queue: []fakeGradeResult{
		{grade: revisionGrade("still wrong", "c1")},
		{grade: revisionGrade("still wrong", "c1")},
	}}
	withFakeGrader(s, grader)

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("plot", "# Rubric", 2))
	if rr.Code != http.StatusOK {
		t.Fatalf("define_outcome status = %d: %s", rr.Code, rr.Body.String())
	}
	oc := waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultMaxIterationsReached)
	waitForSessionStatus(t, rec, cma.StatusIdle)

	// Two evaluated iterations plus one final acknowledgment turn (per the CMA
	// docs, one acknowledgment turn follows max_iterations_reached before idle).
	if streams := sched.streams.all(); len(streams) != 3 {
		t.Fatalf("executor dispatches = %d, want 2 iterations + 1 acknowledgment turn", len(streams))
	}
	if oc.Deliverable == "" {
		t.Fatal("deliverable empty; the acknowledgment reply should be recorded")
	}
	ends := eventsOfType(listEvents(t, s, rec.Session.ID), cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 2 || ends[1].Result != cma.OutcomeResultMaxIterationsReached {
		t.Fatalf("end events = %+v, want [needs_revision, max_iterations_reached]", ends)
	}
	if got := grader.calls(); got != 2 {
		t.Fatalf("grader calls = %d, want exactly max_iterations", got)
	}
}

func TestDefineOutcome_GraderFailureEndsFailed(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	grader := &fakeGrader{queue: []fakeGradeResult{
		{err: fmt.Errorf("grader unavailable")},
		{err: fmt.Errorf("grader unavailable again")},
	}}
	withFakeGrader(s, grader)

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("plot", "# Rubric", 3))
	if rr.Code != http.StatusOK {
		t.Fatalf("define_outcome status = %d: %s", rr.Code, rr.Body.String())
	}
	oc := waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultFailed)
	waitForSessionStatus(t, rec, cma.StatusIdle)

	if got := grader.calls(); got != 2 {
		t.Fatalf("grader calls = %d, want 1 attempt + 1 retry", got)
	}
	events := listEvents(t, s, rec.Session.ID)
	ends := eventsOfType(events, cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 1 || ends[0].Result != cma.OutcomeResultFailed {
		t.Fatalf("end events = %+v, want one failed", ends)
	}
	if !strings.Contains(ends[0].Explanation, "grader") {
		t.Fatalf("failed end should explain the grading failure: %+v", ends[0])
	}
	// One ongoing per attempt (initial + retry).
	if got := eventsOfType(events, cma.EvtSpanOutcomeEvaluationOngoing); len(got) != 2 {
		t.Fatalf("ongoing events = %d, want one per grading attempt", len(got))
	}
	if oc.Explanation == "" {
		t.Fatal("read model should carry the failure explanation")
	}
	// A failed outcome must not terminate the session: the session stays usable.
	if streams := sched.streams.all(); len(streams) != 1 {
		t.Fatalf("executor dispatches = %d, want 1", len(streams))
	}
}

func TestDefineOutcome_InterruptDuringGrading(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	grader := &fakeGrader{blockUntilCtx: true}
	withFakeGrader(s, grader)

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("plot", "# Rubric", 3))
	if rr.Code != http.StatusOK {
		t.Fatalf("define_outcome status = %d: %s", rr.Code, rr.Body.String())
	}
	// The read model must report the evaluating state while the grader runs.
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeStateEvaluating)

	if rr := sendInterrupt(s, rec.Session.ID); rr.Code != http.StatusOK {
		t.Fatalf("interrupt status = %d: %s", rr.Code, rr.Body.String())
	}
	oc := waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultInterrupted)
	waitForSessionStatus(t, rec, cma.StatusIdle)

	ends := eventsOfType(listEvents(t, s, rec.Session.ID), cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 1 || ends[0].Result != cma.OutcomeResultInterrupted {
		t.Fatalf("end events = %+v, want interrupted", ends)
	}
	// An evaluation had started, so the end event links to it.
	if ends[0].OutcomeEvaluationStartID == nil || *ends[0].OutcomeEvaluationStartID == "" {
		t.Fatalf("interrupted end should link its start event: %+v", ends[0])
	}
	if oc.CompletedAt == nil {
		t.Fatal("completed_at not set on interrupted outcome")
	}

	// After a terminal result a new outcome may be defined on the same session.
	grader2 := &fakeGrader{queue: []fakeGradeResult{{grade: satisfiedGrade("ok")}}}
	withFakeGrader(s, grader2)
	rr = sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("next", "# Rubric", 1))
	if rr.Code != http.StatusOK {
		t.Fatalf("chained define_outcome status = %d: %s", rr.Code, rr.Body.String())
	}
	waitForOutcomeResultAt(t, s, rec.Session.ID, 1, cma.OutcomeResultSatisfied)
	sess := getSession(t, s, rec.Session.ID)
	if len(sess.OutcomeEvaluations) != 2 {
		t.Fatalf("outcome_evaluations = %d entries, want 2 chained outcomes", len(sess.OutcomeEvaluations))
	}
	waitForOutcomeResultAt(t, s, rec.Session.ID, 1, cma.OutcomeResultSatisfied)
}

func TestDefineOutcome_InterruptDuringExecutorTurn(t *testing.T) {
	sched := newScriptedScheduler(t)
	sched.mu.Lock()
	sched.blockStream = make(chan struct{})
	sched.cancelCh = make(chan struct{})
	sched.mu.Unlock()
	// Release the blocked stream once tasks/cancel arrives, then report the
	// canceled task state like a real runtime would.
	go func() {
		<-sched.cancelCh
		sched.mu.Lock()
		close(sched.blockStream)
		sched.defaultReply = ""
		sched.mu.Unlock()
	}()

	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	grader := &fakeGrader{}
	withFakeGrader(s, grader)

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("plot", "# Rubric", 3))
	if rr.Code != http.StatusOK {
		t.Fatalf("define_outcome status = %d: %s", rr.Code, rr.Body.String())
	}
	// Read model reports running while the executor turn is in flight.
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeStateRunning)

	if rr := sendInterrupt(s, rec.Session.ID); rr.Code != http.StatusOK {
		t.Fatalf("interrupt status = %d: %s", rr.Code, rr.Body.String())
	}
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultInterrupted)
	waitForSessionStatus(t, rec, cma.StatusIdle)

	ends := eventsOfType(listEvents(t, s, rec.Session.ID), cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 1 || ends[0].Result != cma.OutcomeResultInterrupted {
		t.Fatalf("end events = %+v, want interrupted", ends)
	}
	// No evaluation ever started: the docs require an empty start id here.
	if ends[0].OutcomeEvaluationStartID == nil || *ends[0].OutcomeEvaluationStartID != "" {
		t.Fatalf("interrupted-before-start end must carry empty start id: %+v", ends[0])
	}
	if got := grader.calls(); got != 0 {
		t.Fatalf("grader calls = %d, want 0 (interrupted before evaluation)", got)
	}
}

func TestDefineOutcome_ConcurrentDefineConflict(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	grader := &fakeGrader{blockUntilCtx: true}
	withFakeGrader(s, grader)

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("first", "# Rubric", 3))
	if rr.Code != http.StatusOK {
		t.Fatalf("first define status = %d: %s", rr.Code, rr.Body.String())
	}
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeStateEvaluating)

	rr = sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("second", "# Rubric", 3))
	if rr.Code != http.StatusConflict {
		t.Fatalf("concurrent define status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	sess := getSession(t, s, rec.Session.ID)
	if len(sess.OutcomeEvaluations) != 1 {
		t.Fatalf("conflict must not append an outcome entry: %+v", sess.OutcomeEvaluations)
	}

	sendInterrupt(s, rec.Session.ID)
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultInterrupted)
	waitForSessionStatus(t, rec, cma.StatusIdle)
}

func TestDefineOutcome_Validation(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	withFakeGrader(s, &fakeGrader{})

	cases := map[string]string{
		"blank description":    `{"events":[{"type":"user.define_outcome","description":"  ","rubric":{"type":"text","content":"# R"}}]}`,
		"missing rubric":       `{"events":[{"type":"user.define_outcome","description":"d"}]}`,
		"blank rubric":         `{"events":[{"type":"user.define_outcome","description":"d","rubric":{"type":"text","content":"   "}}]}`,
		"file rubric":          `{"events":[{"type":"user.define_outcome","description":"d","rubric":{"type":"file","file_id":"file_1"}}]}`,
		"negative iterations":  defineOutcomeBody("d", "# R", -1),
		"over-cap iterations":  defineOutcomeBody("d", "# R", 21),
		"oversize description": defineOutcomeBody(strings.Repeat("x", 17<<10), "# R", 1),
		"oversize rubric":      defineOutcomeBody("d", strings.Repeat("x", 65<<10), 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(rec.Snapshot())
			rr := sendDefineOutcome(s, rec.Session.ID, body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
			}
			if got := len(rec.Snapshot()); got != before {
				t.Fatalf("rejected define_outcome persisted %d events", got-before)
			}
			if n := len(getSession(t, s, rec.Session.ID).OutcomeEvaluations); n != 0 {
				t.Fatalf("rejected define_outcome left %d outcome entries", n)
			}
		})
	}
}

func waitForStatus(t *testing.T, rec *store.SessionRecord, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.Status() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session status = %q, want %q", rec.Status(), want)
}

func waitForOutcomeResultAt(t *testing.T, s *Server, sessionID string, idx int, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess := getSession(t, s, sessionID)
		if len(sess.OutcomeEvaluations) > idx && sess.OutcomeEvaluations[idx].Result == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("outcome_evaluations[%d] never reached %q", idx, want)
}

func TestOutcome_PendingWhileQueued(t *testing.T) {
	sched := newScriptedScheduler(t)
	sched.mu.Lock()
	sched.blockStream = make(chan struct{})
	sched.mu.Unlock()

	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	withFakeGrader(s, &fakeGrader{queue: []fakeGradeResult{{grade: satisfiedGrade("ok")}}})

	// Occupy the serial turn queue with a blocked user.message turn.
	rr := sendUserMessage(s, rec.Session.ID, "hold the queue")
	if rr.Code != http.StatusOK {
		t.Fatalf("user.message status = %d", rr.Code)
	}
	waitForStatus(t, rec, cma.StatusRunning)

	rr = sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("queued", "# R", 1))
	if rr.Code != http.StatusOK {
		t.Fatalf("define_outcome status = %d: %s", rr.Code, rr.Body.String())
	}
	// The outcome is accepted but cannot start until the turn ahead drains.
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeStatePending)

	sched.mu.Lock()
	close(sched.blockStream)
	sched.mu.Unlock()
	waitForOutcomeResultAt(t, s, rec.Session.ID, 0, cma.OutcomeResultSatisfied)
	waitForSessionStatus(t, rec, cma.StatusIdle)
}

func TestOutcome_RestartMarksActiveOutcomeFailed(t *testing.T) {
	for _, state := range []string{cma.OutcomeStatePending, cma.OutcomeStateRunning, cma.OutcomeStateEvaluating} {
		t.Run(state, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cma-state.json")
			st, err := store.New(path)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			agent := &cma.Agent{
				Type: "agent", ID: "agent_oc", Version: 1, Name: "oc",
				Model: cma.ModelConfig{ID: "test-model"}, Metadata: map[string]string{},
				Tools: []cma.ToolDef{}, Skills: []cma.SkillRef{}, MCPServers: []cma.MCPServer{},
				CreatedAt: now, UpdatedAt: now,
			}
			if err := st.PutAgentVersion(agent); err != nil {
				t.Fatal(err)
			}
			rec := &store.SessionRecord{
				Session: &cma.Session{
					Type: "session", ID: "sesn_oc", Status: cma.StatusRunning,
					Agent: sessionAgentFrom(agent), Resources: []any{}, VaultIDs: []string{},
					Metadata: map[string]string{}, CreatedAt: now, UpdatedAt: now,
					OutcomeEvaluations: []cma.OutcomeEvaluation{{
						OutcomeID: "outc_active", Description: "d", Rubric: "# R",
						MaxIterations: 3, Result: state, CreatedAt: now, UpdatedAt: now,
					}},
				},
				AhsirName: translate.AhsirAgentName(agent.ID, agent.Version),
				ContextID: "ctx_oc",
			}
			if err := st.PutSession(rec); err != nil {
				t.Fatal(err)
			}

			// Simulate a facade restart: reopen the store and construct a new
			// Server, which reconciles interrupted outcome state.
			restarted, err := store.New(path)
			if err != nil {
				t.Fatal(err)
			}
			sched := newScriptedScheduler(t)
			s := newOutcomeServer(restarted, sched.URL)

			sess := getSession(t, s, "sesn_oc")
			if len(sess.OutcomeEvaluations) != 1 {
				t.Fatalf("outcome entries = %d, want 1", len(sess.OutcomeEvaluations))
			}
			oc := sess.OutcomeEvaluations[0]
			if oc.Result != cma.OutcomeResultFailed {
				t.Fatalf("active outcome after restart = %q, want failed (never guessed satisfied)", oc.Result)
			}
			if oc.CompletedAt == nil {
				t.Fatal("completed_at not set by recovery")
			}
			events := listEvents(t, s, "sesn_oc")
			ends := eventsOfType(events, cma.EvtSpanOutcomeEvaluationEnd)
			if len(ends) != 1 || ends[0].Result != cma.OutcomeResultFailed {
				t.Fatalf("recovery end events = %+v, want one failed", ends)
			}
			if !strings.Contains(ends[0].Explanation, "restart") {
				t.Fatalf("recovery end should explain the restart: %+v", ends[0])
			}
		})
	}
}

func TestOutcome_TerminalOutcomeSurvivesRestart(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	path := st.Path()
	s := newOutcomeServer(st, sched.URL)
	withFakeGrader(s, &fakeGrader{queue: []fakeGradeResult{{grade: satisfiedGrade("ok")}}})

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("d", "# R", 1))
	if rr.Code != http.StatusOK {
		t.Fatalf("define status = %d", rr.Code)
	}
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultSatisfied)
	waitForSessionStatus(t, rec, cma.StatusIdle)

	restarted, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	s2 := newOutcomeServer(restarted, sched.URL)
	sess := getSession(t, s2, rec.Session.ID)
	if len(sess.OutcomeEvaluations) != 1 || sess.OutcomeEvaluations[0].Result != cma.OutcomeResultSatisfied {
		t.Fatalf("terminal outcome rewritten by restart: %+v", sess.OutcomeEvaluations)
	}
	// No extra end event beyond the satisfied one from before the restart.
	ends := eventsOfType(listEvents(t, s2, rec.Session.ID), cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 1 || ends[0].Result != cma.OutcomeResultSatisfied {
		t.Fatalf("restart appended spurious end events: %+v", ends)
	}
}

func TestOutcome_StreamAndListAgree(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	withFakeGrader(s, &fakeGrader{queue: []fakeGradeResult{{grade: satisfiedGrade("ok")}}})

	facade := httptest.NewServer(s.Handler())
	defer facade.Close()

	// Subscribe BEFORE defining the outcome: the stream is a live tail.
	req, _ := http.NewRequest(http.MethodGet, facade.URL+"/v1/sessions/"+rec.Session.ID+"/events/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	type sseEvent struct{ typ, id string }
	streamed := make(chan sseEvent, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		var typ string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				typ = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") && typ != "" {
				var ev cma.Event
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
					streamed <- sseEvent{typ: typ, id: ev.ID}
				}
				typ = ""
			}
		}
	}()

	post, _ := http.NewRequest(http.MethodPost, facade.URL+"/v1/sessions/"+rec.Session.ID+"/events",
		strings.NewReader(defineOutcomeBody("d", "# R", 1)))
	post.Header.Set("Content-Type", "application/json")
	postResp, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("define status = %d", postResp.StatusCode)
	}

	// Collect the stream until the terminal end event arrives.
	var got []sseEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-streamed:
			got = append(got, ev)
			if ev.typ == cma.EvtSpanOutcomeEvaluationEnd {
				goto drained
			}
		case <-deadline:
			t.Fatalf("stream never delivered the outcome end event; got %+v", got)
		}
	}
drained:

	// Let the outcome loop finish persisting before TempDir cleanup runs.
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultSatisfied)
	waitForSessionStatus(t, rec, cma.StatusIdle)

	listed := listEvents(t, s, rec.Session.ID)
	listedByID := map[string]string{}
	for _, ev := range listed {
		listedByID[ev.ID] = ev.Type
	}
	if len(got) == 0 {
		t.Fatal("stream delivered nothing")
	}
	for _, ev := range got {
		if listedByID[ev.id] != ev.typ {
			t.Fatalf("streamed event %s (%s) missing/mismatched in events list", ev.id, ev.typ)
		}
	}
	// And the stream must have carried the full outcome span sequence.
	var types []string
	for _, ev := range got {
		types = append(types, ev.typ)
	}
	for _, want := range []string{cma.EvtUserDefineOutcome, cma.EvtSpanOutcomeEvaluationStart, cma.EvtSpanOutcomeEvaluationOngoing, cma.EvtSpanOutcomeEvaluationEnd} {
		found := false
		for _, typ := range types {
			if typ == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("stream missing %s; got %v", want, types)
		}
	}
}

func TestOutcome_GraderIsolationAndNoReasoningLeak(t *testing.T) {
	sched := newScriptedScheduler(t)
	// The grader (a real ahsirGrader driven over the fake scheduler) replies
	// with prose, a chain-of-thought-ish preamble, and an extra field that must
	// never surface in the event stream or the read model.
	sched.mu.Lock()
	sched.chatReplies = []string{
		"Let me think step by step. SECRET-REASONING the deliverable looks fine.\n" +
			"```json\n{\"verdict\":\"satisfied\",\"criteria\":[{\"criterion\":\"c1\",\"passed\":true,\"feedback\":\"good\"}],\"explanation\":\"all good\",\"reasoning\":\"SECRET-REASONING\"}\n```",
	}
	sched.mu.Unlock()

	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL) // default grader factory: ahsir-backed

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("d", "# R", 1))
	if rr.Code != http.StatusOK {
		t.Fatalf("define status = %d: %s", rr.Code, rr.Body.String())
	}
	waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultSatisfied)
	waitForSessionStatus(t, rec, cma.StatusIdle)

	// The grader ran as its own agent with a fresh context — never the
	// executor's contextId.
	sched.mu.Lock()
	chats := append([]string(nil), sched.chatBodies...)
	sched.mu.Unlock()
	if len(chats) != 1 {
		t.Fatalf("grader chat calls = %d, want 1", len(chats))
	}
	if !strings.HasPrefix(chats[0], "cma-grader-") {
		t.Fatalf("grader chat went to %q, want a cma-grader-* agent", chats[0])
	}
	if strings.Contains(chats[0], rec.ContextID) {
		t.Fatal("grader must not reuse the executor's contextId")
	}

	// Hidden reasoning must not leak into any event or the read model.
	all, _ := json.Marshal(listEvents(t, s, rec.Session.ID))
	if strings.Contains(string(all), "SECRET-REASONING") {
		t.Fatal("grader reasoning leaked into the event log")
	}
	sess := getSession(t, s, rec.Session.ID)
	oc := sess.OutcomeEvaluations[0]
	if oc.Explanation != "all good" || len(oc.Criteria) != 1 {
		t.Fatalf("read model grade = %+v", oc)
	}
	rm, _ := json.Marshal(sess)
	if strings.Contains(string(rm), "SECRET-REASONING") {
		t.Fatal("grader reasoning leaked into the session read model")
	}
}

func TestOutcome_DeliverableTruncated(t *testing.T) {
	sched := newScriptedScheduler(t)
	big := strings.Repeat("y", 300<<10)
	sched.mu.Lock()
	sched.defaultReply = big
	sched.mu.Unlock()

	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	grader := &fakeGrader{queue: []fakeGradeResult{{grade: satisfiedGrade("ok")}}}
	withFakeGrader(s, grader)

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("d", "# R", 1))
	if rr.Code != http.StatusOK {
		t.Fatalf("define status = %d", rr.Code)
	}
	oc := waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultSatisfied)
	if len(oc.Deliverable) >= len(big) {
		t.Fatalf("deliverable not truncated: %d bytes", len(oc.Deliverable))
	}
	if !strings.Contains(oc.Deliverable, "truncated") {
		t.Fatal("truncated deliverable should carry a marker")
	}
	// The grader receives the truncated reply, not the unbounded one.
	if got := len(grader.lastReq().Response); got >= len(big) {
		t.Fatalf("grader received untruncated response: %d bytes", got)
	}
}

func TestDefineOutcome_ExecutorFailureTerminatesOutcome(t *testing.T) {
	// A persistent non-404 dispatch failure is not reconciled/replayed: the
	// session terminates and the outcome records failed (never idle).
	sched := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/agents" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.Error(w, `{"error":"runtime exploded"}`, http.StatusInternalServerError)
	}))
	defer sched.Close()

	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	withFakeGrader(s, &fakeGrader{})

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("d", "# R", 3))
	if rr.Code != http.StatusOK {
		t.Fatalf("define status = %d: %s", rr.Code, rr.Body.String())
	}
	oc := waitForOutcome(t, s, rec.Session.ID, cma.OutcomeResultFailed)
	waitForSessionStatus(t, rec, cma.StatusTerminated)
	if !strings.Contains(oc.Explanation, "executor") {
		t.Fatalf("outcome explanation = %q, want executor failure", oc.Explanation)
	}
	ends := eventsOfType(listEvents(t, s, rec.Session.ID), cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 1 || ends[0].Result != cma.OutcomeResultFailed {
		t.Fatalf("end events = %+v, want one failed", ends)
	}
}

func TestDefineOutcome_ArchivedAgentRejected(t *testing.T) {
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	if _, ok := st.ArchiveAgent(rec.Session.Agent.ID, time.Now().UTC()); !ok {
		t.Fatal("archive agent")
	}
	s := newOutcomeServer(st, sched.URL)
	withFakeGrader(s, &fakeGrader{})

	rr := sendDefineOutcome(s, rec.Session.ID, defineOutcomeBody("d", "# R", 1))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("define on archived agent = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if events := rec.Snapshot(); len(events) != 0 {
		t.Fatalf("rejected define appended events: %+v", events)
	}
	if n := len(getSession(t, s, rec.Session.ID).OutcomeEvaluations); n != 0 {
		t.Fatalf("rejected define left %d outcome entries", n)
	}
}

func TestExecuteOutcome_AgentArchivedAfterAccept(t *testing.T) {
	// The agent is archived in the window between define_outcome acceptance and
	// the loop starting: the outcome fails explicitly and the session idles.
	sched := newScriptedScheduler(t)
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, sched.URL)
	withFakeGrader(s, &fakeGrader{})

	now := time.Now().UTC()
	oc := cma.OutcomeEvaluation{
		OutcomeID: "outc_race", Description: "d", Rubric: "# R", MaxIterations: 1,
		Result: cma.OutcomeStatePending, CreatedAt: now, UpdatedAt: now,
	}
	if ok, err := st.BeginOutcome(rec, oc); !ok || err != nil {
		t.Fatalf("BeginOutcome = %v, %v", ok, err)
	}
	if _, ok := st.ArchiveAgent(rec.Session.Agent.ID, now); !ok {
		t.Fatal("archive agent")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.executeOutcome(ctx, rec, oc.OutcomeID)

	evals := rec.OutcomeEvaluations()
	if len(evals) != 1 || evals[0].Result != cma.OutcomeResultFailed {
		t.Fatalf("outcome = %+v, want failed", evals)
	}
	if rec.Status() != cma.StatusIdle {
		t.Fatalf("session status = %q, want idle", rec.Status())
	}
	ends := eventsOfType(rec.Snapshot(), cma.EvtSpanOutcomeEvaluationEnd)
	if len(ends) != 1 || ends[0].Result != cma.OutcomeResultFailed {
		t.Fatalf("end events = %+v, want one failed", ends)
	}
}

func TestExecuteOutcome_MissingOutcomeIsNoop(t *testing.T) {
	st, rec := newOutcomeSession(t)
	s := newOutcomeServer(st, newScriptedScheduler(t).URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.executeOutcome(ctx, rec, "outc_missing") // must not panic or emit
	if events := rec.Snapshot(); len(events) != 0 {
		t.Fatalf("missing outcome emitted events: %+v", events)
	}
}
