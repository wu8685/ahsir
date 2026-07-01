//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestUIConsole_ChatLoop_E2E is the real-stack version of the manual
// verification: boot the scheduler + agents, start the standalone `ahsir ui`
// console against it, then drive the console's own /api surface exactly as the
// browser does — async dispatch under a client-owned contextId, poll the task
// to completion, and confirm the conversation surfaces in /api/contexts and its
// transcript in /api/.../history.
//
// This is the regression guard for the bug found during that verification: if
// the console (or the client contract) stops carrying the client's contextId
// end-to-end, the scheduler records an empty-contextId invocation and the
// transcript lands under a different id — so /api/contexts and history both go
// empty even though the turn ran. Asserting both populate locks the contract.
func TestUIConsole_ChatLoop_E2E(t *testing.T) {
	fix := setupE2E(t)

	consolePort, _, _ := allocateFreePorts(t, 3)
	consoleURL := fmt.Sprintf("http://127.0.0.1:%d", consolePort)
	uiBin := filepath.Join(fix.repoRoot, "bin", "ahsir")

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, uiBin, "ui",
		"--addr", fmt.Sprintf("127.0.0.1:%d", consolePort),
		"--scheduler", fix.schedulerURL(),
	)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logBuf := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = logBuf, logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start console: %v", err)
	}
	t.Cleanup(func() {
		cleanupSchedulerProcess(cmd, cancel, nil, t.Logf)
	})
	if err := waitForPort(consolePort, 15*time.Second); err != nil {
		t.Fatalf("console not ready: %v\nconsole log:\n%s", err, logBuf.String())
	}

	const ctxID = "console-e2e-conv"
	httpc := &http.Client{Timeout: 3 * time.Minute}

	// 1. async dispatch with a client-owned contextId.
	body := `{"message":"In one word, reply: pong","async":true,"speaker":"console-e2e","contextId":"` + ctxID + `"}`
	resp, err := httpc.Post(consoleURL+"/api/agents/teacher/chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("console chat POST: %v", err)
	}
	var sub struct{ TaskID, ContextID string }
	decodeJSON(t, resp, &sub)
	if sub.ContextID != ctxID || sub.TaskID == "" {
		t.Fatalf("submit = %+v, want taskId set + contextId %q (console must forward it)", sub, ctxID)
	}

	// 2. poll the task to a terminal state through the console.
	reply := ""
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		r, err := httpc.Get(fmt.Sprintf("%s/api/agents/teacher/tasks/%s", consoleURL, sub.TaskID))
		if err != nil {
			t.Fatalf("poll task: %v", err)
		}
		var task struct {
			Status  struct{ State string } `json:"status"`
			History []struct {
				Role  string `json:"role"`
				Parts []struct{ Kind, Text string } `json:"parts"`
			} `json:"history"`
		}
		decodeJSON(t, r, &task)
		if task.Status.State == "completed" {
			for i := len(task.History) - 1; i >= 0; i-- {
				if task.History[i].Role == "agent" {
					for _, p := range task.History[i].Parts {
						if p.Kind == "text" {
							reply += p.Text
						}
					}
					break
				}
			}
			break
		}
		if task.Status.State == "failed" || task.Status.State == "canceled" {
			t.Fatalf("task ended %s\nconsole log:\n%s\nscheduler log:\n%s", task.Status.State, logBuf.String(), fix.schedulerLog())
		}
		time.Sleep(2 * time.Second)
	}
	if strings.TrimSpace(reply) == "" {
		t.Fatalf("no reply collected from task\nscheduler log:\n%s", fix.schedulerLog())
	}

	// 3. the conversation now surfaces in /api/contexts, keyed by OUR contextId.
	var ctxs []struct {
		ContextID string   `json:"contextId"`
		Title     string   `json:"title"`
		Agents    []string `json:"agents"`
		Turns     int      `json:"turns"`
	}
	getJSON(t, httpc, consoleURL+"/api/contexts", &ctxs)
	var found *struct {
		ContextID string   `json:"contextId"`
		Title     string   `json:"title"`
		Agents    []string `json:"agents"`
		Turns     int      `json:"turns"`
	}
	for i := range ctxs {
		if ctxs[i].ContextID == ctxID {
			found = &ctxs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("contextId %q not in /api/contexts: %+v", ctxID, ctxs)
	}
	if found.Turns < 1 || len(found.Agents) == 0 || found.Agents[0] != "teacher" {
		t.Errorf("context summary wrong: %+v", *found)
	}

	// 4. per-agent history in that context replays the turn (with the reply).
	var turns []struct {
		UserText string `json:"userText"`
		Reply    string `json:"reply"`
		Status   string `json:"status"`
	}
	getJSON(t, httpc, fmt.Sprintf("%s/api/agents/teacher/history/%s", consoleURL, ctxID), &turns)
	if len(turns) < 1 {
		t.Fatalf("history empty for contextId %q — the console dropped it", ctxID)
	}
	if turns[0].Reply == "" || turns[0].Status != "completed" {
		t.Errorf("history turn wrong: %+v", turns[0])
	}
}

func decodeJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s: status %d, body=%s", resp.Request.URL.Path, resp.StatusCode, string(raw))
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %s: %v; body=%s", resp.Request.URL.Path, err, string(raw))
	}
}

func getJSON(t *testing.T, c *http.Client, url string, v any) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	decodeJSON(t, resp, v)
}
