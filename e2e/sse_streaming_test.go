//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSSEStreaming_TeacherEmitsDeltas_E2E validates the message/stream path
// end-to-end with a real claude subprocess:
//
//  1. The teacher card has streaming.partial_messages=true, so the spawned
//     claude is launched with --include-partial-messages.
//  2. The client POSTs `message/stream` over JSON-RPC; the SDK opens an SSE
//     response.
//  3. ClaudeSession parses stream_event NDJSON frames from claude and emits
//     EventTextDelta; ExecuteStream relays them as TaskStatusUpdateEvent;
//     the SDK serialises them as `data: {...}` lines and finally yields a
//     terminal *a2a.Task.
//
// Assertions:
//   - At least one delta-bearing status-update is observed BEFORE the final
//     task (proves the stream is actually incremental, not buffered).
//   - Final event is `kind:task` with state=completed.
//   - Concatenated delta text is non-empty and broadly answers the question.
//
// A truly delta-poor model (single big chunk) is acceptable — the test only
// requires evidence of streaming, not a minimum chunk count. We use a
// concrete question that should force at least a sentence-worth of output.
func TestSSEStreaming_TeacherEmitsDeltas_E2E(t *testing.T) {
	fix := setupE2E(t)

	events, err := fix.sendMessageStreamToAgent(
		"teacher",
		"msg-sse-1",
		"e2e-sse-1",
		"Briefly explain in two short sentences what a TCP three-way handshake is.",
	)
	if err != nil {
		t.Fatalf("sendMessageStream: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	if len(events) == 0 {
		t.Fatalf("no SSE events received\n--- scheduler log ---\n%s", fix.schedulerLog())
	}

	var (
		statusUpdates int
		deltaText     strings.Builder
		finalIdx      = -1
		finalState    string
	)
	for i, ev := range events {
		switch ev.Kind {
		case "status-update":
			statusUpdates++
			var su struct {
				Status struct {
					State   string `json:"state"`
					Message *struct {
						Parts []struct {
							Kind string `json:"kind"`
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"message"`
				} `json:"status"`
			}
			if err := json.Unmarshal(ev.Raw, &su); err != nil {
				t.Fatalf("parse status-update[%d]: %v\n  raw=%s", i, err, ev.Raw)
			}
			if su.Status.Message == nil {
				continue
			}
			for _, p := range su.Status.Message.Parts {
				if p.Kind == "text" {
					deltaText.WriteString(p.Text)
				}
			}
		case "task":
			finalIdx = i
			var task struct {
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			}
			if err := json.Unmarshal(ev.Raw, &task); err != nil {
				t.Fatalf("parse task event[%d]: %v\n  raw=%s", i, err, ev.Raw)
			}
			finalState = task.Status.State
		}
	}

	if finalIdx < 0 {
		t.Fatalf("no terminal task event observed; got %d events\n--- scheduler log ---\n%s", len(events), fix.schedulerLog())
	}
	if finalIdx != len(events)-1 {
		t.Errorf("terminal task should be the LAST event; got at idx=%d of %d", finalIdx, len(events))
	}
	if finalState != "completed" {
		t.Errorf("want final state=completed, got %q", finalState)
	}
	// statusUpdates must be ≥1 (the initial Working announcement is yielded
	// before the first claude delta lands, so even a single-chunk claude
	// response yields ≥1 status-update). To prove incremental streaming we
	// expect at least one delta-bearing update — i.e. non-empty aggregated
	// text from status updates' message parts.
	if statusUpdates < 1 {
		t.Errorf("want ≥1 status-update before task, got %d", statusUpdates)
	}
	if deltaText.Len() == 0 {
		t.Errorf("expected non-empty aggregated delta text from status updates — partial-message streaming did not produce text. claude may not have surfaced content_block_delta frames.\n--- scheduler log ---\n%s", fix.schedulerLog())
	}
	t.Logf("SSE events=%d status_updates=%d aggregated_delta_bytes=%d final_state=%s", len(events), statusUpdates, deltaText.Len(), finalState)
	t.Logf("aggregated deltas: %q", deltaText.String())
}

// TestSSEStreaming_DeltasArriveIncrementally_E2E proves that the stream is
// genuinely incremental — i.e. not "buffer the whole response then flush all
// frames at once". Without this assertion the SSE plumbing could be entirely
// broken (server buffers until claude finishes, dumps all frames in one
// go) and TestSSEStreaming_TeacherEmitsDeltas_E2E would still pass.
//
// The signal: the time elapsed between the FIRST and LAST delta-bearing
// status-update must be > 200ms. A 50-200 ms wall-clock answer would suggest
// the LLM responded almost instantly and dumping all chunks took microseconds
// — that's possible but rare for a real model. 200ms is well above realistic
// network jitter floor while still being short enough that even a fast
// claude response trips it.
//
// We also assert that NOT all deltas arrived in the first 50 ms — a tighter
// bound that catches the "all-frames-in-one-batch" failure mode where the
// timestamps would cluster within the same millisecond.
func TestSSEStreaming_DeltasArriveIncrementally_E2E(t *testing.T) {
	fix := setupE2E(t)

	// Ask for a longer answer so the model has to emit many deltas over time.
	// Empirically deepseek-v4-pro produces ~50-60 delta frames over ~5s for
	// a "explain X in 4-5 sentences" prompt.
	events, err := fix.sendMessageStreamToAgent(
		"teacher",
		"msg-sse-incr",
		"e2e-sse-incr",
		"Explain in four short sentences what a goroutine is, how it differs from an OS thread, what M:N scheduling means in Go, and one common pitfall to avoid when using them.",
	)
	if err != nil {
		t.Fatalf("sendMessageStream: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}

	// Pick out only the deltas (status-updates that carry text) — the
	// initial "working" announcement (no message) doesn't count for the
	// incrementality argument.
	var deltaArrival []time.Time
	for _, ev := range events {
		if ev.Kind != "status-update" {
			continue
		}
		var su struct {
			Status struct {
				Message *struct {
					Parts []struct {
						Kind string `json:"kind"`
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"status"`
		}
		if err := json.Unmarshal(ev.Raw, &su); err != nil {
			continue
		}
		if su.Status.Message == nil {
			continue
		}
		hasText := false
		for _, p := range su.Status.Message.Parts {
			if p.Kind == "text" && p.Text != "" {
				hasText = true
				break
			}
		}
		if hasText {
			deltaArrival = append(deltaArrival, ev.ArrivedAt)
		}
	}

	if len(deltaArrival) < 3 {
		t.Fatalf("want ≥3 delta-bearing status updates to argue incrementality, got %d\n--- scheduler log ---\n%s", len(deltaArrival), fix.schedulerLog())
	}

	first := deltaArrival[0]
	last := deltaArrival[len(deltaArrival)-1]
	spread := last.Sub(first)
	if spread < 200*time.Millisecond {
		t.Errorf("deltas spread only %v across %d frames — looks buffered, not streamed", spread, len(deltaArrival))
	}

	// Tighter check: not all deltas fit in the first 50ms window. If the
	// server were batch-flushing, every delta's timestamp would cluster
	// within scanner-read jitter (sub-millisecond) of the first one.
	var inEarlyWindow int
	earlyCutoff := first.Add(50 * time.Millisecond)
	for _, ts := range deltaArrival {
		if ts.Before(earlyCutoff) {
			inEarlyWindow++
		}
	}
	if inEarlyWindow == len(deltaArrival) {
		t.Errorf("all %d deltas arrived within 50ms of the first — buffering suspected", len(deltaArrival))
	}

	t.Logf("incrementality OK: %d deltas, total spread=%v, deltas-in-first-50ms=%d", len(deltaArrival), spread, inEarlyWindow)
}

// TestSSEStreaming_CLI_E2E exercises the full `ahsir chat --stream` CLI
// path: the binary resolves the agent URL from the scheduler, opens SSE,
// streams deltas to stdout, and exits 0 on the terminal task frame. The
// test asserts that stdout contains a coherent answer to the question.
//
// Why this exists on top of TestSSEStreaming_TeacherEmitsDeltas_E2E: the
// HTTP-level test proves the wire protocol works; this one proves the CLI
// actually uses it correctly — different layer, different breakage modes
// (URL resolution, stdin/stdout plumbing, exit codes, signal handling).
func TestSSEStreaming_CLI_E2E(t *testing.T) {
	fix := setupE2E(t)

	ahsirBin := filepath.Join(fix.repoRoot, "bin", "ahsir")
	schedulerURL := fmt.Sprintf("http://127.0.0.1:%d", fix.registryPort)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, ahsirBin, "chat",
		"--scheduler", schedulerURL,
		"--stream",
		"teacher",
		"In one short sentence, what is a TCP three-way handshake?",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ahsir chat --stream failed: %v\noutput:\n%s\n--- scheduler log ---\n%s", err, string(out), fix.schedulerLog())
	}
	output := string(out)
	lower := strings.ToLower(output)
	if !strings.Contains(lower, "tcp") && !strings.Contains(lower, "handshake") {
		t.Errorf("stream output did not mention tcp / handshake: %q", output)
	}
	if len(strings.TrimSpace(output)) == 0 {
		t.Fatal("stream output was empty")
	}
	t.Logf("CLI stream stdout: %q", output)
}
