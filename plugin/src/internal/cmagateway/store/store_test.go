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
