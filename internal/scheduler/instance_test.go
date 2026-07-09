package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The admin start API must carry the instances cap so dynamically-registered
// agents (the cma-service path) can opt into a worker pool.
func TestStartAgentRequestDecodesInstances(t *testing.T) {
	var req startAgentRequest
	if err := json.Unmarshal([]byte(`{"name":"cma-coder-v1","workspace":"/ws","instances":4}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Instances != 4 {
		t.Fatalf("instances = %d, want 4", req.Instances)
	}
	// Omitted instances defaults to 0 → single instance downstream.
	var bare startAgentRequest
	if err := json.Unmarshal([]byte(`{"name":"x","workspace":"/ws"}`), &bare); err != nil {
		t.Fatal(err)
	}
	if bare.Instances != 0 {
		t.Fatalf("omitted instances = %d, want 0", bare.Instances)
	}
}

func TestInstanceName(t *testing.T) {
	cases := []struct {
		base string
		idx  int
		want string
	}{
		{"cma-x-v1", 0, "cma-x-v1"},  // ordinal 0 is the base, unchanged
		{"cma-x-v1", -3, "cma-x-v1"}, // negative clamps to base
		{"cma-x-v1", 1, "cma-x-v1#1"},
		{"cma-x-v1", 12, "cma-x-v1#12"},
	}
	for _, c := range cases {
		if got := instanceName(c.base, c.idx); got != c.want {
			t.Errorf("instanceName(%q,%d) = %q, want %q", c.base, c.idx, got, c.want)
		}
	}
}

func TestParseInstanceName(t *testing.T) {
	cases := []struct {
		name     string
		wantBase string
		wantIdx  int
		wantOK   bool
	}{
		{"worker", "worker", 0, true},
		{"worker#1", "worker", 1, true},
		{"worker#42", "worker", 42, true},
		{"cma-a-b-v1#3", "cma-a-b-v1", 3, true},
		{"worker#0", "worker#0", 0, false},   // #0 is not a child ordinal
		{"worker#", "worker#", 0, false},     // empty suffix
		{"worker#x", "worker#x", 0, false},   // non-numeric suffix
		{"worker#-1", "worker#-1", 0, false}, // negative suffix
		{"#1", "#1", 0, false},               // empty base
	}
	for _, c := range cases {
		base, idx, ok := parseInstanceName(c.name)
		if base != c.wantBase || idx != c.wantIdx || ok != c.wantOK {
			t.Errorf("parseInstanceName(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.name, base, idx, ok, c.wantBase, c.wantIdx, c.wantOK)
		}
	}
}

// parseInstanceName must round-trip instanceName for every ordinal so the naming
// and routing halves can never drift.
func TestInstanceNameRoundTrip(t *testing.T) {
	for idx := 1; idx <= 20; idx++ {
		name := instanceName("cma-coder-v1", idx)
		base, got, ok := parseInstanceName(name)
		if !ok || base != "cma-coder-v1" || got != idx {
			t.Errorf("round-trip idx=%d via %q → (%q,%d,%v)", idx, name, base, got, ok)
		}
	}
}

func TestIsInstanceChild(t *testing.T) {
	if isInstanceChild("worker") {
		t.Error("base name must not be an instance child")
	}
	if !isInstanceChild("worker#1") {
		t.Error("worker#1 must be an instance child")
	}
	if isInstanceChild("worker#0") {
		t.Error("worker#0 is not a valid child ordinal")
	}
}

func TestInstanceWorkspace(t *testing.T) {
	base := filepath.Join("tmp", "agents", "cma-x-v1")
	if got := instanceWorkspace(base, 0); got != base {
		t.Errorf("idx 0 must keep base workspace, got %q", got)
	}
	if got := instanceWorkspace(base, 2); got != filepath.Join(base, "inst-2") {
		t.Errorf("idx 2 workspace = %q, want %q", got, filepath.Join(base, "inst-2"))
	}
}

func TestInstanceCap(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 1}, {-5, 1}, {1, 1}, {3, 3}, {16, 16},
	}
	for _, c := range cases {
		if got := (AgentConfig{Instances: c.in}).InstanceCap(); got != c.want {
			t.Errorf("InstanceCap(Instances=%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestDeriveInstanceConfig(t *testing.T) {
	base := AgentConfig{
		Name:          "cma-coder-v1",
		Workspace:     "/ws/cma-coder-v1",
		Port:          9810,
		InternalToken: "secret",
		Instances:     4,
	}
	inst := deriveInstanceConfig(base, 2)
	if inst.Name != "cma-coder-v1#2" {
		t.Errorf("name = %q, want cma-coder-v1#2", inst.Name)
	}
	if inst.Workspace != filepath.Join("/ws/cma-coder-v1", "inst-2") {
		t.Errorf("workspace = %q", inst.Workspace)
	}
	if inst.Port != 0 {
		t.Errorf("port must reset to 0 for fresh allocation, got %d", inst.Port)
	}
	if inst.InternalToken != "" {
		t.Errorf("internal token must reset, got %q", inst.InternalToken)
	}
	// Instances is carried so the derived cfg still reports the pool cap.
	if inst.Instances != 4 {
		t.Errorf("instances = %d, want 4", inst.Instances)
	}
}

// A brand-new context reuses a free base instance rather than spawning.
func TestInstancePoolReusesFreeBase(t *testing.T) {
	p := newInstancePool(3)
	idx := p.acquire("ctx-A")
	if idx != 0 {
		t.Fatalf("first session should land on base (0), got %d", idx)
	}
	p.release(idx)
	// Base is free again → next session reuses it, no growth.
	if idx := p.acquire("ctx-B"); idx != 0 {
		t.Fatalf("free base should be reused, got %d", idx)
	}
	if p.activeInstances() != 1 {
		t.Fatalf("no new instance should have spawned, spawned=%d", p.activeInstances())
	}
}

// Concurrent distinct sessions spread onto fresh instances up to the cap.
func TestInstancePoolSpreadsUpToCap(t *testing.T) {
	p := newInstancePool(3)
	// Hold three concurrent sessions — each should get its own instance.
	a := p.acquire("ctx-A") // base 0 (free)
	b := p.acquire("ctx-B") // base busy → spawn 1
	c := p.acquire("ctx-C") // 0,1 busy → spawn 2
	if a != 0 || b != 1 || c != 2 {
		t.Fatalf("spread = (%d,%d,%d), want (0,1,2)", a, b, c)
	}
	if p.activeInstances() != 3 {
		t.Fatalf("spawned = %d, want 3", p.activeInstances())
	}
	// A fourth concurrent session at cap must reuse the least-loaded instance,
	// not spawn a fourth.
	d := p.acquire("ctx-D")
	if d < 0 || d > 2 {
		t.Fatalf("at-cap session must reuse an existing instance, got %d", d)
	}
	if p.activeInstances() != 3 {
		t.Fatalf("cap breached: spawned = %d, want 3", p.activeInstances())
	}
}

// A context is pinned to its instance for its whole life (resume affinity),
// even after the turn is released and the instance is free.
func TestInstancePoolAffinity(t *testing.T) {
	p := newInstancePool(3)
	a := p.acquire("ctx-A") // 0
	b := p.acquire("ctx-B") // 1 (0 busy)
	if a != 0 || b != 1 {
		t.Fatalf("initial spread = (%d,%d), want (0,1)", a, b)
	}
	p.release(a)
	p.release(b)
	// ctx-A returns → must land on its original instance 0 again, even though 1
	// is equally free, because affinity is sticky.
	if got := p.acquire("ctx-A"); got != 0 {
		t.Fatalf("affinity broken: ctx-A → %d, want 0", got)
	}
	if got := p.acquire("ctx-B"); got != 1 {
		t.Fatalf("affinity broken: ctx-B → %d, want 1", got)
	}
}

// The empty contextID (isolated turn) never gets sticky affinity: two
// concurrent empty-context turns still spread across instances.
func TestInstancePoolEmptyContextSpreads(t *testing.T) {
	p := newInstancePool(2)
	a := p.acquire("") // 0
	b := p.acquire("") // 0 busy → 1
	if a != 0 || b != 1 {
		t.Fatalf("empty-context spread = (%d,%d), want (0,1)", a, b)
	}
	// No affinity entry was stored for "".
	p.mu.Lock()
	n := len(p.assign)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("empty context must not create affinity entries, got %d", n)
	}
}

// release must not underflow the active counter.
func TestInstancePoolReleaseUnderflowSafe(t *testing.T) {
	p := newInstancePool(2)
	p.release(0) // never acquired
	p.release(0)
	idx := p.acquire("ctx-A")
	if idx != 0 {
		t.Fatalf("acquire after spurious release = %d, want 0", idx)
	}
}

func TestScaffoldInstanceWorkspace(t *testing.T) {
	base := t.TempDir()
	// A base card to copy.
	if err := os.MkdirAll(filepath.Join(base, ".a2a"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := "name: worker\nruntime:\n  command: echo\n"
	if err := os.WriteFile(filepath.Join(base, ".a2a", "agent-card.yaml"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	inst := filepath.Join(base, "inst-1")

	// First scaffold copies the card verbatim.
	if err := scaffoldInstanceWorkspace(base, inst); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(inst, ".a2a", "agent-card.yaml"))
	if err != nil {
		t.Fatalf("read scaffolded card: %v", err)
	}
	if string(got) != want {
		t.Fatalf("scaffolded card = %q, want %q", got, want)
	}

	// Idempotent: a second scaffold leaves an operator-edited instance card alone.
	edited := "name: worker\nruntime:\n  command: edited\n"
	if err := os.WriteFile(filepath.Join(inst, ".a2a", "agent-card.yaml"), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scaffoldInstanceWorkspace(base, inst); err != nil {
		t.Fatalf("re-scaffold: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(inst, ".a2a", "agent-card.yaml"))
	if string(got) != edited {
		t.Fatalf("re-scaffold clobbered an existing card: %q", got)
	}
}

func TestScaffoldInstanceWorkspaceNoOpAndError(t *testing.T) {
	// Same base and instance path → no-op (never happens for idx>0, but guarded).
	dir := t.TempDir()
	if err := scaffoldInstanceWorkspace(dir, dir); err != nil {
		t.Fatalf("same-path scaffold should be a no-op, got %v", err)
	}
	// Empty paths → no-op.
	if err := scaffoldInstanceWorkspace("", "/x"); err != nil {
		t.Fatalf("empty base should be a no-op, got %v", err)
	}
	// Missing base card → error (nothing to copy).
	base := t.TempDir()
	if err := scaffoldInstanceWorkspace(base, filepath.Join(base, "inst-1")); err == nil {
		t.Fatal("expected error when base card is missing")
	}
}

func TestNewInstancePoolClampsCapacity(t *testing.T) {
	if p := newInstancePool(0); p.cap != 1 {
		t.Errorf("cap 0 should clamp to 1, got %d", p.cap)
	}
	if p := newInstancePool(-4); p.cap != 1 {
		t.Errorf("negative cap should clamp to 1, got %d", p.cap)
	}
}
