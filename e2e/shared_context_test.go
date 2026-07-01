//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Shared-context collaboration e2e
// (specs/2026-06-08-shared-context-collaboration.md): speaker attribution,
// transcript replay, async chat with task polling.

// gatewayChat POSTs to the scheduler's /agents/{name}/chat endpoint with an
// arbitrary body — the CLI's wire shape (speaker, async, ...).
func gatewayChat(f *e2eFixture, agent string, body map[string]any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/agents/%s/chat", f.registryPort, agent)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Post(url, "application/json", bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

func gatewayGetJSON(f *e2eFixture, path string, out any) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", f.registryPort, path)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %d: %s", path, resp.StatusCode, raw)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type historyTurn struct {
	Turn     int    `json:"turn"`
	Speaker  string `json:"speaker"`
	UserText string `json:"userText"`
	Reply    string `json:"reply"`
	Status   string `json:"status"`
}

// TestSharedContextSpeakers_E2E: two people interleave on one context; the
// transcript records who said what with full replies. (Whether the model
// attributes facts correctly is prompt-level behaviour — pinned by unit tests
// on the prompt shape, not asserted against a live LLM here.)
func TestSharedContextSpeakers_E2E(t *testing.T) {
	fix := setupE2E(t)
	const ctxID = "shared-speakers"

	status, body, err := gatewayChat(fix, "teacher", map[string]any{
		"message":   "Remember: my favorite color is red. Confirm in one short sentence.",
		"contextId": ctxID,
		"speaker":   "alice",
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("alice turn: status=%d err=%v body=%s", status, err, body)
	}

	status, body, err = gatewayChat(fix, "teacher", map[string]any{
		"message":   "I'm a different person than the previous speaker. Greet me in one short sentence.",
		"contextId": ctxID,
		"speaker":   "bob",
	})
	if err != nil || status != http.StatusOK {
		t.Fatalf("bob turn: status=%d err=%v body=%s", status, err, body)
	}

	var turns []historyTurn
	if err := gatewayGetJSON(fix, "/agents/teacher/history/"+ctxID, &turns); err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("history turns = %d, want 2: %+v", len(turns), turns)
	}
	if turns[0].Speaker != "alice" || turns[1].Speaker != "bob" {
		t.Errorf("speakers = %q,%q, want alice,bob", turns[0].Speaker, turns[1].Speaker)
	}
	if turns[0].Turn != 1 || turns[1].Turn != 2 {
		t.Errorf("turn numbers = %d,%d, want 1,2", turns[0].Turn, turns[1].Turn)
	}
	for i, turn := range turns {
		if turn.Status != "completed" || turn.Reply == "" {
			t.Errorf("turn %d must be completed with a full reply, got %+v", i+1, turn)
		}
	}
}

// TestAsyncChat_E2E: async=true answers 202 + taskId immediately; the task
// polls to completed with the reply; the ledger passes through queued and
// settles to completed.
func TestAsyncChat_E2E(t *testing.T) {
	fix := setupE2E(t)
	const ctxID = "async-chat"

	started := time.Now()
	status, body, err := gatewayChat(fix, "teacher", map[string]any{
		"message":   "Reply with exactly: pong",
		"contextId": ctxID,
		"speaker":   "alice",
		"async":     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("async chat status = %d, want 202; body=%s", status, body)
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("async submit took %v — must not wait for the LLM turn", took)
	}
	var accepted struct {
		TaskID    string `json:"taskId"`
		ContextID string `json:"contextId"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.TaskID == "" {
		t.Fatalf("async response: err=%v body=%s", err, body)
	}

	// Immediately after submit the invocation should be visibly queued (or
	// already completed if the turn was very fast — both prove the ledger
	// followed the async path; what it must NOT be is a bare in_flight that
	// never settles).
	var invocations []struct {
		Status string `json:"status"`
	}
	if err := gatewayGetJSON(fix, "/invocations?contextId="+ctxID, &invocations); err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 1 {
		t.Fatalf("invocations = %d, want 1", len(invocations))
	}
	if s := invocations[0].Status; s != "queued" && s != "completed" {
		t.Errorf("post-submit invocation status = %q, want queued (or completed)", s)
	}

	// Poll the task to terminal.
	deadline := time.Now().Add(3 * time.Minute)
	var reply string
	for {
		if time.Now().After(deadline) {
			t.Fatal("task never reached a terminal state")
		}
		var task struct {
			Status struct {
				State string `json:"state"`
			} `json:"status"`
			History []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"history"`
		}
		if err := gatewayGetJSON(fix, "/agents/teacher/tasks/"+accepted.TaskID, &task); err != nil {
			t.Fatal(err)
		}
		if task.Status.State == "failed" || task.Status.State == "canceled" {
			t.Fatalf("task ended %s", task.Status.State)
		}
		if task.Status.State == "completed" {
			for i := len(task.History) - 1; i >= 0; i-- {
				if task.History[i].Role == "agent" && len(task.History[i].Parts) > 0 {
					reply = task.History[i].Parts[0].Text
					break
				}
			}
			break
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(strings.ToLower(reply), "pong") {
		t.Errorf("async reply = %q, want pong", reply)
	}

	// The ledger must settle to completed via the background poll.
	settleDeadline := time.Now().Add(30 * time.Second)
	for {
		if err := gatewayGetJSON(fix, "/invocations?contextId="+ctxID, &invocations); err != nil {
			t.Fatal(err)
		}
		if len(invocations) == 1 && invocations[0].Status == "completed" {
			break
		}
		if time.Now().After(settleDeadline) {
			t.Fatalf("async invocation never settled, last=%+v", invocations)
		}
		time.Sleep(time.Second)
	}
}
