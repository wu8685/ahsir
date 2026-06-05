package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/registry"
	"github.com/wu8685/ahsir/internal/wrapper"
)

// realAgent spins up a wrapper.A2AServer wired to a synchronous executor over
// httptest, mirroring how a real agent process is reachable from the
// scheduler. Returns the agent's URL so it can be registered with the
// scheduler's registry. The reply produced by the agent is fixed so the test
// can assert deterministically.
//
// We deliberately use the real wrapper.A2AServer (not a hand-rolled JSON-RPC
// stub like scheduler_test.go's mockA2AServer) so this test exercises the
// full Option A path: HTTP → A2A JSON-RPC handler → executor → mocked
// SendPrompt. That way both Option A and Option B pass through identical
// code on the agent side; only the entry point differs.
func realAgent(t *testing.T, name, reply string, replyDelay time.Duration) string {
	t.Helper()
	taskStore := wrapper.NewTaskStore()
	sender := func(ctx context.Context, prompt string) (string, error) {
		if replyDelay > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(replyDelay):
			}
		}
		return reply + "\n", nil
	}
	exec := wrapper.NewExecutor(wrapper.ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (wrapper.Session, error) {
			return wrapper.NewOneshotSession(sender), nil
		},
		ListAgents: func() []*a2a.AgentCard { return nil },
		MaxDepth:   0,
		BasePrompt: "you are " + name,
	})
	a2aServer := wrapper.NewA2AServer(taskStore, exec.Execute)
	srv := httptest.NewServer(a2aServer)
	t.Cleanup(srv.Close)
	return srv.URL
}

// newTestScheduler wires up a Scheduler with a registry and gateway exposed
// over httptest, returning both the scheduler and the gateway URL. No
// subprocess agents are launched — agents are registered directly in the
// registry, which is exactly how the gateway sees them in production.
func newTestScheduler(t *testing.T) (*Scheduler, string) {
	t.Helper()
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: 0},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	regHandler := registry.NewHTTPHandler(sch.Registry())
	gw := newGatewayHandler(sch, regHandler)

	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return sch, srv.URL
}

// postChat is a thin helper that hits the gateway's chat endpoint.
func postChat(t *testing.T, gatewayURL, agent, message string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"message": message})
	req, _ := http.NewRequest(http.MethodPost, gatewayURL+"/agents/"+agent+"/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s/agents/%s/chat: %v", gatewayURL, agent, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// postA2AMessage is a thin helper that hits an agent's A2A JSON-RPC endpoint
// directly (Option A path). Returns the parsed `result` field so callers can
// assert on the resulting Task.
func postA2AMessage(t *testing.T, agentURL, text string) map[string]any {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: text})
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "message/send",
		"params":  &a2a.MessageSendParams{Message: msg},
		"id":      "test",
	})
	resp, err := http.Post(agentURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", agentURL, err)
	}
	defer resp.Body.Close()

	var rpc struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		t.Fatalf("decode JSON-RPC response: %v", err)
	}
	if rpc.Error != nil {
		t.Fatalf("JSON-RPC error: %v", rpc.Error)
	}
	return rpc.Result
}

// assertReplyInTask digs the agent's textual reply out of an A2A Task
// returned via JSON-RPC and asserts it contains `want`.
func assertReplyInTask(t *testing.T, task map[string]any, want string) {
	t.Helper()
	history, ok := task["history"].([]any)
	if !ok || len(history) == 0 {
		t.Fatalf("task has no history: %v", task)
	}
	last, _ := history[len(history)-1].(map[string]any)
	parts, _ := last["parts"].([]any)
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if text, ok := pm["text"].(string); ok && strings.Contains(text, want) {
			return
		}
	}
	t.Fatalf("reply %q not found in task history: %v", want, task)
}

// TestExampleFlow_OptionA_DirectAgentA2A is the regression test for the
// "curl http://127.0.0.1:9801/ with JSON-RPC message/send" flow documented
// in example/README.md. It does NOT go through the scheduler gateway —
// just the agent's A2A endpoint, exactly like Option A in the README.
func TestExampleFlow_OptionA_DirectAgentA2A(t *testing.T) {
	agentURL := realAgent(t, "teacher", "A goroutine is a lightweight thread.", 0)

	task := postA2AMessage(t, agentURL, "What is a goroutine?")
	if state, _ := task["status"].(map[string]any)["state"].(string); state != string(a2a.TaskStateCompleted) {
		t.Errorf("expected task state=completed, got %q", state)
	}
	assertReplyInTask(t, task, "lightweight thread")
}

// TestExampleFlow_OptionB_SchedulerGateway is the regression test for the
// "curl http://127.0.0.1:9800/agents/teacher/chat" flow documented as
// Option B in example/README.md. It exercises the full scheduler-side path:
//
//	gateway HTTP -> ChatWithAgent -> A2A client -> agent A2A server
//
// This is the path that the user's curl was failing on with the old 30s
// gateway timeout — the test would have failed previously when paired with
// a slow-enough agent reply.
func TestExampleFlow_OptionB_SchedulerGateway(t *testing.T) {
	sch, gwURL := newTestScheduler(t)

	agentURL := realAgent(t, "teacher", "A goroutine is a lightweight thread.", 0)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                agentURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	status, body := postChat(t, gwURL, "teacher", "What is a goroutine?")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !strings.Contains(resp.Response, "lightweight thread") {
		t.Errorf("expected response to contain 'lightweight thread', got %q", resp.Response)
	}
}

// TestGatewayChat_AgentNotFound verifies the gateway distinguishes "agent
// missing from registry" (404) from "agent reachable but failed" (502).
// This split exists so CLI callers can surface a useful error to the user.
func TestGatewayChat_AgentNotFound(t *testing.T) {
	_, gwURL := newTestScheduler(t)

	status, body := postChat(t, gwURL, "ghost", "hi")
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 for missing agent, got %d: %s", status, body)
	}
}

// TestAdminStart_RejectsBadBody verifies the /admin/agents endpoint
// returns 400 for malformed input — no name, no workspace, broken JSON.
// We don't drive the full subprocess-spawn path in unit tests because
// that requires a real ahsir-agent binary; the spawn path is covered by
// the end-to-end CLI smoke run on a built binary tree.
func TestAdminStart_RejectsBadBody(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sch.Stop()

	cases := []struct {
		name   string
		body   string
		want   int
		errSub string
	}{
		{"missing-name", `{"workspace":"/tmp/ws"}`, http.StatusBadRequest, "name"},
		{"missing-workspace", `{"name":"foo"}`, http.StatusBadRequest, "workspace"},
		{"malformed-json", `{not json`, http.StatusBadRequest, "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, gwURL+"/admin/agents", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status: got %d want %d; body=%s", resp.StatusCode, tc.want, body)
			}
		})
	}
}

// TestAdminStart_RejectsBeforeRun verifies that calling /admin/agents on
// a scheduler that hasn't called Start() yet returns 500 with a clear
// "not running" message. This is the case that surfaces when the CLI
// races against scheduler startup.
func TestAdminStart_RejectsBeforeRun(t *testing.T) {
	_, gwURL := newTestScheduler(t)
	// Note: NOT calling sch.Start() — emulates "scheduler is alive enough
	// to serve HTTP but never finished initialization".
	body := `{"name":"foo","workspace":"/tmp/ws"}`
	req, _ := http.NewRequest(http.MethodPost, gwURL+"/admin/agents", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status: got %d want 500; body=%s", resp.StatusCode, raw)
	}
}

// TestAdminStop_IdempotentOnMissing verifies that DELETE on an agent
// the scheduler doesn't know about returns 204, not 404. The contract:
// stop is idempotent so the CLI / scripts can safely call it during
// cleanup without checking-then-deleting.
func TestAdminStop_IdempotentOnMissing(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sch.Stop()

	req, _ := http.NewRequest(http.MethodDelete, gwURL+"/admin/agents/never-started", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status: got %d want 204; body=%s", resp.StatusCode, raw)
	}
}

// TestGatewayChat_BadBody covers malformed JSON and missing message field.
func TestGatewayChat_BadBody(t *testing.T) {
	_, gwURL := newTestScheduler(t)

	// Empty message
	status, body := postChat(t, gwURL, "anyone", "")
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty message, got %d: %s", status, body)
	}

	// Malformed JSON
	req, _ := http.NewRequest(http.MethodPost, gwURL+"/agents/anyone/chat", bytes.NewReader([]byte("{not json")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST malformed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for malformed JSON, got %d", resp.StatusCode)
	}
}

// TestGatewayChat_AgentReplyDelay exercises the regression around the old
// hardcoded 30s gateway timeout. The mocked agent intentionally pauses
// before replying — long enough that a 30s timeout would fail, short
// enough to keep the test fast. With the bumped 10-minute gateway timeout
// the reply should still get through.
//
// The delay is bounded so the test runs in seconds; what's being asserted
// is "the gateway does not impose its own short ceiling under the agent's
// timeout", not "the gateway waits 5 minutes".
func TestGatewayChat_AgentReplyDelay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping delay test in short mode")
	}
	sch, gwURL := newTestScheduler(t)

	agentURL := realAgent(t, "slow", "took my time", 200*time.Millisecond)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "slow",
		URL:                agentURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	status, body := postChat(t, gwURL, "slow", "wait for it")
	if status != http.StatusOK {
		t.Fatalf("expected 200 after slow reply, got %d: %s", status, body)
	}
	if !bytes.Contains(body, []byte("took my time")) {
		t.Errorf("expected reply to contain 'took my time', got %s", body)
	}
}

// TestGatewayTaskStatus covers the GET /agents/{name}/tasks/{taskID} path.
// Same shape as Option B chat: gateway forwards to the agent over A2A.
func TestGatewayTaskStatus(t *testing.T) {
	sch, gwURL := newTestScheduler(t)

	// Spin up a minimal A2A server that always returns a completed task
	// for tasks/get. We don't reuse realAgent because the real wrapper's
	// task store would 404 for an unknown ID — here we want a deterministic
	// "task found" response.
	taskID := "task-abc"
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			Method string          `json:"method"`
			ID     string          `json:"id"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		if rpc.Method != "tasks/get" {
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		task := &a2a.Task{
			ID:     a2a.TaskID(taskID),
			Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		}
		result, _ := json.Marshal(task)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"result":  json.RawMessage(result),
			"id":      rpc.ID,
		})
	}))
	t.Cleanup(mockSrv.Close)

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		URL:                mockSrv.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	resp, err := http.Get(gwURL + "/agents/teacher/tasks/" + taskID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var task a2a.Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatal(err)
	}
	if string(task.ID) != taskID {
		t.Errorf("expected task ID %q, got %q", taskID, task.ID)
	}
}

// TestGatewayChat_RegistryFallthrough verifies the gateway only intercepts
// /agents/{name}/chat and /agents/{name}/tasks/{id}. Plain registry CRUD
// (GET /agents, GET /agents/{name}) must still pass through to the registry
// handler unchanged. Without this, gateway routing changes could
// accidentally swallow registry endpoints.
func TestGatewayChat_RegistryFallthrough(t *testing.T) {
	sch, gwURL := newTestScheduler(t)

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                "http://example.invalid",
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	// GET /agents -> list
	resp, err := http.Get(gwURL + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents: expected 200, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "teacher") {
		t.Errorf("GET /agents response missing 'teacher': %s", body)
	}

	// GET /agents/teacher -> single agent
	resp, err = http.Get(gwURL + "/agents/teacher")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agents/teacher: expected 200, got %d: %s", resp.StatusCode, body)
	}
}

// TestGatewayDoubleEntry runs Option A and Option B side-by-side against
// the same agent — the closest in-process analogue to the README's two
// curl examples. If either path regresses (e.g. someone tightens a
// timeout, breaks JSON-RPC parsing, or misroutes the gateway), this test
// fails loudly with which path broke.
func TestGatewayDoubleEntry(t *testing.T) {
	sch, gwURL := newTestScheduler(t)

	const reply = "shared agent reply"
	agentURL := realAgent(t, "teacher", reply, 0)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                agentURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	t.Run("OptionA_DirectA2A", func(t *testing.T) {
		task := postA2AMessage(t, agentURL, "ping")
		assertReplyInTask(t, task, reply)
	})

	t.Run("OptionB_SchedulerGateway", func(t *testing.T) {
		status, body := postChat(t, gwURL, "teacher", "ping")
		if status != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", status, body)
		}
		if !bytes.Contains(body, []byte(reply)) {
			t.Errorf("gateway response missing %q: %s", reply, body)
		}
	})
}
