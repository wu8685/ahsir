package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/cmagateway/cma"
)

func TestAgentReturnsSnapshotNotSharedPointer(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := &cma.Agent{ID: "agent-1", Version: 1, Metadata: map[string]string{"mode": "original"}}
	if err := s.PutAgentVersion(a); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := s.Agent(a.ID, a.Version)
	if !ok {
		t.Fatal("agent not found")
	}
	if _, ok := s.ArchiveAgent(a.ID, time.Now().UTC()); !ok {
		t.Fatal("archive agent")
	}
	if snapshot.ArchivedAt != nil {
		t.Fatal("previous Agent result changed after archive; Store returned a shared pointer")
	}
	snapshot.Metadata["mode"] = "caller-mutated"
	again, _ := s.Agent(a.ID, a.Version)
	if again.Metadata["mode"] != "original" {
		t.Fatalf("caller mutation leaked into Store: metadata=%v", again.Metadata)
	}
}

func TestAgentSnapshotConcurrentArchive(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := &cma.Agent{ID: "agent-race", Version: 1, Metadata: map[string]string{"mode": "stable"}}
	if err := s.PutAgentVersion(a); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			got, _ := s.Agent(a.ID, a.Version)
			_ = got.ArchivedAt
			_ = got.Metadata["mode"]
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _ = s.ArchiveAgent(a.ID, time.Now().UTC())
		}
	}()
	wg.Wait()
}

func outcomeTestSession(t *testing.T, s *Store) *SessionRecord {
	t.Helper()
	now := time.Now().UTC()
	rec := &SessionRecord{
		Session: &cma.Session{
			Type: "session", ID: "sesn_outcome", Status: cma.StatusIdle,
			Resources: []any{}, VaultIDs: []string{}, Metadata: map[string]string{},
			CreatedAt: now, UpdatedAt: now,
		},
		AhsirName: "cma-x-v1",
		ContextID: "ctx_outcome",
	}
	if err := s.PutSession(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestBeginOutcomeConflictsWhileActive(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := outcomeTestSession(t, s)

	oc := cma.OutcomeEvaluation{OutcomeID: "outc_1", Result: cma.OutcomeStatePending, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	ok, err := s.BeginOutcome(rec, oc)
	if err != nil || !ok {
		t.Fatalf("first BeginOutcome = %v, %v; want true, nil", ok, err)
	}
	for _, active := range []string{cma.OutcomeStatePending, cma.OutcomeStateRunning, cma.OutcomeStateEvaluating} {
		if err := s.UpdateOutcome(rec, "outc_1", func(o *cma.OutcomeEvaluation) { o.Result = active }); err != nil {
			t.Fatal(err)
		}
		ok, err = s.BeginOutcome(rec, cma.OutcomeEvaluation{OutcomeID: "outc_2", Result: cma.OutcomeStatePending})
		if err != nil || ok {
			t.Fatalf("BeginOutcome during %s = %v, %v; want false, nil", active, ok, err)
		}
	}
	// Terminal outcomes free the slot.
	if err := s.UpdateOutcome(rec, "outc_1", func(o *cma.OutcomeEvaluation) { o.Result = cma.OutcomeResultSatisfied }); err != nil {
		t.Fatal(err)
	}
	ok, err = s.BeginOutcome(rec, cma.OutcomeEvaluation{OutcomeID: "outc_2", Result: cma.OutcomeStatePending})
	if err != nil || !ok {
		t.Fatalf("BeginOutcome after terminal = %v, %v; want true, nil", ok, err)
	}
	if got := rec.OutcomeEvaluations(); len(got) != 2 {
		t.Fatalf("outcome_evaluations = %d, want chained history of 2", len(got))
	}
}

func TestOutcomeStateSurvivesStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := outcomeTestSession(t, s)
	now := time.Now().UTC()
	if _, err := s.BeginOutcome(rec, cma.OutcomeEvaluation{
		OutcomeID: "outc_p", Description: "d", Rubric: "# R", MaxIterations: 3,
		Result: cma.OutcomeStateRunning, Iteration: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOutcome(rec, "outc_p", func(o *cma.OutcomeEvaluation) {
		o.Criteria = []cma.OutcomeCriterion{{Criterion: "c1", Passed: true}}
	}); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	rec2, ok := restarted.Session(rec.Session.ID)
	if !ok {
		t.Fatal("session missing after restart")
	}
	got := rec2.OutcomeEvaluations()
	if len(got) != 1 || got[0].OutcomeID != "outc_p" || got[0].Result != cma.OutcomeStateRunning ||
		got[0].Iteration != 1 || len(got[0].Criteria) != 1 || got[0].Rubric != "# R" {
		t.Fatalf("outcome state after restart = %+v", got)
	}
}

func TestUpdateOutcomeUnknownID(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := outcomeTestSession(t, s)
	if err := s.UpdateOutcome(rec, "outc_missing", func(o *cma.OutcomeEvaluation) {}); err == nil {
		t.Fatal("UpdateOutcome for an unknown outcome_id succeeded, want error")
	}
}

func TestListSessionsReturnsSnapshots(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := outcomeTestSession(t, s)
	if _, err := s.BeginOutcome(rec, cma.OutcomeEvaluation{OutcomeID: "outc_s", Result: cma.OutcomeStatePending}); err != nil {
		t.Fatal(err)
	}
	list := s.ListSessions()
	if len(list) != 1 {
		t.Fatalf("ListSessions = %d sessions, want 1", len(list))
	}
	if list[0] == rec.Session {
		t.Fatal("ListSessions handed out the live *cma.Session; want a snapshot")
	}
	// Mutating a listed session must not leak into the store.
	list[0].Status = "hacked"
	list[0].OutcomeEvaluations = append(list[0].OutcomeEvaluations, cma.OutcomeEvaluation{OutcomeID: "outc_forged"})
	if got := rec.OutcomeEvaluations(); len(got) != 1 {
		t.Fatalf("caller mutation leaked into Store: outcome_evaluations = %d, want 1", len(got))
	}
	if again := s.ListSessions()[0]; again.Status == "hacked" {
		t.Fatalf("caller mutation leaked into Store: status = %q", again.Status)
	}
}

// TestListSessionsConcurrentOutcomeUpdates hammers ListSessions and marshals
// every returned session (as GET /v1/sessions does) while the outcome loop
// replaces OutcomeEvaluations and status transitions run. With -race this
// fails if ListSessions hands out live *cma.Session pointers.
func TestListSessionsConcurrentOutcomeUpdates(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := outcomeTestSession(t, s)
	if _, err := s.BeginOutcome(rec, cma.OutcomeEvaluation{OutcomeID: "outc_race", Result: cma.OutcomeStatePending}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // writer: the outcome loop
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := s.UpdateOutcome(rec, "outc_race", func(o *cma.OutcomeEvaluation) {
				o.Result = cma.OutcomeStateRunning
				o.Iteration = i
			}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() { // writer: status transitions
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := s.SetSessionStatus(rec, cma.StatusIdle, time.Now().UTC()); err != nil {
				t.Error(err)
				return
			}
		}
		close(done)
	}()
	go func() { // reader: GET /v1/sessions
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			for _, sess := range s.ListSessions() {
				if _, err := json.Marshal(sess); err != nil {
					t.Errorf("marshal listed session: %v", err)
				}
			}
		}
	}()
	wg.Wait()
}

// TestBeginOutcomeWithCancelRegistersCancelAtomically covers the
// interrupt-during-define window: the cancel func is registered under the same
// record lock as the outcome append, so an interrupt landing before the loop
// is enqueued still cancels it.
func TestBeginOutcomeWithCancelRegistersCancelAtomically(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := outcomeTestSession(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	ok, err := s.BeginOutcomeWithCancel(rec, cma.OutcomeEvaluation{OutcomeID: "outc_c", Result: cma.OutcomeStatePending}, cancel)
	if err != nil || !ok {
		t.Fatalf("BeginOutcomeWithCancel = %v, %v; want true, nil", ok, err)
	}
	rec.CancelOutcome()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("CancelOutcome did not cancel the context registered at begin")
	}

	// On conflict the cancel func must not be registered (the active outcome
	// keeps its own) — begin a second outcome while the first is active.
	if err := s.UpdateOutcome(rec, "outc_c", func(o *cma.OutcomeEvaluation) { o.Result = cma.OutcomeStateRunning }); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ok, err = s.BeginOutcomeWithCancel(rec, cma.OutcomeEvaluation{OutcomeID: "outc_c2", Result: cma.OutcomeStatePending}, cancel2)
	if err != nil || ok {
		t.Fatalf("BeginOutcomeWithCancel during active = %v, %v; want false, nil", ok, err)
	}
	select {
	case <-ctx2.Done():
		t.Fatal("conflicting BeginOutcomeWithCancel consumed the caller's cancel func")
	default:
	}
}
