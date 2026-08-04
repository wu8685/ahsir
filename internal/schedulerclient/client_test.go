package schedulerclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/wrapper"
)

func TestRefreshTimeoutAddsClientBufferForPositiveChatTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/timeouts" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":"10m0s"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	got, err := c.RefreshTimeout()
	if err != nil {
		t.Fatal(err)
	}
	if got != 11*time.Minute {
		t.Fatalf("expected client timeout 11m, got %s", got)
	}
}

func TestRefreshTimeoutZeroChatTimeoutDisablesClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":"0s"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	got, err := c.RefreshTimeout()
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("expected client timeout 0 for no-deadline scheduler chat, got %s", got)
	}
	if c.httpc.Timeout != 0 {
		t.Fatalf("expected http client timeout 0, got %s", c.httpc.Timeout)
	}
}

// --- speaker attribution (specs/2026-06-08-shared-context-collaboration.md) ---

// chatCaptureServer records the JSON body posted to /agents/{name}/chat and
// replies with a fixed chat response.
func chatCaptureServer(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"chat":"1m0s"}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChatWithAgentAsSendsSpeaker(t *testing.T) {
	var captured map[string]any
	srv := chatCaptureServer(t, &captured)

	c := NewSchedulerHTTPClient(srv.URL)
	if _, err := c.ChatWithAgentAs("teacher", "ctx-1", "alice", "hello"); err != nil {
		t.Fatal(err)
	}
	if got, _ := captured["speaker"].(string); got != "alice" {
		t.Fatalf("posted speaker = %q, want alice (body=%v)", got, captured)
	}
	if got, _ := captured["contextId"].(string); got != "ctx-1" {
		t.Fatalf("posted contextId = %q, want ctx-1", got)
	}
}

// TestChatWithAgentOmitsSpeaker pins that the legacy method keeps today's
// wire shape — no speaker key at all.
func TestChatWithAgentOmitsSpeaker(t *testing.T) {
	var captured map[string]any
	srv := chatCaptureServer(t, &captured)

	c := NewSchedulerHTTPClient(srv.URL)
	if _, err := c.ChatWithAgent("teacher", "ctx-1", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, present := captured["speaker"]; present {
		t.Fatalf("legacy ChatWithAgent must not post speaker, got %v", captured)
	}
}

// TestGetHistoryDecodesTurns pins the client side of `ahsir history`.
func TestGetHistoryDecodesTurns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agents/teacher/history/ctx-h" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"turn":1,"speaker":"alice","userText":"q","reply":"a","status":"completed"},{"turn":2,"speaker":"bob","userText":"q2","reply":"a2","status":"completed"}]`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":"1m0s"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	turns, err := c.GetHistory("teacher", "ctx-h")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Speaker != "alice" || turns[1].Speaker != "bob" {
		t.Fatalf("turns mismatch: %+v", turns)
	}
}

// --- CLI waiting modes (specs/2026-06-08-shared-context-collaboration.md) ---

func TestChatAsyncPostsAsyncFlag(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat") {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &captured)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"taskId":"task-9","contextId":"ctx-9"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":"1m0s"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	taskID, ctxID, err := c.ChatAsync("teacher", "ctx-9", "alice", "later please")
	if err != nil {
		t.Fatal(err)
	}
	if taskID != "task-9" || ctxID != "ctx-9" {
		t.Fatalf("ChatAsync = (%q, %q)", taskID, ctxID)
	}
	if got, _ := captured["async"].(bool); !got {
		t.Fatalf("posted body must set async=true, got %v", captured)
	}
	if got, _ := captured["speaker"].(string); got != "alice" {
		t.Fatalf("posted speaker = %q", got)
	}
}

func TestChatAsyncWithRequiredPathsPostsExplicitRequirements(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"taskId":"task-1","contextId":"ctx-1"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	_, _, err := c.ChatAsyncWithRequiredPaths("reviewer", "ctx-1", "alice", "review", []string{"/repo/a", "/repo/b"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := captured["requiredPaths"].([]any)
	if !ok || len(got) != 2 || got[0] != "/repo/a" || got[1] != "/repo/b" {
		t.Fatalf("requiredPaths = %#v (body=%v)", captured["requiredPaths"], captured)
	}
}

func TestStreamWithRequiredPathsUsesSchedulerA2AProxy(t *testing.T) {
	var gotPath string
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"result\":{\"kind\":\"task\",\"history\":[{\"role\":\"agent\",\"parts\":[{\"kind\":\"text\",\"text\":\"ok\"}]}]}}\n\n")
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	reply, err := c.StreamWithAgentAsRequiredPaths("reviewer", "ctx-1", "alice", "review", []string{"/repo/a"}, nil)
	if err != nil || reply != "ok" {
		t.Fatalf("stream reply=%q err=%v", reply, err)
	}
	if gotPath != "/a2a/reviewer" {
		t.Fatalf("stream path=%q", gotPath)
	}
	params := captured["params"].(map[string]any)
	message := params["message"].(map[string]any)
	metadata := message["metadata"].(map[string]any)
	paths := metadata[wrapper.MetadataRequiredFilesystemPathsKey].([]any)
	if len(paths) != 1 || paths[0] != "/repo/a" {
		t.Fatalf("stream metadata=%v", metadata)
	}
}

// TestWaitForTaskPollsToCompleted: WaitForTask polls tasks/{id} until
// terminal and returns the agent's reply text.
func TestWaitForTaskPollsToCompleted(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tasks/") {
			n := atomic.AddInt32(&polls, 1)
			w.Header().Set("Content-Type", "application/json")
			if n < 3 {
				_, _ = w.Write([]byte(`{"id":"task-9","contextId":"ctx-9","status":{"state":"working"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"task-9","contextId":"ctx-9","status":{"state":"completed"},"history":[{"messageId":"m1","role":"agent","parts":[{"kind":"text","text":"late reply"}]}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":"1m0s"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	reply, err := c.WaitForTask("teacher", "task-9", "ctx-9")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "late reply" {
		t.Fatalf("reply = %q, want late reply", reply)
	}
	if atomic.LoadInt32(&polls) < 3 {
		t.Fatalf("polls = %d, want >= 3", polls)
	}
}

// TestWaitForTask404NamesHistoryFallback pins the restart degradation path:
// a 404 while polling (agent restarted, memory-only task store) must produce
// an error that points the user at `ahsir history`.
func TestWaitForTask404NamesHistoryFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"get task task-9: task not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":"1m0s"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	_, err := c.WaitForTask("teacher", "task-9", "ctx-9")
	if err == nil {
		t.Fatal("expected degradation error on 404")
	}
	if !strings.Contains(err.Error(), "ahsir history teacher ctx-9") {
		t.Fatalf("404 error must name the history fallback, got: %v", err)
	}
}
