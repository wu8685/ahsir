package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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

// TestExampleFlow_OptionA_DirectAgentA2A keeps the internal agent A2A server
// covered. Public examples now use the scheduler-owned /a2a/{agent} endpoint;
// this direct path remains useful as a low-level wrapper regression test.
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

func TestGatewayChatRecordsInvocationLifecycle(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	release := make(chan struct{})
	upstream := blockingA2AServer(t, release, "ledger chat reply")
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	done := make(chan []byte, 1)
	go func() {
		body, _ := json.Marshal(map[string]string{
			"message":   "remember this for recovery",
			"contextId": "ctx-ledger-chat",
		})
		resp, err := http.Post(gwURL+"/agents/teacher/chat", "application/json", bytes.NewReader(body))
		if err != nil {
			done <- []byte(err.Error())
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		done <- raw
	}()

	inFlight := waitForLedgerStatus(t, sch, "teacher", InvocationStatusInFlight)
	if inFlight.Source != InvocationSourceChatGateway {
		t.Fatalf("source = %q", inFlight.Source)
	}
	if inFlight.ContextID != "ctx-ledger-chat" {
		t.Fatalf("contextID = %q", inFlight.ContextID)
	}
	if inFlight.UserText != "remember this for recovery" {
		t.Fatalf("user text = %q", inFlight.UserText)
	}

	close(release)
	raw := <-done
	if !bytes.Contains(raw, []byte("ledger chat reply")) {
		t.Fatalf("chat response missing reply: %s", raw)
	}
	completed := waitForLedgerStatus(t, sch, "teacher", InvocationStatusCompleted)
	if completed.ID != inFlight.ID {
		t.Fatalf("completed ID = %q, want %q", completed.ID, inFlight.ID)
	}
}

// TestGatewayChatMintsContextIDWhenAbsent guards the ledger half of the
// contextID fix: a chat with no contextId must still be recorded against a real
// (minted) contextId, not an empty one — otherwise `trace`/contexts grouping
// never sees the turn.
func TestGatewayChatMintsContextIDWhenAbsent(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	release := make(chan struct{})
	upstream := blockingA2AServer(t, release, "minted ctx reply")
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	done := make(chan []byte, 1)
	go func() {
		// No contextId in the body — the gateway must mint one.
		body, _ := json.Marshal(map[string]string{"message": "no context supplied"})
		resp, err := http.Post(gwURL+"/agents/teacher/chat", "application/json", bytes.NewReader(body))
		if err != nil {
			done <- []byte(err.Error())
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		done <- raw
	}()

	inFlight := waitForLedgerStatus(t, sch, "teacher", InvocationStatusInFlight)
	if inFlight.ContextID == "" {
		t.Fatal("gateway must mint a contextID when the caller omits one; ledger recorded empty")
	}
	close(release)
	<-done
}

// TestGatewayRooms_CreateDriveStop exercises the roundtable HTTP surface end to
// end: create a room with an opening @-mention, watch the addressed agent take
// its turn (driven through the normal chat path), then stop the room.
func TestGatewayRooms_CreateDriveStop(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	agentURL := realAgent(t, "alice", "alice here, nothing more", 0)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "alice",
		URL:                agentURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	// Reject a non-registered participant.
	body, _ := json.Marshal(map[string]any{"participants": []string{"ghost"}})
	resp, err := http.Post(gwURL+"/rooms", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ghost participant: status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Create a room and kick it off addressing alice.
	body, _ = json.Marshal(map[string]any{
		"topic":        "demo",
		"participants": []string{"alice"},
		"organizer":    "operator",
		"message":      "welcome, @alice please open",
	})
	resp, err = http.Post(gwURL+"/rooms", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: status = %d, body=%s", resp.StatusCode, raw)
	}
	var room RoomView
	json.NewDecoder(resp.Body).Decode(&room)
	resp.Body.Close()
	if room.ID == "" || room.Organizer != "operator" {
		t.Fatalf("unexpected room view: %+v", room)
	}

	// Poll until alice has taken her turn and the room parks (she addressed no
	// one, organizer is the operator).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get(gwURL + "/rooms/" + room.ID)
		if err != nil {
			t.Fatal(err)
		}
		var v RoomView
		json.NewDecoder(r.Body).Decode(&v)
		r.Body.Close()
		if v.Status == RoomWaiting && len(v.Transcript) == 2 {
			if v.Transcript[0].Speaker != "operator" || v.Transcript[1].Speaker != "alice" {
				t.Fatalf("transcript speakers wrong: %+v", v.Transcript)
			}
			if !strings.Contains(v.Transcript[1].Text, "alice here") {
				t.Fatalf("alice's turn text wrong: %q", v.Transcript[1].Text)
			}
			goto stop
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("room never reached waiting with alice's turn")

stop:
	// The room turn must be recorded in the ledger (keyed on the room id, which
	// is the contextId) so `trace`/the console 轨迹 panel can see roundtable
	// activity — roundtable turns bypass the gateway chat handler that normally
	// writes the ledger.
	var roundtableRecs int
	for _, rec := range sch.ledger.Snapshot() {
		if rec.ContextID == room.ID && rec.Source == InvocationSourceRoundtable {
			roundtableRecs++
			if rec.AgentName != "alice" {
				t.Errorf("roundtable ledger agent = %q, want alice", rec.AgentName)
			}
		}
	}
	if roundtableRecs == 0 {
		t.Fatal("no roundtable invocation recorded in ledger for the room contextId")
	}

	resp, err = http.Post(gwURL+"/rooms/"+room.ID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var stopped RoomView
	json.NewDecoder(resp.Body).Decode(&stopped)
	resp.Body.Close()
	if stopped.Status != RoomStopped {
		t.Fatalf("status after stop = %q, want stopped", stopped.Status)
	}
}

func TestGatewayA2AProxyRecordsInvocationLifecycle(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	release := make(chan struct{})
	upstream := blockingA2AServer(t, release, "ledger proxy reply")
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	done := make(chan []byte, 1)
	go func() {
		body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-ledger-a2a","contextId":"ctx-ledger-a2a","role":"user","parts":[{"kind":"text","text":"proxy text for recovery"}]}},"id":1}`)
		resp, err := http.Post(gwURL+"/a2a/teacher", "application/json", bytes.NewReader(body))
		if err != nil {
			done <- []byte(err.Error())
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		done <- raw
	}()

	inFlight := waitForLedgerStatus(t, sch, "teacher", InvocationStatusInFlight)
	if inFlight.Source != InvocationSourceA2AProxy {
		t.Fatalf("source = %q", inFlight.Source)
	}
	if inFlight.Method != "message/send" {
		t.Fatalf("method = %q", inFlight.Method)
	}
	if inFlight.ContextID != "ctx-ledger-a2a" {
		t.Fatalf("contextID = %q", inFlight.ContextID)
	}
	if inFlight.MessageID != "msg-ledger-a2a" {
		t.Fatalf("messageID = %q", inFlight.MessageID)
	}
	if inFlight.UserText != "proxy text for recovery" {
		t.Fatalf("user text = %q", inFlight.UserText)
	}

	close(release)
	raw := <-done
	if !bytes.Contains(raw, []byte("ledger proxy reply")) {
		t.Fatalf("A2A response missing reply: %s", raw)
	}
	completed := waitForLedgerStatus(t, sch, "teacher", InvocationStatusCompleted)
	if completed.ID != inFlight.ID {
		t.Fatalf("completed ID = %q, want %q", completed.ID, inFlight.ID)
	}
}

func TestGatewayA2AProxyPersistsInvocationLedgerFile(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: 0},
		PortRange: PortRange{Start: 9801, End: 9900},
		path:      filepath.Join(dir, "ahsir.yaml"),
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	regHandler := registry.NewHTTPHandler(sch.Registry())
	gw := newGatewayHandler(sch, regHandler)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	upstream := realAgent(t, "teacher", "persistent ledger reply", 0)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		URL:                upstream,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-persist-a2a","contextId":"ctx-persist-a2a","role":"user","parts":[{"kind":"text","text":"persist this proxy call"}]}},"id":1}`)
	resp, err := http.Post(srv.URL+"/a2a/teacher", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rawResp, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, rawResp)
	}

	// Completion is recorded only after the response body has been fully
	// relayed (so interrupted SSE streams don't get logged as successes),
	// which the client's body read can race with — wait for the ledger to
	// settle before asserting on the file.
	waitForLedgerStatus(t, sch, "teacher", InvocationStatusCompleted)

	ledgerPath := filepath.Join(dir, ".ahsir", "ledger.jsonl")
	rawLedger, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger file: %v", err)
	}
	ledger := string(rawLedger)
	for _, marker := range []string{
		`"type":"started"`,
		`"source":"a2a_proxy"`,
		`"agentName":"teacher"`,
		`"contextId":"ctx-persist-a2a"`,
		`"messageId":"msg-persist-a2a"`,
		`"type":"completed"`,
	} {
		if !strings.Contains(ledger, marker) {
			t.Fatalf("ledger file missing %q\n--- ledger ---\n%s", marker, ledger)
		}
	}

	replayed, err := NewInvocationLedgerFromFile(ledgerPath)
	if err != nil {
		t.Fatalf("replay ledger: %v", err)
	}
	snapshot := replayed.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("replayed snapshot len = %d, want 1: %+v", len(snapshot), snapshot)
	}
	rec := snapshot[0]
	if rec.Status != InvocationStatusCompleted || rec.Source != InvocationSourceA2AProxy || rec.ContextID != "ctx-persist-a2a" {
		t.Fatalf("unexpected replayed record: %+v", rec)
	}
}

func TestGatewayA2AProxyRecordsFailedInvocation(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	port := freeGatewayTestPort(t)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		URL:                fmt.Sprintf("http://127.0.0.1:%d/", port),
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-fail","contextId":"ctx-fail","role":"user","parts":[{"kind":"text","text":"will fail"}]}},"id":1}`)
	resp, err := http.Post(gwURL+"/a2a/teacher", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 502, got %d: %s", resp.StatusCode, raw)
	}

	failed := waitForLedgerStatus(t, sch, "teacher", InvocationStatusFailed)
	if failed.Error == "" {
		t.Fatal("expected failed invocation to record error detail")
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

func TestGatewayRegistryReturnsSchedulerA2AURLs(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	agentURL := realAgent(t, "teacher", "public card target", 0)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                agentURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	resp, err := http.Get(gwURL + "/agents/teacher")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /agents/teacher: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card.URL != gwURL+"/a2a/teacher" {
		t.Fatalf("public card URL = %q, want %q", card.URL, gwURL+"/a2a/teacher")
	}

	resp, err = http.Get(gwURL + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /agents: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var cards []struct {
		*a2a.AgentCard
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cards); err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 {
		t.Fatalf("expected one card, got %d", len(cards))
	}
	if cards[0].URL != gwURL+"/a2a/teacher" {
		t.Fatalf("public list URL = %q, want %q", cards[0].URL, gwURL+"/a2a/teacher")
	}
}

func TestGatewayA2AProxyForwardsNativeMessageSend(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	agentURL := realAgent(t, "teacher", "proxied native A2A reply", 0)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                agentURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	task := postA2AMessage(t, gwURL+"/a2a/teacher", "ping through scheduler")
	if state, _ := task["status"].(map[string]any)["state"].(string); state != string(a2a.TaskStateCompleted) {
		t.Errorf("expected task state=completed, got %q", state)
	}
	assertReplyInTask(t, task, "proxied native A2A reply")
}

func TestGatewayA2AProxyAddsInternalToken(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	var sawToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Ahsir-Internal-Token")
		writeTestA2AReply(t, w, "token accepted")
	}))
	defer upstream.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})
	// Managed agent reached at its recorded local port (cfg.Port); the
	// registry card URL is not the token destination.
	sch.agents["teacher"] = &agentProcess{
		cfg:           AgentConfig{Name: "teacher", Port: portOfURL(t, upstream.URL)},
		internalToken: "scheduler-token",
	}

	task := postA2AMessage(t, gwURL+"/a2a/teacher", "ping through scheduler")
	assertReplyInTask(t, task, "token accepted")
	if sawToken != "scheduler-token" {
		t.Fatalf("proxy token header = %q, want scheduler-token", sawToken)
	}
}

func writeTestA2AReply(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	task := a2a.NewSubmittedTask(a2a.TaskInfo{}, nil)
	task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	task.History = []*a2a.Message{
		a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: text}),
	}
	result, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"result":  json.RawMessage(result),
		"id":      "test",
	})
}

func blockingA2AServer(t *testing.T, release <-chan struct{}, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		writeTestA2AReply(t, w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitForLedgerStatus(t *testing.T, sch *Scheduler, agent string, status InvocationStatus) InvocationRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, inv := range sch.Invocations().Snapshot() {
			if inv.AgentName == agent && inv.Status == status {
				return inv
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for ledger status %s for agent %s; snapshot=%+v", status, agent, sch.Invocations().Snapshot())
	return InvocationRecord{}
}

func freeGatewayTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func useFastHealthyAgent(t *testing.T, sch *Scheduler, starts *atomic.Int32) {
	t.Helper()
	sch.supervisor.HealthStartupGrace = 0
	sch.supervisor.HealthInterval = 10 * time.Millisecond
	sch.supervisor.HealthTimeout = 20 * time.Millisecond
	base := healthAgentCommand(filepath.Join(t.TempDir(), "health-starts.log"), "healthy", 0)
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		starts.Add(1)
		return base(ctx, agentExe, cfg, registryURL)
	}
}

func TestGatewayA2AProxySchedulerNotFoundHasMachineMarker(t *testing.T) {
	_, gwURL := newTestScheduler(t)
	resp, err := http.Post(gwURL+"/a2a/missing", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get(SchedulerErrorCodeHeader); got != SchedulerErrorAgentNotFound {
		t.Fatalf("scheduler error marker = %q, want %q", got, SchedulerErrorAgentNotFound)
	}
}

func TestGatewayA2AProxyUpstreamNotFoundHasNoSchedulerMarker(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This response header belongs to the scheduler control plane. An
		// upstream must not be able to forge it and make CMA replay a request
		// that already reached the Agent.
		w.Header().Set(SchedulerErrorCodeHeader, SchedulerErrorAgentNotFound)
		writeJSONError(w, http.StatusNotFound, "agent not found")
	}))
	defer upstream.Close()
	sch.Registry().Register(&a2a.AgentCard{Name: "upstream-404", URL: upstream.URL})

	resp, err := http.Post(gwURL+"/a2a/upstream-404", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want upstream 404", resp.StatusCode)
	}
	if got := resp.Header.Get(SchedulerErrorCodeHeader); got != "" {
		t.Fatalf("upstream forged scheduler marker was forwarded: %q", got)
	}
}

func TestGatewayRegistryLeavesRemoteAgentURLUnchanged(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	remoteURL := "http://192.0.2.10:9801/"
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "remote-teacher",
		Version:            "1.0.0",
		URL:                remoteURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	resp, err := http.Get(gwURL + "/agents/remote-teacher")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /agents/remote-teacher: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card.URL != remoteURL {
		t.Fatalf("remote card URL = %q, want %q", card.URL, remoteURL)
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

// TestGatewayInvocationsEndpoint pins the /invocations read API that backs
// `ahsir trace`: filterable by contextId, bounded userText, duration present
// on settled records.
func TestGatewayInvocationsEndpoint(t *testing.T) {
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: 0},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	gw := newGatewayHandler(sch, registry.NewHTTPHandler(sch.Registry()))
	srv := httptest.NewServer(gw)
	defer srv.Close()

	inv1 := sch.ledger.Begin(InvocationMetadata{
		Source: InvocationSourceChatGateway, AgentName: "teacher",
		Method: "message/send", ContextID: "ctx-a", UserText: "hello",
	})
	// durationMs is omitempty — give the invocation a measurable duration so
	// the settled record is asserted to carry it.
	time.Sleep(5 * time.Millisecond)
	sch.ledger.Complete(inv1.ID)
	inv2 := sch.ledger.Begin(InvocationMetadata{
		Source: InvocationSourceA2AProxy, AgentName: "student",
		Method: "message/send", ContextID: "ctx-b", UserText: "other",
	})
	sch.ledger.FailMessage(inv2.ID, "boom")

	fetch := func(query string) []map[string]any {
		resp, err := http.Get(srv.URL + "/invocations" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var out []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	all := fetch("")
	if len(all) != 2 {
		t.Fatalf("want 2 invocations, got %d", len(all))
	}

	filtered := fetch("?contextId=ctx-a")
	if len(filtered) != 1 {
		t.Fatalf("want 1 invocation for ctx-a, got %d", len(filtered))
	}
	rec := filtered[0]
	if rec["agentName"] != "teacher" || rec["status"] != "completed" || rec["userText"] != "hello" {
		t.Errorf("unexpected record: %+v", rec)
	}
	if _, ok := rec["durationMs"]; !ok {
		t.Errorf("settled record must carry durationMs: %+v", rec)
	}

	failedRec := fetch("?contextId=ctx-b")[0]
	if failedRec["status"] != "failed" || failedRec["error"] != "boom" {
		t.Errorf("unexpected failed record: %+v", failedRec)
	}
}

// TestLedgerBoundsUserText pins the redaction contract: the ledger stores a
// bounded preview of the user's message, never the full text.
func TestLedgerBoundsUserText(t *testing.T) {
	l := NewInvocationLedger()
	long := strings.Repeat("好", 1000) // multi-byte: also exercises rune-safe cut
	rec := l.Begin(InvocationMetadata{AgentName: "a", UserText: long})
	if len(rec.UserText) > 512+len("…[truncated]") {
		t.Fatalf("UserText not bounded: %d bytes", len(rec.UserText))
	}
	if !strings.HasSuffix(rec.UserText, "…[truncated]") {
		t.Fatalf("UserText missing truncation marker: %q", rec.UserText[:40])
	}
	if !utf8.ValidString(rec.UserText) {
		t.Fatal("truncation split a UTF-8 rune")
	}
}

// TestGatewayChat_BusyMapsTo409 pins the gateway half of the same-context
// busy contract: an upstream error carrying the "agent busy:" wire marker
// becomes HTTP 409 Conflict with the readable message in the JSON body —
// distinguishable from 404 (unknown agent) and 502 (generic failure).
func TestGatewayChat_BusyMapsTo409(t *testing.T) {
	sch, gwURL := newTestScheduler(t)

	agentURL := busyAgent(t)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                agentURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	status, body := postChat(t, gwURL, "teacher", "hello while busy")
	if status != http.StatusConflict {
		t.Fatalf("busy upstream must map to 409, got %d: %s", status, body)
	}
	if !strings.Contains(string(body), "agent busy") {
		t.Errorf("409 body must carry the busy message, got: %s", body)
	}
}

// busyAgent is a realAgent variant whose every turn fails with the busy
// sentinel — simulating a same-context collision inside the agent process.
func busyAgent(t *testing.T) string {
	t.Helper()
	taskStore := wrapper.NewTaskStore()
	exec := wrapper.NewExecutor(wrapper.ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (wrapper.Session, error) {
			return wrapper.NewOneshotSession(func(ctx context.Context, prompt string) (string, error) {
				return "", wrapper.ErrTurnInFlight
			}), nil
		},
		ListAgents: func() []*a2a.AgentCard { return nil },
		MaxDepth:   0,
		BasePrompt: "busy",
	})
	srv := httptest.NewServer(wrapper.NewA2AServer(taskStore, exec.Execute))
	t.Cleanup(srv.Close)
	return srv.URL
}

// --- speaker attribution (specs/2026-06-08-shared-context-collaboration.md) ---

// TestGatewayChatForwardsSpeaker pins the full speaker plumbing through the
// chat path: HTTP body → scheduler → outbound A2A message metadata, plus the
// ledger record and the /invocations view.
func TestGatewayChatForwardsSpeaker(t *testing.T) {
	sch, gwURL := newTestScheduler(t)

	var capturedMeta map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Message struct {
					Metadata map[string]any `json:"metadata"`
				} `json:"message"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &rpc)
		capturedMeta = rpc.Params.Message.Metadata
		writeTestA2AReply(t, w, "speaker accepted")
	}))
	defer upstream.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	body, _ := json.Marshal(map[string]string{
		"message":   "hello from alice",
		"contextId": "ctx-speaker-chat",
		"speaker":   "alice",
	})
	resp, err := http.Post(gwURL+"/agents/teacher/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("chat status = %d: %s", resp.StatusCode, raw)
	}

	if got, _ := capturedMeta["speaker"].(string); got != "alice" {
		t.Fatalf("upstream metadata speaker = %q, want alice (metadata=%v)", got, capturedMeta)
	}

	completed := waitForLedgerStatus(t, sch, "teacher", InvocationStatusCompleted)
	if completed.Speaker != "alice" {
		t.Fatalf("ledger speaker = %q, want alice", completed.Speaker)
	}

	// /invocations must expose the speaker so `ahsir trace` can render it.
	invResp, err := http.Get(gwURL + "/invocations?contextId=ctx-speaker-chat")
	if err != nil {
		t.Fatal(err)
	}
	defer invResp.Body.Close()
	var views []map[string]any
	if err := json.NewDecoder(invResp.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("invocation views = %d, want 1", len(views))
	}
	if got, _ := views[0]["speaker"].(string); got != "alice" {
		t.Fatalf("/invocations speaker = %q, want alice (view=%v)", got, views[0])
	}
}

// TestGatewayA2AProxyRecordsSpeaker pins that a native A2A message/send with
// Metadata["speaker"] gets attributed in the ledger by the proxy path.
func TestGatewayA2AProxyRecordsSpeaker(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestA2AReply(t, w, "proxy speaker accepted")
	}))
	defer upstream.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "native hi"})
	msg.ContextID = "ctx-speaker-proxy"
	msg.Metadata = map[string]any{"speaker": "bob"}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "message/send",
		"params":  &a2a.MessageSendParams{Message: msg},
		"id":      "test",
	})
	resp, err := http.Post(gwURL+"/a2a/teacher", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}

	completed := waitForLedgerStatus(t, sch, "teacher", InvocationStatusCompleted)
	if completed.Speaker != "bob" {
		t.Fatalf("ledger speaker = %q, want bob", completed.Speaker)
	}
}

// TestGatewayHistoryProxy pins GET /agents/{name}/history/{contextId}: the
// scheduler proxies the wrapper's /history endpoint, attaching the agent's
// internal token (the transcript holds full conversation content — the
// scheduler, not the caller, owns that credential).
func TestGatewayHistoryProxy(t *testing.T) {
	sch, gwURL := newTestScheduler(t)

	var sawToken, sawContextID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/history" {
			t.Errorf("upstream path = %q, want /history", r.URL.Path)
		}
		sawToken = r.Header.Get(wrapper.InternalTokenHeader)
		sawContextID = r.URL.Query().Get("contextId")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"turn":1,"speaker":"alice","userText":"q","reply":"a","status":"completed"}]`))
	}))
	defer upstream.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})
	// Managed agent: the token-bearing history request goes to the
	// scheduler-recorded local address (cfg.Port), not the registry card URL.
	sch.agents["teacher"] = &agentProcess{
		cfg:           AgentConfig{Name: "teacher", Port: portOfURL(t, upstream.URL)},
		internalToken: "history-token",
	}

	resp, err := http.Get(gwURL + "/agents/teacher/history/ctx-h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d body=%s", resp.StatusCode, body)
	}
	if sawToken != "history-token" {
		t.Errorf("upstream token = %q, want history-token", sawToken)
	}
	if sawContextID != "ctx-h" {
		t.Errorf("upstream contextId = %q, want ctx-h", sawContextID)
	}
	var turns []wrapper.TranscriptTurn
	if err := json.Unmarshal(body, &turns); err != nil {
		t.Fatalf("decode history body: %v body=%s", err, body)
	}
	if len(turns) != 1 || turns[0].Speaker != "alice" {
		t.Fatalf("history turns mismatch: %+v", turns)
	}

	// Unknown agent: 404.
	resp2, err := http.Get(gwURL + "/agents/nobody/history/ctx-h")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent history status = %d, want 404", resp2.StatusCode)
	}
}

// --- async chat (specs/2026-06-08-shared-context-collaboration.md) ---

// asyncStubAgent answers message/send with a submitted task (recording
// whether blocking=false arrived) and tasks/get with the completed task.
func asyncStubAgent(t *testing.T, taskID string, sawNonBlocking *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
			Params struct {
				Configuration struct {
					Blocking *bool `json:"blocking"`
				} `json:"configuration"`
				Message struct {
					ContextID string `json:"contextId"`
				} `json:"message"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &rpc)
		switch rpc.Method {
		case "message/send":
			if rpc.Params.Configuration.Blocking != nil && !*rpc.Params.Configuration.Blocking {
				*sawNonBlocking = true
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      "t",
				"result": map[string]any{
					"kind":      "task",
					"id":        taskID,
					"contextId": rpc.Params.Message.ContextID,
					"status":    map[string]any{"state": "submitted"},
				},
			})
		case "tasks/get":
			writeJSON(w, http.StatusOK, map[string]any{
				"jsonrpc": "2.0",
				"id":      "t",
				"result": map[string]any{
					"kind":      "task",
					"id":        taskID,
					"contextId": "ctx-async-chat",
					"status":    map[string]any{"state": "completed"},
					"history": []map[string]any{
						{"kind": "message", "messageId": "m-reply", "role": "agent", "parts": []map[string]any{{"kind": "text", "text": "async reply"}}},
					},
				},
			})
		default:
			t.Errorf("unexpected method %q", rpc.Method)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGatewayChatAsync pins the async chat contract: 202 + taskId without
// waiting, ledger passes through queued, and settles to completed once the
// agent-side task finishes.
func TestGatewayChatAsync(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	var sawNonBlocking bool
	upstream := asyncStubAgent(t, "task-async-1", &sawNonBlocking)

	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                upstream.URL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	body, _ := json.Marshal(map[string]any{
		"message":   "do it later",
		"contextId": "ctx-async-chat",
		"speaker":   "alice",
		"async":     true,
	})
	resp, err := http.Post(gwURL+"/agents/teacher/chat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("async chat status = %d, want 202; body=%s", resp.StatusCode, raw)
	}
	var out struct {
		TaskID    string `json:"taskId"`
		ContextID string `json:"contextId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.TaskID != "task-async-1" || out.ContextID != "ctx-async-chat" {
		t.Fatalf("async response = %+v", out)
	}
	if !sawNonBlocking {
		t.Error("upstream never saw configuration.blocking=false")
	}

	// Ledger: the invocation settles to completed via the background poll.
	completed := waitForLedgerStatus(t, sch, "teacher", InvocationStatusCompleted)
	if completed.Speaker != "alice" {
		t.Errorf("async invocation speaker = %q, want alice", completed.Speaker)
	}
}

// --- admin token enforcement (specs/2026-06-08-auth-baseline.md) ---

// enforcingScheduler returns a scheduler+gateway with a known admin token set,
// simulating an auth-enabled deployment.
func enforcingScheduler(t *testing.T, token string) (*Scheduler, string) {
	t.Helper()
	sch, gwURL := newTestScheduler(t)
	sch.adminTok = token
	sch.adminTokSource = "test"
	return sch, gwURL
}

func doReq(t *testing.T, method, url, token string, body []byte) int {
	t.Helper()
	var r *http.Request
	var err error
	if body != nil {
		r, err = http.NewRequest(method, url, bytes.NewReader(body))
	} else {
		r, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set(AdminTokenHeader, token)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return resp.StatusCode
}

func TestAdminEndpointRequiresToken(t *testing.T) {
	_, gwURL := enforcingScheduler(t, "secret-admin")

	startBody, _ := json.Marshal(map[string]string{"name": "x", "workspace": "/tmp/x"})

	// No token → 401, before any start logic runs.
	if st := doReq(t, http.MethodPost, gwURL+"/admin/agents", "", startBody); st != http.StatusUnauthorized {
		t.Errorf("POST /admin/agents no token = %d, want 401", st)
	}
	// Wrong token → 401.
	if st := doReq(t, http.MethodPost, gwURL+"/admin/agents", "nope", startBody); st != http.StatusUnauthorized {
		t.Errorf("POST /admin/agents wrong token = %d, want 401", st)
	}
	// DELETE no token → 401.
	if st := doReq(t, http.MethodDelete, gwURL+"/admin/agents/x", "", nil); st != http.StatusUnauthorized {
		t.Errorf("DELETE /admin/agents/x no token = %d, want 401", st)
	}
}

// Reconciliation may race an already-running scheduler Agent. The admin start
// endpoint must compare the requested immutable card and instance cap before
// touching disk: an incompatible request returns a distinguishable 409 and
// leaves the configuration consumed by the running process unchanged.
func TestAdminStart_IncompatibleExistingAgentDoesNotOverwriteCard(t *testing.T) {
	tests := []struct {
		name               string
		requestedSystem    string
		requestedInstances int
	}{
		{name: "card mismatch", requestedSystem: "new system", requestedInstances: 2},
		{name: "instances mismatch", requestedSystem: "old system", requestedInstances: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sch, gwURL := newTestScheduler(t)
			workspace := t.TempDir()
			const agentName = "cma-persisted-v4"
			existingCard := &wrapper.AgentCardConfig{
				Name: agentName, Version: "4",
				Claude:  wrapper.ClaudeConfig{SystemPrompt: "old system"},
				Runtime: wrapper.RuntimeConfig{Provider: "echo", Model: "old-model"},
			}
			if err := wrapper.WriteCard(workspace, existingCard); err != nil {
				t.Fatal(err)
			}
			existingCfg := AgentConfig{Name: agentName, Workspace: workspace, Port: 9801, Instances: 2}
			sch.mu.Lock()
			sch.running = true
			sch.ctx = context.Background()
			sch.agents[agentName] = &agentProcess{cfg: existingCfg, cancel: func() {}}
			sch.desired[agentName] = existingCfg
			sch.mu.Unlock()

			requestedCard := *existingCard
			requestedCard.Claude.SystemPrompt = tc.requestedSystem
			body, err := json.Marshal(startAgentRequest{
				Name: agentName, Workspace: workspace, Instances: tc.requestedInstances, Card: &requestedCard,
			})
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, gwURL+"/admin/agents", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			responseBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", resp.StatusCode, responseBody)
			}
			if !strings.Contains(strings.ToLower(string(responseBody)), "incompatible") {
				t.Fatalf("409 is not machine/operator distinguishable as incompatible: %s", responseBody)
			}

			got, err := wrapper.NewAgentCardBuilder(workspace).Load()
			if err != nil {
				t.Fatal(err)
			}
			if got.Claude.SystemPrompt != existingCard.Claude.SystemPrompt || got.Runtime.Model != existingCard.Runtime.Model {
				t.Fatalf("incompatible reconcile overwrote running card: got system=%q model=%q", got.Claude.SystemPrompt, got.Runtime.Model)
			}
			if gotCfg := sch.desired[agentName]; gotCfg.Instances != existingCfg.Instances {
				t.Fatalf("incompatible reconcile changed instances to %d, want %d", gotCfg.Instances, existingCfg.Instances)
			}
		})
	}
}

// A repeated registration is the normal CMA reconciliation path after the
// facade loses its process-local cache. If the immutable card and instance cap
// still match, POST /admin/agents is an idempotent ensure operation: it must
// succeed without restarting the live process or even rewriting its card.
func TestAdminStart_CompatibleExistingAgentIsIdempotent(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	sch.supervisor.HealthStartupGrace = 0
	sch.supervisor.HealthInterval = 10 * time.Millisecond
	sch.supervisor.HealthTimeout = 20 * time.Millisecond
	var healthChecks atomic.Int32
	healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthChecks.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer healthSrv.Close()
	healthURL, err := url.Parse(healthSrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	healthPort, err := strconv.Atoi(healthURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	const agentName = "cma-persisted-v4"
	card := &wrapper.AgentCardConfig{
		Name: agentName, Version: "4",
		Claude:  wrapper.ClaudeConfig{SystemPrompt: "stable system"},
		Runtime: wrapper.RuntimeConfig{Provider: "echo", Model: "stable-model"},
	}
	if err := wrapper.WriteCard(workspace, card); err != nil {
		t.Fatal(err)
	}
	cardPath := filepath.Join(workspace, ".a2a", "agent-card.yaml")
	originalModTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(cardPath, originalModTime, originalModTime); err != nil {
		t.Fatal(err)
	}

	existingCfg := AgentConfig{Name: agentName, Workspace: workspace, Port: healthPort, Instances: 2}
	existingProc := &agentProcess{cfg: existingCfg, cancel: func() {}}
	sch.mu.Lock()
	sch.running = true
	sch.ctx = context.Background()
	sch.agents[agentName] = existingProc
	sch.desired[agentName] = existingCfg
	sch.mu.Unlock()

	body, err := json.Marshal(startAgentRequest{
		Name: agentName, Workspace: workspace, Instances: 2, Card: card,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(gwURL+"/admin/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("compatible ensure status = %d, want 200/201; body=%s", resp.StatusCode, responseBody)
	}
	if healthChecks.Load() == 0 {
		t.Fatal("compatible running ensure returned success without checking readiness")
	}

	sch.mu.Lock()
	gotProc := sch.agents[agentName]
	gotCfg := sch.desired[agentName]
	sch.mu.Unlock()
	if gotProc != existingProc {
		t.Fatal("compatible ensure restarted or replaced the running process")
	}
	if gotCfg.Instances != existingCfg.Instances || gotCfg.Port != existingCfg.Port {
		t.Fatalf("compatible ensure mutated desired config: got %+v, want %+v", gotCfg, existingCfg)
	}
	info, err := os.Stat(cardPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(originalModTime) {
		t.Fatalf("compatible ensure rewrote card: modtime=%s, want %s", info.ModTime(), originalModTime)
	}
}

// Two facades (or two concurrent turns after scheduler-state loss) can race to
// reconcile the same versioned Agent. Identical registrations must coalesce at
// the scheduler boundary: both callers observe success and exactly one process
// is spawned.
func TestAdminStart_ConcurrentIdenticalRegistrationStartsOnce(t *testing.T) {
	registryPort := freeGatewayTestPort(t)
	agentPort := freeGatewayTestPort(t)
	for agentPort == registryPort {
		agentPort = freeGatewayTestPort(t)
	}
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: registryPort},
		PortRange: PortRange{Start: agentPort, End: agentPort},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	var starts atomic.Int32
	useFastHealthyAgent(t, sch, &starts)
	ctx, cancel := context.WithTimeout(context.Background(), testLifecycleDeadline)
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	gw := newGatewayHandler(sch, registry.NewHTTPHandler(sch.Registry()))
	srv := httptest.NewServer(gw)
	defer srv.Close()
	workspace := t.TempDir()
	card := &wrapper.AgentCardConfig{
		Name: "cma-race-v1", Version: "1",
		Claude:  wrapper.ClaudeConfig{SystemPrompt: "same system"},
		Runtime: wrapper.RuntimeConfig{Provider: "echo", Model: "same-model"},
	}
	body, err := json.Marshal(startAgentRequest{
		Name: card.Name, Workspace: workspace, Instances: 2, Card: card,
	})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	statuses := make([]int, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := http.Post(srv.URL+"/admin/agents", "application/json", bytes.NewReader(body))
			if err != nil {
				errs[i] = err
				return
			}
			statuses[i] = resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			errs[i] = resp.Body.Close()
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range statuses {
		if errs[i] != nil {
			t.Fatalf("registration %d failed: %v", i, errs[i])
		}
		if statuses[i] != http.StatusOK && statuses[i] != http.StatusCreated {
			t.Errorf("registration %d status = %d, want 200/201", i, statuses[i])
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("identical concurrent registration started %d processes, want 1", got)
	}
}

// Desired state alone is not proof that a runtime exists. This is the drift
// shape left by a lost process (or a pending supervisor restart): reconciliation
// must fill the missing runtime atomically, while concurrent callers still
// coalesce to one spawn.
func TestAdminStart_CompatibleDesiredWithoutRuntimeStartsOnce(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	workspace := t.TempDir()
	port := freeGatewayTestPort(t)
	card := &wrapper.AgentCardConfig{
		Name: "cma-drift-v1", Version: "1",
		Claude:  wrapper.ClaudeConfig{SystemPrompt: "same system"},
		Runtime: wrapper.RuntimeConfig{Provider: "echo", Model: "same-model"},
	}
	if err := wrapper.WriteCard(workspace, card); err != nil {
		t.Fatal(err)
	}
	existingCfg := AgentConfig{Name: card.Name, Workspace: workspace, Port: port, Instances: 2}
	var starts atomic.Int32
	useFastHealthyAgent(t, sch, &starts)
	sch.mu.Lock()
	sch.running = true
	sch.ctx = context.Background()
	sch.desired[card.Name] = existingCfg
	sch.mu.Unlock()
	t.Cleanup(func() {
		sch.mu.Lock()
		if proc := sch.agents[card.Name]; proc != nil {
			proc.stopping = true
			killAgentProcess(proc)
			proc.cancel()
		}
		sch.mu.Unlock()
	})

	body, err := json.Marshal(startAgentRequest{
		Name: card.Name, Workspace: workspace, Instances: 2, Card: card,
	})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	statuses := make([]int, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := http.Post(gwURL+"/admin/agents", "application/json", bytes.NewReader(body))
			if err != nil {
				errs[i] = err
				return
			}
			statuses[i] = resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			errs[i] = resp.Body.Close()
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range statuses {
		if errs[i] != nil {
			t.Fatalf("registration %d failed: %v", i, errs[i])
		}
		if statuses[i] != http.StatusOK && statuses[i] != http.StatusCreated {
			t.Errorf("registration %d status = %d, want 200/201", i, statuses[i])
		}
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("compatible desired reconciliation started %d processes, want 1", got)
	}
	sch.mu.Lock()
	proc := sch.agents[card.Name]
	sch.mu.Unlock()
	if proc == nil {
		t.Fatal("compatible desired reconciliation returned success without a runtime")
	}
}

func TestAdminStart_CompatibleIdleStoppedWakesBeforeSuccess(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	workspace := t.TempDir()
	card := &wrapper.AgentCardConfig{Name: "cma-idle-v1", Version: "1", Runtime: wrapper.RuntimeConfig{Provider: "echo"}}
	if err := wrapper.WriteCard(workspace, card); err != nil {
		t.Fatal(err)
	}
	cfg := AgentConfig{Name: card.Name, Workspace: workspace, Port: freeGatewayTestPort(t)}
	var starts atomic.Int32
	useFastHealthyAgent(t, sch, &starts)
	sch.mu.Lock()
	sch.running = true
	sch.ctx = context.Background()
	sch.desired[card.Name] = cfg
	sch.idleStopped[card.Name] = cfg
	sch.mu.Unlock()
	t.Cleanup(func() { sch.Stop() })

	body, _ := json.Marshal(startAgentRequest{Name: card.Name, Workspace: workspace, Card: card})
	resp, err := http.Post(gwURL+"/admin/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("idle ensure status=%d body=%s", resp.StatusCode, raw)
	}
	if starts.Load() != 1 {
		t.Fatalf("idle ensure starts=%d, want 1", starts.Load())
	}
	if got := sch.IdleStoppedAgents(); len(got) != 0 {
		t.Fatalf("idle ensure returned before wake completed: %v", got)
	}
}

func TestAdminStart_NewAgentReadinessFailureRollsBackAndReturnsError(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	workspace := t.TempDir()
	card := &wrapper.AgentCardConfig{Name: "cma-not-ready-v1", Version: "1", Runtime: wrapper.RuntimeConfig{Provider: "echo"}}
	port := freeGatewayTestPort(t)
	sch.cfg.PortRange = PortRange{Start: port, End: port}
	sch.cfg.nextPort = port
	sch.supervisor.HealthStartupGrace = 0
	sch.supervisor.HealthInterval = 10 * time.Millisecond
	sch.supervisor.HealthTimeout = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	sch.mu.Lock()
	sch.running = true
	sch.ctx = ctx
	sch.mu.Unlock()
	sch.agentCommand = func(ctx context.Context, agentExe string, cfg AgentConfig, registryURL string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}

	body, _ := json.Marshal(startAgentRequest{Name: card.Name, Workspace: workspace, Card: card})
	resp, err := http.Post(gwURL+"/admin/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 300 {
		t.Fatalf("unready new Agent was acknowledged: status=%d body=%s", resp.StatusCode, raw)
	}
	sch.mu.Lock()
	_, running := sch.agents[card.Name]
	sch.mu.Unlock()
	if running {
		t.Fatal("unready new Agent was not rolled back")
	}
}

func TestAdminStart_DifferentNameCannotReuseDesiredWorkspace(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	workspace := t.TempDir()
	existing := &wrapper.AgentCardConfig{Name: "existing", Version: "1", Runtime: wrapper.RuntimeConfig{Provider: "echo"}}
	if err := wrapper.WriteCard(workspace, existing); err != nil {
		t.Fatal(err)
	}
	existingCfg := AgentConfig{Name: existing.Name, Workspace: workspace, Port: 9801}
	sch.mu.Lock()
	sch.running = true
	sch.ctx = context.Background()
	sch.agents[existing.Name] = &agentProcess{cfg: existingCfg, cancel: func() {}}
	sch.desired[existing.Name] = existingCfg
	sch.mu.Unlock()

	requested := &wrapper.AgentCardConfig{Name: "other", Version: "1", Runtime: wrapper.RuntimeConfig{Provider: "echo"}}
	body, _ := json.Marshal(startAgentRequest{Name: requested.Name, Workspace: workspace, Card: requested})
	resp, err := http.Post(gwURL+"/admin/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("workspace reuse status=%d, want 409; body=%s", resp.StatusCode, raw)
	}
	got, err := wrapper.NewAgentCardBuilder(workspace).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != existing.Name {
		t.Fatalf("workspace reuse overwrote existing card name=%q", got.Name)
	}
}

func TestAdminStart_DifferentNameCannotReuseWorkspaceThroughSymlink(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	realWorkspace := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(realWorkspace, alias); err != nil {
		t.Fatal(err)
	}
	existing := &wrapper.AgentCardConfig{Name: "existing", Version: "1", Runtime: wrapper.RuntimeConfig{Provider: "echo"}}
	if err := wrapper.WriteCard(realWorkspace, existing); err != nil {
		t.Fatal(err)
	}
	existingCfg := AgentConfig{Name: existing.Name, Workspace: realWorkspace, Port: 9801}
	sch.mu.Lock()
	sch.running = true
	sch.ctx = context.Background()
	sch.agents[existing.Name] = &agentProcess{cfg: existingCfg, cancel: func() {}}
	sch.desired[existing.Name] = existingCfg
	sch.mu.Unlock()

	requested := &wrapper.AgentCardConfig{Name: "other", Version: "1", Runtime: wrapper.RuntimeConfig{Provider: "echo"}}
	body, _ := json.Marshal(startAgentRequest{Name: requested.Name, Workspace: alias, Card: requested})
	resp, err := http.Post(gwURL+"/admin/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("symlink workspace reuse status=%d, want 409; body=%s", resp.StatusCode, raw)
	}
	got, err := wrapper.NewAgentCardBuilder(realWorkspace).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != existing.Name {
		t.Fatalf("symlink workspace reuse overwrote existing card name=%q", got.Name)
	}
}

// The legacy pre-staged-card API supplies no inline card to verify. Preserve
// its historical conflict behavior for an existing Agent instead of treating
// an unverifiable registration as compatible success.
func TestAdminStart_ExistingAgentWithoutInlineCardConflicts(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	workspace := t.TempDir()
	const agentName = "legacy-agent"
	existingCfg := AgentConfig{Name: agentName, Workspace: workspace, Port: 9801}
	existingProc := &agentProcess{cfg: existingCfg, cancel: func() {}}
	sch.mu.Lock()
	sch.running = true
	sch.ctx = context.Background()
	sch.agents[agentName] = existingProc
	sch.desired[agentName] = existingCfg
	sch.mu.Unlock()

	body, err := json.Marshal(startAgentRequest{Name: agentName, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(gwURL+"/admin/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("legacy duplicate status = %d, want 409; body=%s", resp.StatusCode, responseBody)
	}
	sch.mu.Lock()
	gotProc := sch.agents[agentName]
	sch.mu.Unlock()
	if gotProc != existingProc {
		t.Fatal("legacy duplicate registration replaced the running process")
	}
}

func TestRegistryWriteRequiresToken(t *testing.T) {
	sch, gwURL := enforcingScheduler(t, "secret-admin")

	// Pre-seed an existing card so we can prove an unauthorized overwrite is
	// blocked (the card stays unchanged).
	sch.Registry().Register(&a2a.AgentCard{Name: "teacher", Version: "1.0.0", URL: "http://127.0.0.1:1/"})

	overwrite, _ := json.Marshal(&a2a.AgentCard{Name: "teacher", Version: "9.9.9", URL: "http://evil/"})

	// POST /agents without token → 401.
	if st := doReq(t, http.MethodPost, gwURL+"/agents", "", overwrite); st != http.StatusUnauthorized {
		t.Errorf("POST /agents no token = %d, want 401", st)
	}
	// The existing card must be unchanged (overwrite blocked).
	if card, _ := sch.Registry().Get("teacher"); card == nil || card.URL != "http://127.0.0.1:1/" {
		t.Errorf("unauthorized overwrite mutated the card: %+v", card)
	}

	// DELETE /agents/{name} without token → 401.
	if st := doReq(t, http.MethodDelete, gwURL+"/agents/teacher", "", nil); st != http.StatusUnauthorized {
		t.Errorf("DELETE /agents/teacher no token = %d, want 401", st)
	}
	if card, _ := sch.Registry().Get("teacher"); card == nil {
		t.Error("unauthorized DELETE removed the agent")
	}

	// With the token, registration succeeds (this is the agent heartbeat path).
	if st := doReq(t, http.MethodPost, gwURL+"/agents", "secret-admin", overwrite); st != http.StatusCreated && st != http.StatusOK {
		t.Errorf("POST /agents with token = %d, want 200/201", st)
	}
}

func TestDataPlaneStaysOpenUnderAuth(t *testing.T) {
	sch, gwURL := enforcingScheduler(t, "secret-admin")
	sch.Registry().Register(&a2a.AgentCard{Name: "teacher", Version: "1.0.0", URL: "http://127.0.0.1:1/"})

	// GET endpoints must NOT require the token.
	for _, path := range []string{"/agents", "/agents/teacher", "/invocations", "/config/timeouts"} {
		if st := doReq(t, http.MethodGet, gwURL+path, "", nil); st == http.StatusUnauthorized {
			t.Errorf("GET %s returned 401 — data/read plane must stay open", path)
		}
	}
}

func TestNoAuthWhenTokenEmpty(t *testing.T) {
	// A bare scheduler (adminTok == "") must pass control-plane requests
	// through — preserves zero-config local behaviour and existing tests.
	_, gwURL := newTestScheduler(t) // adminTok defaults to ""
	startBody, _ := json.Marshal(map[string]string{"name": "x", "workspace": "/tmp/x"})
	if st := doReq(t, http.MethodPost, gwURL+"/admin/agents", "", startBody); st == http.StatusUnauthorized {
		t.Error("empty admin token must disable enforcement, got 401")
	}
}
