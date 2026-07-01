//go:build e2e

package wrapper

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestClaudeSession_E2E_MultiTurnContext spawns a real `claude` process in
// stream-json mode and verifies that conversation history persists across
// turns inside one Session. Build-tagged `e2e` so it never runs in CI by
// default; gated on AHSIR_E2E_CLAUDE=1 to avoid surprise API spend.
//
// Run with:
//   AHSIR_E2E_CLAUDE=1 go test -tags=e2e ./internal/wrapper/ -run TestClaudeSession_E2E -v
//
// The test also incidentally validates Spec §8 Q#5 — whether --resume / the
// stream-json protocol works against the configured provider (zhipu /
// Anthropic direct / etc).
func TestClaudeSession_E2E_MultiTurnContext(t *testing.T) {
	if os.Getenv("AHSIR_E2E_CLAUDE") != "1" {
		t.Skip("set AHSIR_E2E_CLAUDE=1 to run real-claude e2e tests")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := SessionConfig{
		Name:    "e2e",
		Command: "claude",
		Timeout: 60 * time.Second,
		// Inherit ambient env so ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN
		// / ANTHROPIC_MODEL from the test runner apply.
		Env: os.Environ(),
	}

	s, err := NewClaudeSession(ctx, cfg, "")
	if err != nil {
		t.Fatalf("NewClaudeSession: %v", err)
	}
	defer s.Close()

	// Turn 1: tell claude a name. SessionID becomes available once init
	// arrives in the event stream of this first turn.
	out1, err := s.Turn(ctx, "我叫昊天，请简短回复确认收到。")
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	t.Logf("turn 1 reply: %q", out1)

	if s.SessionID() == "" {
		t.Error("expected non-empty SessionID after first turn (init should have arrived)")
	}

	// Turn 2: ask it back — should hit the same process, claude remembers.
	out2, err := s.Turn(ctx, "我刚才告诉你我叫什么名字？")
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	t.Logf("turn 2 reply: %q", out2)

	if !strings.Contains(out2, "昊天") {
		t.Errorf("multi-turn context broken: turn 2 reply does not mention '昊天': %q", out2)
	}
}

// TestClaudeSession_E2E_ResumeAcrossProcesses verifies that --resume picks
// up a prior conversation in a brand-new process. This is the linchpin of
// SessionPool's EVICTED→READY recovery path.
func TestClaudeSession_E2E_ResumeAcrossProcesses(t *testing.T) {
	if os.Getenv("AHSIR_E2E_CLAUDE") != "1" {
		t.Skip("set AHSIR_E2E_CLAUDE=1 to run real-claude e2e tests")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cfg := SessionConfig{
		Name:    "e2e-resume",
		Command: "claude",
		Timeout: 60 * time.Second,
		Env:     os.Environ(),
	}

	// First process: establish a fact. SessionID is captured during the
	// first Turn (when init arrives in the response stream).
	s1, err := NewClaudeSession(ctx, cfg, "")
	if err != nil {
		t.Fatalf("first NewClaudeSession: %v", err)
	}
	if _, err := s1.Turn(ctx, "记住这个暗号：紫色独角兽42。"); err != nil {
		s1.Close()
		t.Fatalf("first turn: %v", err)
	}
	sessionID := s1.SessionID()
	s1.Close()
	if sessionID == "" {
		t.Fatal("expected non-empty SessionID after first turn")
	}

	// Second process: --resume the same sessionID, ask for the secret. The
	// resume validation now happens via EventTurnDone error if claude
	// rotates the id, so we surface it through the Turn call below.
	s2, err := NewClaudeSession(ctx, cfg, sessionID)
	if err != nil {
		t.Fatalf("resume NewClaudeSession: %v", err)
	}
	defer s2.Close()
	out, err := s2.Turn(ctx, "刚才我让你记的暗号是什么？")
	if err != nil {
		t.Fatalf("resumed turn: %v", err)
	}
	if s2.SessionID() != sessionID {
		t.Errorf("resume sessionID mismatch: got %q want %q", s2.SessionID(), sessionID)
	}
	t.Logf("resumed reply: %q", out)
	if !strings.Contains(out, "紫色独角兽42") {
		t.Errorf("--resume context lost: reply does not mention secret: %q", out)
	}
}
