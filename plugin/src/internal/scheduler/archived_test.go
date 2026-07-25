package scheduler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/registry"
	"github.com/wu8685/ahsir/internal/wrapper"
)

// newArchivedTestScheduler builds a Scheduler whose config has a real on-disk
// path, so ManagedAgentsDir() resolves and archived-agent discovery can scan
// .ahsir/agents/*. Returns the scheduler and the config dir.
func newArchivedTestScheduler(t *testing.T) (*Scheduler, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: 0},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	cfg.path = filepath.Join(dir, "ahsir.yaml")
	return New(cfg), dir
}

// seedArchivedTranscript writes a completed turn into the managed workspace of
// agentName, stamped at ts, mirroring what a live agent leaves behind on disk.
func seedArchivedTranscript(t *testing.T, sch *Scheduler, agentName, contextID, user, reply string, ts time.Time) {
	t.Helper()
	ws := sch.cfg.ManagedAgentWorkspace(agentName)
	if ws == "" {
		t.Fatal("managed workspace is empty; config has no path")
	}
	store := wrapper.NewTranscriptStore(ws)
	if err := store.Append(contextID, wrapper.TranscriptTurn{
		TS:       ts,
		UserText: user,
		Reply:    reply,
		Status:   "completed",
	}); err != nil {
		t.Fatalf("seed transcript for %s: %v", agentName, err)
	}
}

func TestSafeManagedName(t *testing.T) {
	ok := []string{"agent", "my-agent", "cma-abc123", "a.b"}
	bad := []string{"", ".", "..", "../etc", "a/b", `a\b`, "foo/../bar", "..\\x"}
	for _, n := range ok {
		if !safeManagedName(n) {
			t.Errorf("safeManagedName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if safeManagedName(n) {
			t.Errorf("safeManagedName(%q) = true, want false", n)
		}
	}
}

func TestArchivedAgentsDiscoversOfflineAgent(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	now := time.Now()
	seedArchivedTranscript(t, sch, "ghost", "ctx-1", "first question", "reply-1", now.Add(-2*time.Hour))
	seedArchivedTranscript(t, sch, "ghost", "ctx-1", "second question", "reply-2", now.Add(-1*time.Hour))

	agents, err := sch.ArchivedAgents()
	if err != nil {
		t.Fatalf("ArchivedAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("want 1 archived agent, got %d: %+v", len(agents), agents)
	}
	a := agents[0]
	if a.Name != "ghost" {
		t.Fatalf("archived name = %q, want ghost", a.Name)
	}
	if len(a.Contexts) != 1 {
		t.Fatalf("want 1 context, got %d", len(a.Contexts))
	}
	c := a.Contexts[0]
	if c.ContextID != "ctx-1" {
		t.Errorf("contextId = %q", c.ContextID)
	}
	if c.Turns != 2 {
		t.Errorf("turns = %d, want 2", c.Turns)
	}
	if c.Title != "first question" {
		t.Errorf("title = %q, want first userText", c.Title)
	}
	if c.LastActivity == "" {
		t.Error("lastActivity should be populated")
	}
}

func TestArchivedAgentsExcludesLiveAndDesired(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	now := time.Now()
	seedArchivedTranscript(t, sch, "live", "c", "q", "r", now.Add(-time.Hour))
	seedArchivedTranscript(t, sch, "desired", "c", "q", "r", now.Add(-time.Hour))
	seedArchivedTranscript(t, sch, "gone", "c", "q", "r", now.Add(-time.Hour))

	// "live" is registered; "desired" is in the desired set (mid-restart).
	sch.registry.Register(&a2a.AgentCard{Name: "live", URL: "http://127.0.0.1:1"})
	sch.mu.Lock()
	sch.desired["desired"] = AgentConfig{Name: "desired"}
	sch.mu.Unlock()

	agents, err := sch.ArchivedAgents()
	if err != nil {
		t.Fatalf("ArchivedAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "gone" {
		t.Fatalf("want only [gone], got %+v", agents)
	}
}

func TestArchivedAgentsRespectsRetention(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	now := time.Now()
	// Only turn is past the 30-day window → the agent has nothing viewable and
	// must not surface.
	seedArchivedTranscript(t, sch, "stale", "c", "q", "r", now.Add(-40*24*time.Hour))

	agents, err := sch.ArchivedAgents()
	if err != nil {
		t.Fatalf("ArchivedAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("expired-only agent should not appear, got %+v", agents)
	}
}

func TestArchivedAgentsNoManagedDir(t *testing.T) {
	// In-memory config (no path) has no managed dir — must be a clean empty
	// result, not an error.
	cfg := &Config{}
	sch := New(cfg)
	agents, err := sch.ArchivedAgents()
	if err != nil {
		t.Fatalf("ArchivedAgents with no path: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("want empty, got %+v", agents)
	}
}

func TestArchivedAgentHistory(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	now := time.Now()
	seedArchivedTranscript(t, sch, "ghost", "ctx-1", "the question", "the reply", now.Add(-time.Hour))

	turns, err := sch.ArchivedAgentHistory("ghost", "ctx-1")
	if err != nil {
		t.Fatalf("ArchivedAgentHistory: %v", err)
	}
	if len(turns) != 1 || turns[0].UserText != "the question" || turns[0].Reply != "the reply" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
}

func TestArchivedAgentHistoryErrors(t *testing.T) {
	sch, _ := newArchivedTestScheduler(t)
	now := time.Now()
	seedArchivedTranscript(t, sch, "ghost", "ctx-1", "q", "r", now.Add(-time.Hour))
	seedArchivedTranscript(t, sch, "stale", "old", "q", "r", now.Add(-40*24*time.Hour))

	cases := []struct {
		name, agent, ctx string
	}{
		{"unknown agent", "nope", "ctx-1"},
		{"unknown context", "ghost", "missing"},
		{"path traversal", "../../etc", "ctx-1"},
		{"past retention", "stale", "old"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sch.ArchivedAgentHistory(tc.agent, tc.ctx); err == nil {
				t.Fatalf("expected error for %s/%s", tc.agent, tc.ctx)
			}
		})
	}
}

// --- gateway integration -------------------------------------------------

func newArchivedGateway(t *testing.T) (*Scheduler, string) {
	t.Helper()
	sch, _ := newArchivedTestScheduler(t)
	gw := newGatewayHandler(sch, registry.NewHTTPHandler(sch.Registry()))
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return sch, srv.URL
}

func getJSON(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestGatewayArchivedAgentsEndpoint(t *testing.T) {
	sch, url := newArchivedGateway(t)
	seedArchivedTranscript(t, sch, "ghost", "ctx-1", "hello", "hi", time.Now().Add(-time.Hour))

	status, body := getJSON(t, url+"/archived-agents")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	var agents []ArchivedAgent
	if err := json.Unmarshal(body, &agents); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(agents) != 1 || agents[0].Name != "ghost" {
		t.Fatalf("unexpected archived agents: %+v", agents)
	}
}

func TestGatewayArchivedAgentsEndpointEmpty(t *testing.T) {
	_, url := newArchivedGateway(t)
	status, body := getJSON(t, url+"/archived-agents")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	// Must be a JSON array, never null — the SPA iterates the result.
	if string(body) != "[]\n" && string(body) != "[]" {
		t.Fatalf("empty archived list should be [], got %q", body)
	}
}

func TestGatewayHistoryServesOfflineAgent(t *testing.T) {
	sch, url := newArchivedGateway(t)
	// Agent is NOT registered — only its on-disk transcript exists.
	seedArchivedTranscript(t, sch, "ghost", "ctx-1", "the question", "the reply", time.Now().Add(-time.Hour))

	status, body := getJSON(t, url+"/agents/ghost/history/ctx-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body=%s", status, body)
	}
	var turns []wrapper.TranscriptTurn
	if err := json.Unmarshal(body, &turns); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(turns) != 1 || turns[0].Reply != "the reply" {
		t.Fatalf("unexpected turns: %+v", turns)
	}
}

func TestGatewayHistoryOfflineAgentNotFound(t *testing.T) {
	_, url := newArchivedGateway(t)
	status, _ := getJSON(t, url+"/agents/ghost/history/ctx-1")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}
