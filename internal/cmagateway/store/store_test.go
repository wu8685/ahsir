package store

import (
	"path/filepath"
	"strings"
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

func TestReconcileInterruptedSessions_SettlesPersistedRunningExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC)
	rec := &SessionRecord{
		Session: &cma.Session{
			Type: "session", ID: "sesn_interrupted", Status: cma.StatusRunning,
			Metadata: map[string]string{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		},
		AhsirName: "cma-agent-v1",
		ContextID: "ctx_interrupted",
	}
	if err := st.PutSession(rec); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	count, err := restarted.ReconcileInterruptedSessions(now)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reconciled=%d, want 1", count)
	}
	got, ok := restarted.Session(rec.Session.ID)
	if !ok {
		t.Fatal("session missing after reconciliation")
	}
	if got.Status() != cma.StatusTerminated {
		t.Fatalf("status=%q, want terminated", got.Status())
	}
	events := got.Snapshot()
	if len(events) != 2 || events[0].Type != cma.EvtSessionError || events[1].Type != cma.EvtSessionStatusTerminate {
		t.Fatalf("events=%+v, want error then terminated", events)
	}
	if events[0].Error == nil || !strings.Contains(events[0].Error.Message, "runtime_interrupted") || events[0].Error.RetryStatus.Type != "terminal" {
		t.Fatalf("unexpected recovery error: %+v", events[0].Error)
	}

	// Re-opening and reconciling again must not duplicate terminal events.
	again, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	count, err = again.ReconcileInterruptedSessions(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("second reconciliation count=%d, want 0", count)
	}
	againRec, _ := again.Session(rec.Session.ID)
	if got := len(againRec.Snapshot()); got != 2 {
		t.Fatalf("second reconciliation duplicated events: len=%d", got)
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
