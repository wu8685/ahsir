package store

import (
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
