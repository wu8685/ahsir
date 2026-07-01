//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// timeoutTeacherCardYAML is teacherCardYAML with runtime.timeout dropped to
// 1s — below any realistic LLM turn latency, so the very first turn is
// guaranteed to exceed the per-turn deadline.
const timeoutTeacherCardYAML = `name: teacher
description: e2e teacher agent with a deliberately tiny per-turn timeout
version: "1.0.0"
provider:
  name: ahsir
  url: https://github.com/wu8685/ahsir
skills:
  - name: teaching
    description: answer questions concisely
claude:
  systemPrompt: |
    You are a teacher. Answer the user's question in one concise sentence.
  maxAgentCalls: 0
runtime:
  command: claude
  args: []
  timeout: 1s
  provider: deepseek
  baseURL: https://api.deepseek.com/anthropic
  apiKey: "${MODEL_API_KEY}"
  model: deepseek-v4-pro
filesystem:
  enabled: false
network:
  bind: "127.0.0.1"
`

// TestTurnTimeout_E2E pins the stuck-agent UX: when a turn exceeds
// runtime.timeout, the user must get a CLEAR, TIMELY error — not a hang
// until the 5-minute transport deadline. The per-turn timer kills the claude
// process and surfaces "turn timed out" on the failed turn; the session
// transitions to EVICTED with its sessionID preserved (resume recovery is
// covered by TestKillRecovery_E2E, which exercises the same path).
func TestTurnTimeout_E2E(t *testing.T) {
	requireClaudeE2E(t)
	fix := setupE2EWithCards(t, timeoutTeacherCardYAML, studentCardYAML)

	started := time.Now()
	_, err := fix.sendMessageToAgent("teacher", "timeout-m1", "timeout-conv", "What is a goroutine?")
	elapsed := time.Since(started)

	if err == nil {
		t.Fatalf("a 1s per-turn timeout must fail the turn, but it succeeded\n--- log ---\n%s", fix.schedulerLog())
	}
	// Timeliness IS the feature: the error must arrive promptly after the
	// 1s deadline, not ride out the transport's 5-minute budget. 60s is a
	// generous ceiling covering process spawn + kill + error propagation.
	if elapsed > 60*time.Second {
		t.Errorf("timeout error took %v to surface — user-facing latency contract is broken", elapsed)
	}

	logs := fix.schedulerLog()
	if !strings.Contains(logs, "turn timed out after 1s") {
		t.Errorf("expected per-turn timeout marker in log, got:\n%s", logs)
	}
	// The pool's recovery posture: the killed session must be evicted with
	// its identity preserved, not left as a healthy-looking zombie.
	if !strings.Contains(logs, "killing process (sessionID preserved for resume)") {
		t.Errorf("expected timeout-kill marker with resume preservation, got:\n%s", logs)
	}
}
