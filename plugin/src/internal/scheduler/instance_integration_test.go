package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/wrapper"
)

// writeInstanceTestCard drops a minimal agent-card.yaml in a base workspace so
// scaffoldInstanceWorkspace has something to copy into inst-<n>. The fake agent
// process never Loads it — only the scheduler's scaffold reads/copies the bytes.
func writeInstanceTestCard(t *testing.T, workspace string) {
	t.Helper()
	dir := filepath.Join(workspace, ".a2a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	card := "name: worker\nruntime:\n  command: echo\n"
	if err := os.WriteFile(filepath.Join(dir, "agent-card.yaml"), []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSchedulerInstancePoolSpawnsIsolatedInstances: a pooled agent (Instances>1)
// spreads concurrent distinct sessions onto separate ahsir-agent processes, each
// with its own isolated inst-<n> workspace and a scaffolded card, while a
// contextID stays pinned to its original instance (resume affinity). This is the
// end-to-end core of issue #18.
func TestSchedulerInstancePoolSpawnsIsolatedInstances(t *testing.T) {
	dir := t.TempDir()
	writeInstanceTestCard(t, dir)
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	// Large idle window so the fakes stay resident serving /healthz for the test.
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 600000, wrapper.IdleStopExitCode))
	sch.cfg.Agents[0].Instances = 3

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, logPath, 1, testLifecycleDeadline) // base instance up

	// First session lands on the already-running base (ordinal 0) — no new proc.
	tA, relA, err := sch.resolveInstance("worker", "ctx-A")
	if err != nil {
		t.Fatalf("resolve ctx-A: %v", err)
	}
	if tA != "worker" {
		t.Fatalf("ctx-A target = %q, want worker", tA)
	}

	// Second concurrent distinct session (base busy) spawns worker#1 into inst-1.
	tB, relB, err := sch.resolveInstance("worker", "ctx-B")
	if err != nil {
		t.Fatalf("resolve ctx-B: %v", err)
	}
	if tB != "worker#1" {
		t.Fatalf("ctx-B target = %q, want worker#1", tB)
	}

	_ = waitForLines(t, logPath, 2, testLifecycleDeadline)

	// The instance was spawned against its isolated inst-1 workspace.
	instWS := filepath.Join(dir, "inst-1")
	foundWS := false
	for _, l := range readLines(t, logPath) {
		if strings.Contains(l, instWS) {
			foundWS = true
		}
	}
	if !foundWS {
		t.Fatalf("no spawn used isolated workspace %q; log=%v", instWS, readLines(t, logPath))
	}

	// ...and its card was scaffolded into that workspace so it can boot.
	if _, err := os.Stat(filepath.Join(instWS, ".a2a", "agent-card.yaml")); err != nil {
		t.Fatalf("instance card not scaffolded: %v", err)
	}

	// The instance process is in the running set.
	sch.mu.Lock()
	_, up := sch.agents["worker#1"]
	sch.mu.Unlock()
	if !up {
		t.Fatal("worker#1 not in the running set")
	}

	// Affinity: ctx-A returns to the base even though worker#1 is up and free-ish.
	tA2, relA2, err := sch.resolveInstance("worker", "ctx-A")
	if err != nil {
		t.Fatalf("resolve ctx-A again: %v", err)
	}
	if tA2 != "worker" {
		t.Fatalf("ctx-A affinity target = %q, want worker", tA2)
	}

	relA()
	relB()
	relA2()

	// Exactly two processes were started (base + one instance) — the pool did not
	// over-spawn for the affinity re-acquire.
	if lines := readLines(t, logPath); len(lines) != 2 {
		t.Fatalf("expected 2 start lines (base + 1 instance), got %d: %v", len(lines), lines)
	}
}

// TestSchedulerInstanceSpawnSingleFlight: concurrent resolves for the SAME new
// context (so the pool hands them the same instance ordinal) spawn exactly one
// instance process, not one per caller — the single-flight in startOrWakeInstance.
func TestSchedulerInstanceSpawnSingleFlight(t *testing.T) {
	dir := t.TempDir()
	writeInstanceTestCard(t, dir)
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 600000, wrapper.IdleStopExitCode))
	sch.cfg.Agents[0].Instances = 3

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()
	_ = waitForLines(t, logPath, 1, testLifecycleDeadline)

	// Pin the base busy so the shared context is forced onto a child instance.
	_, relBase, err := sch.resolveInstance("worker", "base-hold")
	if err != nil {
		t.Fatal(err)
	}
	defer relBase()

	const n = 5
	targets := make([]string, n)
	rels := make([]func(), n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tgt, rel, err := sch.resolveInstance("worker", "ctx-shared")
			if err != nil {
				t.Errorf("resolve %d: %v", i, err)
				rel = func() {}
			}
			targets[i], rels[i] = tgt, rel
		}(i)
	}
	wg.Wait()
	for _, r := range rels {
		if r != nil {
			r()
		}
	}

	// All callers of the same context agree on one instance name...
	for i, tgt := range targets {
		if tgt != "worker#1" {
			t.Fatalf("caller %d target = %q, want worker#1", i, tgt)
		}
	}
	// ...and only one instance process was spawned (base + 1 = 2 start lines).
	time.Sleep(150 * time.Millisecond)
	if lines := readLines(t, logPath); len(lines) != 2 {
		t.Fatalf("single-flight violated: %d start lines, want 2: %v", len(lines), lines)
	}
}

// TestSchedulerSingleInstanceUnchanged: the default (Instances unset) agent is
// never pooled — concurrent sessions all route to the base name and no extra
// process is spawned. Guards the backward-compatibility promise.
func TestSchedulerSingleInstanceUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeInstanceTestCard(t, dir)
	logPath := filepath.Join(dir, "starts.log")
	marker := filepath.Join(dir, "marker")
	sch := newIdleTestScheduler(t, dir, idleAgentCommand(logPath, marker, 600000, wrapper.IdleStopExitCode))
	// Instances left at 0 → InstanceCap 1 → not pooled.

	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	_ = waitForLines(t, logPath, 1, testLifecycleDeadline)

	if p := sch.poolFor("worker"); p != nil {
		t.Fatal("single-instance agent must not have a pool")
	}

	t1, r1, err := sch.resolveInstance("worker", "c1")
	if err != nil {
		t.Fatalf("resolve c1: %v", err)
	}
	t2, r2, err := sch.resolveInstance("worker", "c2")
	if err != nil {
		t.Fatalf("resolve c2: %v", err)
	}
	if t1 != "worker" || t2 != "worker" {
		t.Fatalf("targets = (%q,%q), want (worker,worker)", t1, t2)
	}
	r1()
	r2()

	// No extra process spawned by routing.
	time.Sleep(150 * time.Millisecond)
	if lines := readLines(t, logPath); len(lines) != 1 {
		t.Fatalf("single-instance agent spawned extra process: %d start lines %v", len(lines), lines)
	}
}

// TestPublicAgentsHidesInstanceChildren: instance children (base#n) never leak
// into the public /agents listing — a pooled card still shows as one agent.
func TestPublicAgentsHidesInstanceChildren(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	sch.Registry().Register(&a2a.AgentCard{Name: "worker", URL: "http://127.0.0.1:9990/"})
	sch.Registry().Register(&a2a.AgentCard{Name: "worker#1", URL: "http://127.0.0.1:9991/"})
	sch.Registry().Register(&a2a.AgentCard{Name: "worker#2", URL: "http://127.0.0.1:9992/"})
	sch.Registry().Register(&a2a.AgentCard{Name: "other", URL: "http://127.0.0.1:9993/"})

	resp, err := http.Get(gwURL + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents = %d: %s", resp.StatusCode, body)
	}
	var cards []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &cards); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	got := map[string]bool{}
	for _, c := range cards {
		got[c.Name] = true
	}
	if !got["worker"] || !got["other"] {
		t.Fatalf("base cards missing from listing: %v", got)
	}
	if got["worker#1"] || got["worker#2"] {
		t.Fatalf("instance children leaked into public listing: %v", got)
	}
}
