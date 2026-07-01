//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestSameContextQueue_E2E pins the same-context concurrency contract AFTER
// the FIFO turn queue landed (specs/2026-06-08-shared-context-collaboration.md,
// superseding the fail-fast contract of
// specs/2026-06-07-same-context-busy-semantics.md):
//
//   - request #1 starts a turn on contextId X
//   - request #2 on X while #1 is in flight → QUEUES (no 409 at the default
//     queue depth) and completes after #1
//   - the conversation memory established by #1 is intact afterwards
//
// The legacy fail-fast behaviour (immediate ErrTurnInFlight) is still pinned
// at the unit level for queue_depth=0 (TestPoolQueueDepthZeroRestoresFailFast)
// and for queue overflow (TestPoolQueueOverflowFailsFast) — the busy/409 wire
// contract survives as the backpressure signal.
func TestSameContextQueue_E2E(t *testing.T) {
	fix := setupE2E(t)

	const ctxID = "queue-conv"

	// Request #1 in the background — a real LLM turn takes seconds.
	type result struct {
		reply string
		err   error
	}
	firstDone := make(chan result, 1)
	go func() {
		reply, err := fix.sendMessageToAgent("teacher", "queue-m1", ctxID, "Remember the codeword: epsilon-23. Confirm in one short sentence.")
		firstDone <- result{reply, err}
	}()

	// Wait until the turn is actually IN FLIGHT — the executor logs
	// "executor turn start contextID=<ctx>" right before opening the stream.
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(fix.schedulerLog(), "executor turn start contextID="+ctxID) {
		if time.Now().After(deadline) {
			t.Fatalf("turn never started\n--- log ---\n%s", fix.schedulerLog())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Request #2 on the same contextId while #1 runs: must QUEUE and succeed
	// — the pre-queue contract rejected this with "agent busy"/409.
	reply2, err := fix.sendMessageToAgent("teacher", "queue-m2", ctxID, "Reply with exactly: second")
	if err != nil {
		if strings.Contains(err.Error(), "agent busy") {
			t.Fatalf("queued request was rejected busy — FIFO queue not active: %v", err)
		}
		t.Fatalf("queued request failed: %v\n--- log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(strings.ToLower(reply2), "second") {
		t.Errorf("queued request reply = %q, want 'second'", reply2)
	}

	// Request #1 must complete unaffected.
	r := <-firstDone
	if r.err != nil {
		t.Fatalf("in-flight request was disturbed by the queued turn: %v\n--- log ---\n%s", r.err, fix.schedulerLog())
	}

	// Request #3 after completion: session healthy, memory intact.
	reply, err := fix.sendMessageToAgent("teacher", "queue-m3", ctxID, "What codeword did I tell you? Answer with the codeword only.")
	if err != nil {
		t.Fatalf("post-queue request failed: %v\n--- log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(reply, "epsilon-23") {
		t.Errorf("conversation memory lost after queued turn, reply: %q", reply)
	}
}
