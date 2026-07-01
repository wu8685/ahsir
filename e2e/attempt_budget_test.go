//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// ghostStudentCardYAML instructs the student to delegate to an agent that
// does not exist — every A2A_CALL fails at dispatch. maxAgentCalls bounds
// the retry budget the executor grants.
const ghostStudentCardYAML = `name: student
description: e2e student agent that delegates to a nonexistent agent
version: "1.0.0"
provider:
  name: ahsir
  url: https://github.com/wu8685/ahsir
skills:
  - name: learning
    description: delegate questions to the ghost
claude:
  systemPrompt: |
    You are a student. For every user question, you MUST delegate to the
    ghost agent using exactly this format:

    ---A2A_CALL---
    {"agent": "ghost", "task": "<the user's question, verbatim>"}
    ---END---

    If a delegation fails, try delegating to ghost again. Never answer the
    question yourself unless delegation is impossible.
  maxAgentCalls: 2
runtime:
  command: claude
  args: []
  timeout: 300s
  provider: deepseek
  baseURL: https://api.deepseek.com/anthropic
  apiKey: "${MODEL_API_KEY}"
  model: deepseek-v4-pro
filesystem:
  enabled: false
network:
  bind: "127.0.0.1"
`

// TestAttemptBudget_E2E pins the bounded-failure UX: a model stuck retrying
// a broken delegation (here: target agent doesn't exist) must still return
// a reply to the user within the attempt budget — not spin at constant depth
// until the transport's 5-minute deadline.
func TestAttemptBudget_E2E(t *testing.T) {
	requireClaudeE2E(t)
	fix := setupE2EWithCards(t, teacherCardYAML, ghostStudentCardYAML)

	started := time.Now()
	reply, err := fix.sendMessageToAgent("student", "budget-m1", "budget-conv", "What is a goroutine?")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("budget exhaustion must still yield a reply, got error: %v\n--- log ---\n%s", err, fix.schedulerLog())
	}
	if reply == "" {
		t.Error("empty reply after budget exhaustion")
	}

	logs := fix.schedulerLog()
	// The delegation must actually have been attempted and failed.
	if !strings.Contains(logs, "[student → ghost] A2A_CALL") {
		t.Fatalf("student never attempted the ghost delegation:\n%s", logs)
	}
	if !strings.Contains(logs, "A2A_CALL failed") {
		t.Errorf("expected failed-delegation marker in log:\n%s", logs)
	}
	// Budget contract: failed attempts consume depth. maxAgentCalls=2 allows
	// at most attempts at depth 0 and 1 → ≤2 dispatches, ≤3 LLM turns.
	if n := strings.Count(logs, "[student → ghost] A2A_CALL:"); n > 2 {
		t.Errorf("delegation attempted %d times — failed calls are not consuming the budget", n)
	}
	// Wall-clock sanity: a few LLM turns, nowhere near the 5m transport cap.
	if elapsed > 4*time.Minute {
		t.Errorf("reply took %v — suspiciously close to the transport deadline, budget may not be working", elapsed)
	}
}
