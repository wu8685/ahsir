package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRuntime emulates a provider behind a pool-like factory, in two flavors:
// claude-like (killOnCancel: a cancelled turn kills the process; the session
// is unhealthy until the factory reopens it with the same sessionID) and
// codex-like (per-turn processes; the session stays healthy across cancels).
type fakeRuntime struct {
	mu           sync.Mutex
	killOnCancel bool
	unhealthy    bool
	sessionID    string
	prompts      []string
	resumes      int
	script       func(prompt string) fakeTurnScript
}

type fakeTurnScript struct {
	partial  string // emitted (delta+text) before anything else
	block    bool   // then block until ctx is done
	reply    string // emitted on clean completion (when !block)
	failWith error  // immediate provider error
	// nilOnCancel emulates the Codex-style defect from issue #47: the turn
	// blocks until its ctx is done and then reports EventTurnDone{Err: nil}
	// — the provider "succeeded" precisely because it was killed.
	nilOnCancel bool
	// noDone closes the stream without any EventTurnDone (truncated stream).
	noDone bool
	// toolOnly emits a tool-use event and a clean done with NO text — a
	// successful turn whose delivery happened entirely through tools.
	toolOnly bool
}

type fakeSession struct{ rt *fakeRuntime }

func (s *fakeSession) Stream(ctx context.Context, text string) (<-chan Event, error) {
	s.rt.mu.Lock()
	s.rt.prompts = append(s.rt.prompts, text)
	unhealthy := s.rt.unhealthy
	sc := s.rt.script(text)
	s.rt.mu.Unlock()
	if unhealthy {
		return nil, errors.New("session not ready (evicted)")
	}
	ch := make(chan Event, 16)
	go func() {
		defer close(ch)
		if sc.failWith != nil {
			ch <- EventTurnDone{Err: sc.failWith}
			return
		}
		if sc.partial != "" {
			ch <- EventTextDelta{Text: sc.partial}
			ch <- EventText{Text: sc.partial}
		}
		if sc.toolOnly {
			ch <- EventToolUse{Id: "tool-1", Name: "deliver_message", Input: json.RawMessage(`{"to":"user"}`)}
		}
		if sc.block || sc.nilOnCancel {
			<-ctx.Done()
			s.rt.mu.Lock()
			if s.rt.killOnCancel {
				s.rt.unhealthy = true
			}
			s.rt.mu.Unlock()
			switch {
			case sc.noDone:
				return // truncated: EOF without a terminal event
			case sc.nilOnCancel:
				ch <- EventTurnDone{} // issue #47: nil error after cancellation
			default:
				ch <- EventTurnDone{Err: ErrTurnCanceled}
			}
			return
		}
		if sc.reply != "" {
			ch <- EventText{Text: sc.reply}
		}
		if sc.noDone {
			return
		}
		ch <- EventTurnDone{}
	}()
	return ch, nil
}

func (s *fakeSession) Turn(ctx context.Context, text string) (string, error) {
	ch, err := s.Stream(ctx, text)
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

func (s *fakeSession) SessionID() string { return s.rt.sessionID }
func (s *fakeSession) IsHealthy() bool {
	s.rt.mu.Lock()
	defer s.rt.mu.Unlock()
	return !s.rt.unhealthy
}
func (s *fakeSession) Close() error { return nil }

func (rt *fakeRuntime) open(_ context.Context, _ string) (Session, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.unhealthy {
		// Pool semantics: unhealthy entries are recreated with the preserved
		// sessionID as resume identity.
		rt.unhealthy = false
		rt.resumes++
	}
	return &fakeSession{rt: rt}, nil
}

func (rt *fakeRuntime) seen() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.prompts...)
}

func drain(ch <-chan Event) (events []Event, text string, doneErr error) {
	var sb strings.Builder
	for ev := range ch {
		events = append(events, ev)
		switch e := ev.(type) {
		case EventText:
			sb.WriteString(e.Text)
		case EventTurnDone:
			doneErr = e.Err
		}
	}
	return events, sb.String(), doneErr
}

func lifecycleEvents(events []Event) []EventLifecycle {
	var out []EventLifecycle
	for _, ev := range events {
		if le, ok := ev.(EventLifecycle); ok {
			out = append(out, le)
		}
	}
	return out
}

func lifecycleEventTypes(events []Event) []string {
	var out []string
	for _, le := range lifecycleEvents(events) {
		out = append(out, le.Event)
	}
	return out
}

// testLifecycle builds a controller + runtime pair. block/init/finalize
// prompts are matched by script.
func testLifecycle(t *testing.T, cfg ResolvedLifecycle, killOnCancel bool, script func(string) fakeTurnScript) (*LifecycleController, *LifecycleStore, *fakeRuntime) {
	t.Helper()
	rt := &fakeRuntime{killOnCancel: killOnCancel, sessionID: "prov-sess-1", script: script}
	store := NewLifecycleStore(t.TempDir() + "/lifecycle.json")
	ctrl := NewLifecycleController(cfg, store, rt.open)
	return ctrl, store, rt
}

func initOnlyLifecycle(onFailure string) ResolvedLifecycle {
	var r ResolvedLifecycle
	r.Initialize.Enabled = true
	r.Initialize.Timeout = 5 * time.Second
	r.Initialize.Prompt = "INIT"
	r.Initialize.OnFailure = onFailure
	r.Finalize.Enabled = false
	r.Version = "v1"
	return r
}

func finalizeLifecycle(work, grace time.Duration) ResolvedLifecycle {
	var r ResolvedLifecycle
	r.Finalize.Enabled = true
	r.Finalize.WorkTimeout = work
	r.Finalize.GraceTimeout = grace
	r.Finalize.Prompt = "FINALIZE"
	r.Version = "v1"
	return r
}

func quickScript(prompt string) fakeTurnScript {
	return fakeTurnScript{reply: "reply: " + prompt}
}

// Acceptance 1+2: first turn on a new Session initializes exactly once before
// the user turn; restart + resume never repeat a completed initialization.
func TestLifecycleInitRunsOnceBeforeFirstTurn(t *testing.T) {
	rt := &fakeRuntime{sessionID: "s1", script: quickScript}
	storePath := t.TempDir() + "/lifecycle.json"
	store := NewLifecycleStore(storePath)
	ctrl := NewLifecycleController(initOnlyLifecycle(InitFailClosed), store, rt.open)

	sess, err := ctrl.OpenSession(context.Background(), "ctx1")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := sess.Stream(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	events, text, doneErr := drain(ch)
	if doneErr != nil {
		t.Fatalf("turn failed: %v", doneErr)
	}
	if text != "reply: hello" {
		t.Fatalf("reply = %q", text)
	}
	prompts := rt.seen()
	if len(prompts) != 2 || prompts[0] != "INIT" || prompts[1] != "hello" {
		t.Fatalf("init must run once before the first user turn, prompts=%v", prompts)
	}
	want := []string{EvtSessionInitializing, EvtSessionInitialized, EvtTurnRunning, EvtTurnOutcome}
	got := lifecycleEventTypes(events)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("lifecycle events = %v, want %v", got, want)
	}

	// Second turn on the same session: no repeat init.
	ch, _ = sess.Stream(context.Background(), "again")
	if _, _, err := drain(ch); err != nil {
		t.Fatal(err)
	}
	if n := len(rt.seen()); n != 3 {
		t.Fatalf("init repeated on second turn: prompts=%v", rt.seen())
	}

	// Process restart + scale-to-zero/wake: new controller + store over the
	// same file must not repeat the completed initialization.
	ctrl2 := NewLifecycleController(initOnlyLifecycle(InitFailClosed), NewLifecycleStore(storePath), rt.open)
	sess2, _ := ctrl2.OpenSession(context.Background(), "ctx1")
	ch, _ = sess2.Stream(context.Background(), "post-restart")
	if _, _, err := drain(ch); err != nil {
		t.Fatal(err)
	}
	prompts = rt.seen()
	if len(prompts) != 4 || prompts[3] != "post-restart" {
		t.Fatalf("init repeated after restart: prompts=%v", prompts)
	}
}

// Acceptance 3: init failure produces a typed persisted outcome and the user
// turn does not start (fail closed is the default).
func TestLifecycleInitFailureFailClosed(t *testing.T) {
	providerDown := errors.New("provider unavailable")
	script := func(prompt string) fakeTurnScript {
		if prompt == "INIT" {
			return fakeTurnScript{failWith: providerDown}
		}
		return quickScript(prompt)
	}
	ctrl, store, rt := testLifecycle(t, initOnlyLifecycle(InitFailClosed), false, script)

	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, err := sess.Stream(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	events, _, doneErr := drain(ch)
	if !errors.Is(doneErr, ErrInitializationFailed) {
		t.Fatalf("turn error = %v, want ErrInitializationFailed", doneErr)
	}
	if prompts := rt.seen(); len(prompts) != 1 {
		t.Fatalf("user turn must not start after init failure: prompts=%v", prompts)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindInitializationFailure {
		t.Fatalf("typed outcome missing: %+v", out)
	}
	types := lifecycleEventTypes(events)
	if types[len(types)-1] != EvtTurnOutcome || types[0] != EvtSessionInitializing {
		t.Fatalf("event sequence wrong: %v", types)
	}
	found := false
	for _, ty := range types {
		if ty == EvtSessionInitializationFailed {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing %s in %v", EvtSessionInitializationFailed, types)
	}

	// Fail-closed means the next turn retries initialization (marker absent).
	scriptOK := rt.script
	_ = scriptOK
	rt.mu.Lock()
	rt.script = quickScript
	rt.mu.Unlock()
	ch, _ = sess.Stream(context.Background(), "retry")
	_, text, doneErr := drain(ch)
	if doneErr != nil || text != "reply: retry" {
		t.Fatalf("retry after fixing init should succeed: text=%q err=%v", text, doneErr)
	}
	if prompts := rt.seen(); len(prompts) != 3 || prompts[1] != "INIT" {
		t.Fatalf("init must be retried after failure: prompts=%v", prompts)
	}
}

// Init timeout is classified as an initialization failure and bounded by its
// own timeout.
func TestLifecycleInitTimeout(t *testing.T) {
	cfg := initOnlyLifecycle(InitFailClosed)
	cfg.Initialize.Timeout = 50 * time.Millisecond
	script := func(prompt string) fakeTurnScript {
		if prompt == "INIT" {
			return fakeTurnScript{block: true}
		}
		return quickScript(prompt)
	}
	ctrl, store, _ := testLifecycle(t, cfg, false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	start := time.Now()
	ch, _ := sess.Stream(context.Background(), "hello")
	_, _, doneErr := drain(ch)
	if !errors.Is(doneErr, ErrInitializationFailed) {
		t.Fatalf("err = %v", doneErr)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("init timeout not enforced promptly: %v", elapsed)
	}
	if out := store.Record("ctx1").LastOutcome; out == nil || out.Kind != KindInitializationFailure {
		t.Fatalf("outcome = %+v", out)
	}
}

// Degraded mode (explicit opt-in): record the failure once per lifecycle
// version and proceed with the business turn.
func TestLifecycleInitDegraded(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		if prompt == "INIT" {
			return fakeTurnScript{failWith: errors.New("boom")}
		}
		return quickScript(prompt)
	}
	ctrl, store, rt := testLifecycle(t, initOnlyLifecycle(InitDegraded), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "hello")
	_, text, doneErr := drain(ch)
	if doneErr != nil || text != "reply: hello" {
		t.Fatalf("degraded mode must serve the business turn: text=%q err=%v", text, doneErr)
	}
	rec := store.Record("ctx1")
	if !rec.InitDegraded {
		t.Fatal("degraded marker missing")
	}
	// Not retried for the same lifecycle version.
	ch, _ = sess.Stream(context.Background(), "two")
	drain(ch)
	if n := len(rt.seen()); n != 3 { // INIT, hello, two
		t.Fatalf("degraded init must not retry per turn: %v", rt.seen())
	}
}

// Acceptance 4+5: work_timeout preserves partial events, cancels the runtime,
// and runs exactly one finalize turn in the same logical Session (same
// provider sessionID); success produces a typed finalized outcome.
func testSoftDeadline(t *testing.T, killOnCancel bool) {
	t.Helper()
	script := func(prompt string) fakeTurnScript {
		switch prompt {
		case "FINALIZE":
			return fakeTurnScript{reply: "final summary"}
		default:
			return fakeTurnScript{partial: "partial-work", block: true}
		}
	}
	ctrl, store, rt := testLifecycle(t, finalizeLifecycle(100*time.Millisecond, 5*time.Second), killOnCancel, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, err := sess.Stream(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	events, text, doneErr := drain(ch)
	if doneErr != nil {
		t.Fatalf("successful finalize must settle the turn without error: %v", doneErr)
	}
	if !strings.Contains(text, "partial-work") || !strings.Contains(text, "final summary") {
		t.Fatalf("turn text must carry partial evidence + finalize summary, got %q", text)
	}
	prompts := rt.seen()
	if len(prompts) != 2 || prompts[1] != "FINALIZE" {
		t.Fatalf("exactly one finalize turn must run: prompts=%v", prompts)
	}
	if killOnCancel && rt.resumes != 1 {
		t.Fatalf("killed provider must be resumed once for finalize: resumes=%d", rt.resumes)
	}

	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindRuntimeTimeout || !out.Finalized {
		t.Fatalf("outcome = %+v, want kind=runtime_timeout finalized=true", out)
	}
	if out.WorkDeadline == nil || out.GraceDeadline == nil {
		t.Fatalf("outcome must carry effective deadlines: %+v", out)
	}
	if out.LastMessage != "final summary" {
		t.Fatalf("last durable agent message = %q", out.LastMessage)
	}

	types := lifecycleEventTypes(events)
	wantSeq := []string{EvtTurnRunning, EvtTurnSoftDeadline, EvtTurnFinalizing, EvtTurnFinalized, EvtTurnOutcome}
	if fmt.Sprint(types) != fmt.Sprint(wantSeq) {
		t.Fatalf("lifecycle events = %v, want %v", types, wantSeq)
	}
	// All events correlate to one turn id.
	id := ""
	for _, le := range lifecycleEvents(events) {
		if le.TurnID == "" {
			t.Fatalf("event missing turn id: %+v", le)
		}
		if id == "" {
			id = le.TurnID
		} else if le.TurnID != id {
			t.Fatalf("events for one turn must share one turn id: %+v vs %q", le, id)
		}
	}
}

func TestLifecycleSoftDeadlineFinalizesCodexStyle(t *testing.T)  { testSoftDeadline(t, false) }
func TestLifecycleSoftDeadlineFinalizesClaudeStyle(t *testing.T) { testSoftDeadline(t, true) }

// Issue #47 regression: the provider returns a NIL error after the work
// deadline killed it (Codex-style graceful exit without a terminal protocol
// event, surfaced as EventTurnDone{Err: nil}). Deadline truth must take
// precedence over the nil provider error: the turn must NOT be recorded as
// completed; exactly one bounded finalize turn must settle it.
func TestLifecycleSoftDeadlineNilProviderErrorFinalizes(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		if prompt == "FINALIZE" {
			return fakeTurnScript{reply: "final summary"}
		}
		return fakeTurnScript{partial: "wip", nilOnCancel: true}
	}
	ctrl, store, rt := testLifecycle(t, finalizeLifecycle(80*time.Millisecond, 5*time.Second), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, err := sess.Stream(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	events, text, doneErr := drain(ch)
	if doneErr != nil {
		t.Fatalf("successful finalize must settle the turn without error: %v", doneErr)
	}
	if !strings.Contains(text, "final summary") {
		t.Fatalf("finalize summary missing from delivery: %q", text)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindRuntimeTimeout || !out.Finalized || out.Trigger != TriggerSoftDeadline {
		t.Fatalf("nil provider error at deadline must not record completed: %+v", out)
	}
	if out.LastMessage != "final summary" {
		t.Fatalf("last message = %q", out.LastMessage)
	}
	prompts := rt.seen()
	if len(prompts) != 2 || prompts[1] != "FINALIZE" {
		t.Fatalf("exactly one finalize turn must run: %v", prompts)
	}
	types := lifecycleEventTypes(events)
	if !containsStr(types, EvtTurnSoftDeadline) || !containsStr(types, EvtTurnFinalized) {
		t.Fatalf("soft-deadline finalize events missing: %v", types)
	}
}

// EOF without EventTurnDone (no deadline involved) is a truncated stream,
// never a silent completion.
func TestLifecycleMissingTerminalEventIsTruncated(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		return fakeTurnScript{reply: "partial reply", noDone: true}
	}
	ctrl, store, rt := testLifecycle(t, finalizeLifecycle(time.Hour, time.Second), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "work")
	_, text, doneErr := drain(ch)
	if !errors.Is(doneErr, ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", doneErr)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindProviderError || out.Finalized {
		t.Fatalf("truncated stream must not record completed: %+v", out)
	}
	if out.LastMessage != "partial reply" {
		t.Fatalf("partial evidence must be preserved: %q", out.LastMessage)
	}
	_ = text
	if prompts := rt.seen(); len(prompts) != 1 {
		t.Fatalf("finalize must not run on a truncated stream without deadline: %v", prompts)
	}
}

// EOF without EventTurnDone at the work deadline follows the soft-deadline
// path: deadline truth still triggers exactly one bounded finalize turn.
func TestLifecycleMissingTerminalEventAtDeadlineFinalizes(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		if prompt == "FINALIZE" {
			return fakeTurnScript{reply: "recovered summary"}
		}
		return fakeTurnScript{block: true, noDone: true}
	}
	ctrl, store, rt := testLifecycle(t, finalizeLifecycle(60*time.Millisecond, 5*time.Second), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "work")
	_, text, doneErr := drain(ch)
	if doneErr != nil {
		t.Fatalf("finalize after truncation-at-deadline must settle cleanly: %v", doneErr)
	}
	if !strings.Contains(text, "recovered summary") {
		t.Fatalf("finalize summary missing: %q", text)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindRuntimeTimeout || !out.Finalized {
		t.Fatalf("outcome = %+v, want kind=runtime_timeout finalized=true", out)
	}
	if prompts := rt.seen(); len(prompts) != 2 || prompts[1] != "FINALIZE" {
		t.Fatalf("exactly one finalize turn must run: %v", prompts)
	}
}

// Tool-only delivery: a turn that delivered through tools and produced no
// assistant text is a legitimate completion — no blanket nonempty-text rule.
func TestLifecycleToolOnlyDeliveryCompletes(t *testing.T) {
	ctrl, store, _ := testLifecycle(t, finalizeLifecycle(time.Hour, time.Second), false,
		func(string) fakeTurnScript { return fakeTurnScript{toolOnly: true} })
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "deliver via tool")
	events, text, doneErr := drain(ch)
	if doneErr != nil {
		t.Fatalf("tool-only delivery must complete: %v", doneErr)
	}
	if text != "" {
		t.Fatalf("no text expected, got %q", text)
	}
	sawTool := false
	for _, ev := range events {
		if _, ok := ev.(EventToolUse); ok {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatal("tool-use event must be forwarded")
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindCompleted || out.LastMessage != "" {
		t.Fatalf("tool-only delivery is a valid completion: %+v", out)
	}
}

// The finalize turn is held to the same rule: a nil provider error after the
// grace deadline fired is the hard-terminate boundary, not a finalization.
func TestLifecycleFinalizeNilProviderErrorAtGraceDeadline(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		// Business blocks until the work deadline (canceled-style); the
		// finalize turn then hangs and returns nil after the grace kill.
		if prompt == "FINALIZE" {
			return fakeTurnScript{nilOnCancel: true}
		}
		return fakeTurnScript{block: true}
	}
	ctrl, store, _ := testLifecycle(t, finalizeLifecycle(60*time.Millisecond, 120*time.Millisecond), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "work")
	_, _, doneErr := drain(ch)
	if !errors.Is(doneErr, ErrFinalizeTimeout) {
		t.Fatalf("err = %v, want ErrFinalizeTimeout", doneErr)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindFinalizeTimeout || out.Finalized {
		t.Fatalf("nil finalize error at grace deadline must not record finalized: %+v", out)
	}
}

// A finalize stream that closes without EventTurnDone (before grace expires)
// is a finalization failure with the typed truncation cause, never finalized.
func TestLifecycleFinalizeMissingTerminalEvent(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		if prompt == "FINALIZE" {
			return fakeTurnScript{reply: "half a summary", noDone: true}
		}
		return fakeTurnScript{block: true}
	}
	ctrl, store, _ := testLifecycle(t, finalizeLifecycle(60*time.Millisecond, 5*time.Second), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "work")
	_, _, doneErr := drain(ch)
	if !errors.Is(doneErr, ErrFinalizationFailed) || !errors.Is(doneErr, ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrFinalizationFailed wrapping ErrStreamTruncated", doneErr)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindProviderError || out.Finalized {
		t.Fatalf("truncated finalize stream must not record finalized: %+v", out)
	}
}

// Acceptance 6: finalize exceeding grace_timeout hard-terminates with a
// distinct finalize-timeout reason.
func TestLifecycleFinalizeGraceTimeout(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		return fakeTurnScript{block: true} // business AND finalize both hang
	}
	ctrl, store, rt := testLifecycle(t, finalizeLifecycle(60*time.Millisecond, 120*time.Millisecond), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	start := time.Now()
	ch, _ := sess.Stream(context.Background(), "work")
	_, _, doneErr := drain(ch)
	if !errors.Is(doneErr, ErrFinalizeTimeout) {
		t.Fatalf("err = %v, want ErrFinalizeTimeout", doneErr)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("grace deadline not enforced: %v", elapsed)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindFinalizeTimeout || out.Finalized {
		t.Fatalf("outcome = %+v, want kind=finalize_timeout finalized=false", out)
	}
	types := lifecycleEventTypes(streamEventsOf(store, "ctx1"))
	_ = types
	if prompts := rt.seen(); len(prompts) != 2 {
		t.Fatalf("exactly one finalize attempt expected: %v", prompts)
	}
}

func streamEventsOf(store *LifecycleStore, ctxID string) []Event {
	var out []Event
	for _, ev := range store.Record(ctxID).Events {
		out = append(out, EventLifecycle{Event: ev.Type, TurnID: ev.TurnID, Phase: ev.Phase, Kind: ev.Kind})
	}
	return out
}

// Acceptance 7: provider failure makes finalize impossible — distinct terminal
// result, no fabricated final output, no finalize attempt.
func TestLifecycleProviderFailureNoFinalize(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		return fakeTurnScript{failWith: errors.New("provider exploded")}
	}
	ctrl, store, rt := testLifecycle(t, finalizeLifecycle(5*time.Second, time.Second), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "work")
	_, text, doneErr := drain(ch)
	if doneErr == nil || !strings.Contains(doneErr.Error(), "provider exploded") {
		t.Fatalf("err = %v", doneErr)
	}
	if text != "" {
		t.Fatalf("no output may be fabricated on provider failure: %q", text)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindProviderError || out.Finalized {
		t.Fatalf("outcome = %+v", out)
	}
	if prompts := rt.seen(); len(prompts) != 1 {
		t.Fatalf("finalize must not run on provider failure: %v", prompts)
	}
}

// Graceful stop (explicit cancel) while a turn is active triggers bounded
// finalization on a detached context.
func TestLifecycleGracefulStopFinalizes(t *testing.T) {
	script := func(prompt string) fakeTurnScript {
		if prompt == "FINALIZE" {
			return fakeTurnScript{reply: "stopped summary"}
		}
		return fakeTurnScript{partial: "wip", block: true}
	}
	ctrl, store, _ := testLifecycle(t, finalizeLifecycle(time.Hour, 5*time.Second), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := sess.Stream(ctx, "work")
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	_, text, doneErr := drain(ch)
	if doneErr != nil {
		t.Fatalf("graceful stop + finalize must settle cleanly: %v", doneErr)
	}
	if !strings.Contains(text, "stopped summary") {
		t.Fatalf("finalize summary missing: %q", text)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindCanceled || !out.Finalized || out.Trigger != TriggerGracefulStop {
		t.Fatalf("outcome = %+v", out)
	}
}

// Acceptance 9: a user turn arriving during finalization queues deterministically
// and never interleaves with the finalize turn.
func TestLifecycleUserTurnQueuesDuringFinalizeShort(t *testing.T) {
	// Business hangs -> soft deadline at 60ms; finalize hangs -> grace 300ms
	// expires; the queued second turn must start only after the first turn
	// fully settles, and prompts must arrive at the provider serially.
	script := func(prompt string) fakeTurnScript {
		if prompt == "second" {
			return fakeTurnScript{reply: "second reply"}
		}
		return fakeTurnScript{block: true}
	}
	ctrl, store, rt := testLifecycle(t, finalizeLifecycle(60*time.Millisecond, 300*time.Millisecond), false, script)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")

	first, _ := sess.Stream(context.Background(), "work")
	// Wait for finalizing.
	deadline := time.Now().Add(3 * time.Second)
	for store.Record("ctx1").Phase != PhaseFinalizing && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ch, _ := sess.Stream(context.Background(), "second")
		drain(ch)
	}()
	time.Sleep(50 * time.Millisecond) // second turn is queued while finalize runs
	if prompts := rt.seen(); containsStr(prompts, "second") {
		t.Fatalf("queued turn interleaved with finalize: %v", prompts)
	}
	drain(first)
	wg.Wait()
	prompts := rt.seen()
	// Order: work, FINALIZE, second — strictly serial.
	if len(prompts) != 3 || prompts[0] != "work" || prompts[1] != "FINALIZE" || prompts[2] != "second" {
		t.Fatalf("turns must not interleave with finalize: %v", prompts)
	}
	out := store.Record("ctx1").LastOutcome
	if out == nil {
		t.Fatal("second turn outcome missing")
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// Disabled lifecycle: the controller passes sessions through untouched.
func TestLifecycleDisabledPassthrough(t *testing.T) {
	r, err := ResolveLifecycle(LifecycleConfig{})
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{sessionID: "s1", script: quickScript}
	open := WrapOpenSession(r, NewLifecycleStore(t.TempDir()+"/l.json"), rt.open)
	sess, err := open(context.Background(), "ctx1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sess.(*fakeSession); !ok {
		t.Fatalf("disabled lifecycle must return the raw session, got %T", sess)
	}
}

// Terminal outcome carries session + turn identity, phase, timestamps.
func TestLifecycleOutcomeIdentityAndTimestamps(t *testing.T) {
	ctrl, store, _ := testLifecycle(t, finalizeLifecycle(time.Hour, time.Second), false, quickScript)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "work")
	drain(ch)
	out := store.Record("ctx1").LastOutcome
	if out == nil || out.Kind != KindCompleted {
		t.Fatalf("outcome = %+v", out)
	}
	if out.SessionID != "ctx1" || out.TurnID == "" || out.Phase != PhaseIdle {
		t.Fatalf("outcome identity wrong: %+v", out)
	}
	if out.StartedAt.IsZero() || out.FinishedAt.Before(out.StartedAt) {
		t.Fatalf("timestamps wrong: %+v", out)
	}
}

// Acceptance 8: a reused keyed Session accumulates lifecycle events across
// turns, and every event correlates to the turn that produced it — never to
// historical turns.
func TestLifecycleEventsCorrelatePerTurnOnReusedSession(t *testing.T) {
	ctrl, store, _ := testLifecycle(t, finalizeLifecycle(time.Hour, time.Second), false, quickScript)
	sess, _ := ctrl.OpenSession(context.Background(), "ctx1")
	ch, _ := sess.Stream(context.Background(), "one")
	drain(ch)
	ch, _ = sess.Stream(context.Background(), "two")
	drain(ch)

	rec := store.Record("ctx1")
	var outcomes []LifecycleEvent
	for _, ev := range rec.Events {
		if ev.Type == EvtTurnOutcome {
			outcomes = append(outcomes, ev)
		}
	}
	if len(outcomes) != 2 {
		t.Fatalf("want 2 terminal outcomes, got %d", len(outcomes))
	}
	if outcomes[0].TurnID == outcomes[1].TurnID {
		t.Fatal("distinct turns must have distinct turn ids")
	}
	// Every turn-scoped event belongs to one of the two known turns.
	valid := map[string]bool{outcomes[0].TurnID: true, outcomes[1].TurnID: true}
	for _, ev := range rec.Events {
		if ev.TurnID != "" && !valid[ev.TurnID] {
			t.Fatalf("event correlates to unknown turn: %+v", ev)
		}
	}
	// The latest outcome reflects the CURRENT turn, not history.
	if rec.LastOutcome.TurnID != outcomes[1].TurnID || rec.LastOutcome.Kind != KindCompleted {
		t.Fatalf("last outcome must be the current turn's: %+v", rec.LastOutcome)
	}
}

// Acceptance 10: idle scale-to-zero performs resource cleanup only — the
// lifecycle controller runs no semantic finalize turn without an active
// business turn. (Structural: finalization is only reachable from
// runBusiness; this test pins that a fresh/idle session emits no lifecycle
// events at all.)
func TestLifecycleIdleSessionEmitsNothing(t *testing.T) {
	_, store, rt := testLifecycle(t, finalizeLifecycle(time.Hour, time.Second), false, quickScript)
	if rec := store.Record("ctx-never-used"); len(rec.Events) != 0 || rec.LastOutcome != nil {
		t.Fatalf("idle session must have no lifecycle activity: %+v", rec)
	}
	if prompts := rt.seen(); len(prompts) != 0 {
		t.Fatalf("no provider turn may run without a business turn: %v", prompts)
	}
}

// Delegation surface: Turn aggregates text, SessionID/IsHealthy/Close reach
// the last opened provider session, and WrapOpenSession installs the
// controller only when the config enables a block.
func TestLifecycleSessionDelegation(t *testing.T) {
	rt := &fakeRuntime{sessionID: "prov-42", script: quickScript}
	store := NewLifecycleStore("") // persistence disabled branch
	cfg := finalizeLifecycle(time.Hour, time.Second)
	open := WrapOpenSession(cfg, store, rt.open)

	sess, err := open(context.Background(), "ctx1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sess.(*lifecycleSession); !ok {
		t.Fatalf("enabled lifecycle must wrap the session, got %T", sess)
	}
	if sess.SessionID() != "" {
		t.Fatalf("no provider session opened yet, SessionID=%q", sess.SessionID())
	}
	if !sess.IsHealthy() {
		t.Fatal("unused session must report healthy")
	}
	text, err := sess.Turn(context.Background(), "hi")
	if err != nil || text != "reply: hi" {
		t.Fatalf("Turn = %q, %v", text, err)
	}
	if sess.SessionID() != "prov-42" {
		t.Fatalf("SessionID must delegate after open: %q", sess.SessionID())
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
}
