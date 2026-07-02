package scheduler

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/wrapper"
)

// newArchivedTestScheduler builds a scheduler whose config points at a temp
// dir, so ManagedAgentsDir()/ManagedAgentWorkspace() resolve to real paths we
// can seed with on-disk transcripts.
func newArchivedTestScheduler(t *testing.T) (*Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: 0},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.path = filepath.Join(dir, "ahsir.yaml")
	cfg.nextPort = cfg.PortRange.Start
	return New(cfg), dir
}

// seedTranscript writes a completed turn into the managed workspace of an
// agent, creating the on-disk transcript a deleted agent leaves behind.
func seedTranscript(t *testing.T, sch *Scheduler, agent, contextID, userText, reply string, ts time.Time) {
	t.Helper()
	ws := sch.cfg.ManagedAgentWorkspace(agent)
	if ws == "" {
		t.Fatal("ManagedAgentWorkspace returned empty (config has no path)")
	}
	store := wrapper.NewTranscriptStore(ws)
	if err := store.Append(contextID, wrapper.TranscriptTurn{
		TS:       ts,
		UserText: userText,
		Reply:    reply,
		Status:   "completed",
	}); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
}

func TestArchivedAgentsListsOfflineWorkspaces(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	now := time.Now()
	seedTranscript(t, sch, "ghost", "ctx-1", "hello there", "hi back", now)

	agents, err := sch.ArchivedAgents()
	if err != nil {
		t.Fatalf("ArchivedAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("want 1 archived agent, got %d", len(agents))
	}
	a := agents[0]
	if a.Name != "ghost" {
		t.Fatalf("name = %q, want ghost", a.Name)
	}
	if len(a.Contexts) != 1 {
		t.Fatalf("want 1 context, got %d", len(a.Contexts))
	}
	c := a.Contexts[0]
	if c.ContextID != "ctx-1" || c.Title != "hello there" || c.Turns != 1 {
		t.Fatalf("unexpected context summary: %+v", c)
	}
}

func TestArchivedAgentsExcludesLiveAgents(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	seedTranscript(t, sch, "live-one", "ctx-1", "hi", "yo", time.Now())

	// Register it so isAgentLive reports true; it must then drop out of the list.
	sch.registry.Register(&a2a.AgentCard{Name: "live-one", URL: "http://127.0.0.1:9999/", Version: "1.0.0"})

	agents, err := sch.ArchivedAgents()
	if err != nil {
		t.Fatalf("ArchivedAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("live agent should be excluded from archived, got %+v", agents)
	}
}

func TestArchivedAgentsRespectsRetention(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	old := time.Now().Add(-wrapper.RetentionWindow() - time.Hour)
	seedTranscript(t, sch, "stale", "ctx-old", "ancient", "reply", old)

	agents, err := sch.ArchivedAgents()
	if err != nil {
		t.Fatalf("ArchivedAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("context past retention should be pruned from listing, got %+v", agents)
	}
}

func TestAgentHistoryFallsBackToDiskForOfflineAgent(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	now := time.Now()
	seedTranscript(t, sch, "ghost", "ctx-1", "question", "answer", now)

	// The agent is not registered/running, so AgentHistory must read from disk
	// rather than erroring.
	turns, err := sch.AgentHistory("ghost", "ctx-1")
	if err != nil {
		t.Fatalf("AgentHistory (offline): %v", err)
	}
	if len(turns) != 1 || turns[0].Reply != "answer" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
}

func TestAgentHistoryUnknownAgentStillNotFound(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	if _, err := sch.AgentHistory("nobody", "ctx-x"); err == nil {
		t.Fatal("expected error for unknown offline agent with no workspace")
	}
}
