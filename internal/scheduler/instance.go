package scheduler

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// instanceSep separates a card's base name from its runtime-instance ordinal in
// the pooled-instance naming scheme (issue #18): base name "cma-x-v1" with
// ordinal 2 → "cma-x-v1#2". Ordinal 0 IS the base name itself (no suffix), so a
// single-instance agent — the default — keeps byte-identical names, ports, and
// workspace, and every existing code path that keys on the card name is
// unchanged.
const instanceSep = "#"

// instanceName returns the runtime name for instance idx of base. idx 0 maps to
// base unchanged (backward compatible); idx>0 gets the "#<idx>" suffix.
func instanceName(base string, idx int) string {
	if idx <= 0 {
		return base
	}
	return base + instanceSep + strconv.Itoa(idx)
}

// parseInstanceName splits a runtime name into its base card name and instance
// ordinal. A name with no separator is ordinal 0 (the base). ok is false only
// for a malformed suffix (empty base, or empty/non-positive/non-numeric after
// the separator), so callers can treat such a name as a plain agent name.
func parseInstanceName(name string) (base string, idx int, ok bool) {
	i := strings.LastIndex(name, instanceSep)
	if i < 0 {
		return name, 0, true
	}
	suffix := name[i+len(instanceSep):]
	n, err := strconv.Atoi(suffix)
	if err != nil || n <= 0 || name[:i] == "" {
		return name, 0, false
	}
	return name[:i], n, true
}

// isInstanceChild reports whether name is a non-base pooled instance
// (ordinal>0). Used to hide instance children from public agent listings so a
// pooled card still presents as a single agent.
func isInstanceChild(name string) bool {
	_, idx, ok := parseInstanceName(name)
	return ok && idx > 0
}

// instanceWorkspace returns the isolated workspace directory for instance idx.
// idx 0 keeps the base workspace unchanged; idx>0 nests under "inst-<idx>" so
// concurrent instances of one card never share a working tree (issue #18) — the
// whole reason a coder's concurrent clone/checkout/build sessions stop clobbering
// each other.
func instanceWorkspace(baseWorkspace string, idx int) string {
	if idx <= 0 {
		return baseWorkspace
	}
	return filepath.Join(baseWorkspace, "inst-"+strconv.Itoa(idx))
}

// instancePool assigns the concurrent sessions (contextIDs) of one agent card
// across up to `cap` isolated runtime instances (issue #18). It owns only the
// bookkeeping — which contextID sticks to which instance and how many turns are
// in flight per instance — and hands back an instance ordinal for the scheduler
// to spawn / dial. All process lifecycle stays in the scheduler.
//
// Two invariants drive assignment:
//   - Affinity: a contextID keeps the same instance for its whole life, so the
//     agent's `--resume` (per-workspace sessions.json) always finds its history.
//   - Spread: a brand-new contextID goes to the least-loaded instance, spawning
//     a fresh one (up to cap) rather than doubling up on a busy instance — that
//     is what routes concurrent sessions onto separate working trees.
//
// The empty contextID ("isolated turn, no continuity") is never given sticky
// affinity: each such acquire is spread independently and released purely by
// ordinal, so ephemeral turns still fan out instead of piling onto one instance.
type instancePool struct {
	mu      sync.Mutex
	cap     int            // max concurrent instances (>=1)
	spawned int            // instance ordinals handed out so far (1..cap; ordinal 0 always exists)
	assign  map[string]int // contextID -> instance ordinal (sticky; empty contextID excluded)
	active  map[int]int    // instance ordinal -> in-flight turn count
}

// newInstancePool builds a pool capped at `capacity` concurrent instances. A
// capacity < 1 is clamped to 1 (the degenerate single-instance case).
func newInstancePool(capacity int) *instancePool {
	if capacity < 1 {
		capacity = 1
	}
	return &instancePool{
		cap:     capacity,
		spawned: 1, // ordinal 0 (the base process) is always considered present
		assign:  make(map[string]int),
		active:  make(map[int]int),
	}
}

// acquire reserves an instance for a turn on contextID and returns its ordinal.
// The caller MUST later call release(ordinal) exactly once. A non-empty
// contextID keeps its instance for the pool's lifetime (affinity); an empty
// contextID is spread fresh each time.
func (p *instancePool) acquire(contextID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if contextID != "" {
		if idx, ok := p.assign[contextID]; ok {
			p.active[idx]++
			return idx
		}
	}
	idx := p.pickLocked()
	if contextID != "" {
		p.assign[contextID] = idx
	}
	p.active[idx]++
	return idx
}

// pickLocked chooses the target ordinal for a brand-new session: the
// least-loaded existing instance, unless every existing instance is busy and the
// cap leaves room to grow — then a fresh instance is spawned. Ties favor the
// lowest ordinal so the base instance (ordinal 0, already running) is preferred
// when free. Caller holds p.mu.
func (p *instancePool) pickLocked() int {
	minIdx, minLoad := 0, p.active[0]
	for i := 1; i < p.spawned; i++ {
		if p.active[i] < minLoad {
			minIdx, minLoad = i, p.active[i]
		}
	}
	// All known instances are busy and the cap allows another — grow.
	if minLoad > 0 && p.spawned < p.cap {
		idx := p.spawned
		p.spawned++
		return idx
	}
	return minIdx
}

// release marks one in-flight turn on instance idx as finished. Idempotent-safe
// against underflow. Affinity (assign) is intentionally kept so a later turn on
// the same contextID resumes on the same instance.
func (p *instancePool) release(idx int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active[idx] > 0 {
		p.active[idx]--
	}
}

// activeInstances returns how many distinct instance ordinals the pool has
// handed out (including the base). Test/introspection helper.
func (p *instancePool) activeInstances() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawned
}
