//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestConcurrentDistinctContextIDs_E2E fires N requests to teacher
// simultaneously, each with a unique contextID. Validates the
// concurrency cousin of in-process reuse: same contextID → one process
// (covered elsewhere), DIFFERENT contextIDs → independent processes
// that can run in parallel without blocking each other.
//
// Concretely:
//
//  1. All N goroutines succeed in parallel — no deadlock, no entry-level
//     lock cross-blocks because they key on different contextIDs.
//
//  2. Each reply carries its OWN codeword and ONLY its own. Cross-
//     contamination would mean the pool returned the wrong Session to
//     one of the callers — typically because of a corrupted map key or
//     a stale entry, both serious bugs.
//
//  3. Exactly N distinct `claude session: started` lines, all with
//     distinct pids, none with `--resume=`. Anything else means:
//     - fewer than N: at least one goroutine got a session it shouldn't
//       have (somehow keyed on the wrong contextID),
//     - more than N: a process died mid-flight and recovery fired
//       (shouldn't happen in this scenario),
//     - duplicate pids: filesystem race in the persist file or process
//       table accounting bug.
//
//  4. Exactly N `[teacher] receive` lines — one per request reaching
//     the A2A server, no dropped requests.
//
// The test stays away from same-contextID-concurrent scenarios on
// purpose — ClaudeSession serializes turns via stateInFlight and will
// reject overlapping Stream calls. That's a separate test if we ever
// add queuing at the executor or pool layer.
func TestConcurrentDistinctContextIDs_E2E(t *testing.T) {
	fix := setupE2E(t)

	const N = 3

	type result struct {
		idx       int
		contextID string
		reply     string
		err       error
	}
	results := make(chan result, N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			ctxID := fmt.Sprintf("concurrent-conv-%d", i)
			codeword := fmt.Sprintf("cw-%d", i)
			// Prompt asks teacher to echo back exactly the codeword.
			// Verifies the SAME conversation handled this request (no
			// silent swap) without depending on multi-turn memory.
			reply, err := fix.sendMessage(
				fix.teacherPort,
				fmt.Sprintf("msg-concurrent-%d", i),
				ctxID,
				fmt.Sprintf("Please reply with exactly the following codeword and nothing else: %s", codeword),
			)
			results <- result{i, ctxID, reply, err}
		}()
	}
	wg.Wait()
	close(results)

	// Drain results before any structural log assertions — we want to see
	// per-goroutine errors first if anything went wrong.
	got := make([]result, 0, N)
	for r := range results {
		got = append(got, r)
	}

	// ① Each goroutine succeeded AND each reply contains its own
	// codeword AND no reply contains any other codeword. The "no other
	// codeword" check is the real test of pool isolation — without it,
	// the pool could be silently swapping sessions and we'd never notice
	// as long as every reply just happened to contain SOMETHING that
	// matches a codeword pattern.
	for _, r := range got {
		if r.err != nil {
			t.Errorf("goroutine %d (contextID=%s) failed: %v\n--- scheduler log ---\n%s", r.idx, r.contextID, r.err, fix.schedulerLog())
			continue
		}
		expected := fmt.Sprintf("cw-%d", r.idx)
		if !strings.Contains(r.reply, expected) {
			t.Errorf("goroutine %d (contextID=%s): reply missing own codeword %q: %q", r.idx, r.contextID, expected, r.reply)
		}
		for j := 0; j < N; j++ {
			if j == r.idx {
				continue
			}
			other := fmt.Sprintf("cw-%d", j)
			if strings.Contains(r.reply, other) {
				t.Errorf("goroutine %d (contextID=%s) reply LEAKS another contextID's codeword %q: %q", r.idx, r.contextID, other, r.reply)
			}
		}
	}

	logs := fix.schedulerLog()

	// ② Exactly N claude processes spawned. Pool keyed cleanly on
	// distinct contextIDs.
	starts := parseStartLines(t, logs)
	if len(starts) != N {
		t.Errorf("expected exactly %d claude spawns (one per contextID), got %d:\n%+v\n--- scheduler log ---\n%s", N, len(starts), starts, logs)
	}

	// ③ All pids distinct. A duplicate would mean either the regex
	// caught a non-spawn line or the OS reused a pid mid-test (vanishingly
	// unlikely within the window).
	pids := make(map[int]bool, N)
	for _, s := range starts {
		if s.hasResume {
			t.Errorf("unexpected --resume on fresh-contextID spawn: %+v", s)
		}
		if pids[s.pid] {
			t.Errorf("duplicate pid %d in start lines: %+v", s.pid, starts)
		}
		pids[s.pid] = true
	}

	// ④ All N requests reached teacher's A2A server.
	if got := strings.Count(logs, "[teacher] receive"); got != N {
		t.Errorf("expected %d [teacher] receive lines, got %d", N, got)
	}

	// ⑤ Each contextID appears in a [teacher] receive line — confirms
	// every goroutine's request landed under its own conversation, not
	// merged into one.
	for i := 0; i < N; i++ {
		marker := fmt.Sprintf("[teacher] receive: contextID=concurrent-conv-%d", i)
		if !strings.Contains(logs, marker) {
			t.Errorf("missing receive marker for goroutine %d: %s", i, marker)
		}
	}
}
