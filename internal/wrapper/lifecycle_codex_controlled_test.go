package wrapper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Issue #47 controlled-runtime validation: the full lifecycle path runs over a
// REAL CodexSession driving a real subprocess — not a fake that always
// returns cancellation errors. The fake-codex script reproduces the observed
// production behaviour: on the business turn it blocks and, when the wrapper's
// work deadline kills it, exits 0 WITHOUT emitting turn.completed (so the
// provider-level result would historically surface as a nil error). The
// finalize turn emits a well-formed stream.
//
// Expected: the deadline (not the nil provider result) settles the turn —
// turn.soft_deadline fires, exactly one finalize turn runs in the same logical
// session, and the outcome is runtime_timeout + finalized, never a silent
// empty "completed".
func TestLifecycleCodexDeadlineControlledRuntime(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex")
	script := `#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
printf '%s\n' '{"type":"thread.started","thread_id":"thread-ctl"}'
case "$last" in
*FINALIZE*)
  printf '%s\n' '{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"final summary"}}'
  printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":2}}'
  exit 0
  ;;
*)
  printf '%s\n' '{"type":"item.completed","item":{"id":"w1","type":"agent_message","text":"wip"}}'
  trap 'exit 0' TERM INT
  # In-shell wait loop (no child process): a killed child would inherit the
  # stdout pipe and keep the parser blocked until it exits.
  while :; do :; done
  exit 0
  ;;
esac
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	cfg := finalizeLifecycle(300*time.Millisecond, 5*time.Second)
	store := NewLifecycleStore(filepath.Join(dir, "lifecycle.json"))
	open := func(ctx context.Context, _ string) (Session, error) {
		// Timeout 0: only the lifecycle-owned deadlines govern this turn.
		return NewCodexSession(ctx, SessionConfig{Command: scriptPath}, "")
	}
	ctrl := NewLifecycleController(cfg, store, open)

	sess, err := ctrl.OpenSession(context.Background(), "ctx-ctl")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := sess.Stream(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	events, text, doneErr := drain(ch)
	if doneErr != nil {
		t.Fatalf("finalized turn must settle without error: %v", doneErr)
	}
	if !strings.Contains(text, "final summary") {
		t.Fatalf("finalize summary missing from delivery: %q", text)
	}

	out := store.Record("ctx-ctl").LastOutcome
	if out == nil {
		t.Fatal("terminal outcome missing")
	}
	if out.Kind != KindRuntimeTimeout || !out.Finalized || out.Trigger != TriggerSoftDeadline {
		t.Fatalf("deadline truth must win over the nil provider result: %+v", out)
	}
	if out.LastMessage != "final summary" {
		t.Fatalf("last durable message = %q", out.LastMessage)
	}
	types := lifecycleEventTypes(events)
	for _, want := range []string{EvtTurnRunning, EvtTurnSoftDeadline, EvtTurnFinalizing, EvtTurnFinalized, EvtTurnOutcome} {
		if !containsStr(types, want) {
			t.Fatalf("missing %s in %v", want, types)
		}
	}
	if sess.SessionID() != "thread-ctl" {
		t.Fatalf("resume identity must be the codex thread id, got %q", sess.SessionID())
	}
}

// Issue #47 regression over a REAL CodexSession + real subprocess: the
// provider exits 0 mid-turn WITHOUT emitting turn.completed while its ctx is
// still live (a truncated protocol stream). This is the exact nil-error shape
// observed in production. The lifecycle must record a typed provider error —
// never a silent empty "completed" — and must not run a finalize turn (no
// deadline fired).
func TestLifecycleCodexTruncatedStreamControlledRuntime(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-codex")
	logPath := filepath.Join(dir, "prompts.log")
	script := `#!/bin/sh
last=""
for a in "$@"; do last="$a"; done
echo "$last" >> "` + logPath + `"
printf '%s\n' '{"type":"thread.started","thread_id":"thread-trunc"}'
printf '%s\n' '{"type":"item.completed","item":{"id":"w1","type":"agent_message","text":"partial wip"}}'
# EOF here WITHOUT turn.completed/turn.failed/error: truncated stream.
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}

	cfg := finalizeLifecycle(time.Hour, time.Second)
	store := NewLifecycleStore(filepath.Join(dir, "lifecycle.json"))
	open := func(ctx context.Context, _ string) (Session, error) {
		return NewCodexSession(ctx, SessionConfig{Command: scriptPath}, "")
	}
	ctrl := NewLifecycleController(cfg, store, open)

	sess, err := ctrl.OpenSession(context.Background(), "ctx-trunc")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := sess.Stream(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	_, text, doneErr := drain(ch)
	if !errors.Is(doneErr, ErrStreamTruncated) && !errors.Is(doneErr, ErrCodexStreamTruncated) {
		t.Fatalf("truncated stream must surface a typed error, got %v", doneErr)
	}
	out := store.Record("ctx-trunc").LastOutcome
	if out == nil || out.Kind != KindProviderError || out.Finalized {
		t.Fatalf("nil exit without terminal event must not record completed: %+v", out)
	}
	if out.LastMessage != "partial wip" {
		t.Fatalf("partial evidence must be preserved: %q", out.LastMessage)
	}
	_ = text
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if prompts := strings.Split(strings.TrimSpace(string(data)), "\n"); len(prompts) != 1 {
		t.Fatalf("finalize must not run without a deadline: %v", prompts)
	}
}
