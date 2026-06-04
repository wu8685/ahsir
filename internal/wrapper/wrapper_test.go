package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestAgentWrapperStartStop(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	cfg := AgentWrapperConfig{
		Port:        port,
		RegistryURL: "",
		AgentCard: &a2a.AgentCard{
			Name:    "test-agent",
			Version: "1.0.0",
			URL:     fmt.Sprintf("http://127.0.0.1:%d/", port),
		},
	}

	wrapper := NewAgentWrapper(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := wrapper.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Give server time to start
	time.Sleep(100 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		t.Fatal("server not listening:", err)
	}
	conn.Close()

	if err := wrapper.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestAgentWrapperReusesSessionAcrossRequests verifies the linchpin of
// cross-request memory in the new architecture: two message/send calls
// sharing the same contextID must be routed to the same Session instance
// (via SessionPool), so the underlying claude process — not the wrapper —
// retains conversation history.
func TestAgentWrapperReusesSessionAcrossRequests(t *testing.T) {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	w := NewAgentWrapper(AgentWrapperConfig{
		Port: port,
		AgentCard: &a2a.AgentCard{
			Name:    "memo",
			Version: "1.0.0",
			URL:     fmt.Sprintf("http://127.0.0.1:%d/", port),
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Stop(ctx)

	// Build a SessionPool with a factory that records how many distinct
	// Sessions it had to produce.
	var mu sync.Mutex
	var factoryCalls int
	var prompts []string
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		mu.Lock()
		factoryCalls++
		mu.Unlock()
		// Sender accumulates prompts so we can also verify history is NOT
		// stuffed into the prompt (history belongs to claude now).
		sender := func(ctx context.Context, prompt string) (string, error) {
			mu.Lock()
			prompts = append(prompts, prompt)
			n := len(prompts)
			mu.Unlock()
			return "answer-" + fmt.Sprint(n) + "\n", nil
		}
		return NewOneshotSession(sender), nil
	}
	pool := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer pool.Stop()

	executor := NewExecutor(ExecutorConfig{
		OpenSession: pool.LookupOrCreate,
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    0,
		BasePrompt:  "you are a helper",
	})
	w.server.SetExecutor(executor.Execute)

	time.Sleep(50 * time.Millisecond)

	send := func(text, contextID string) *a2a.Task {
		t.Helper()
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: text})
		msg.ContextID = contextID
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"method":  "message/send",
			"params":  &a2a.MessageSendParams{Message: msg},
			"id":      "x",
		})
		resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/", port), "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var rpc struct {
			Result *a2a.Task `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
			t.Fatal(err)
		}
		return rpc.Result
	}

	t1 := send("what is a goroutine?", "ctx-mem")
	if t1 == nil || t1.ContextID != "ctx-mem" {
		t.Fatalf("first task did not preserve contextID: %#v", t1)
	}
	t2 := send("and a channel?", "ctx-mem")
	if t2 == nil || t2.ContextID != "ctx-mem" {
		t.Fatalf("second task did not preserve contextID: %#v", t2)
	}

	mu.Lock()
	defer mu.Unlock()
	if factoryCalls != 1 {
		t.Errorf("expected 1 factory call for two requests with same contextID, got %d", factoryCalls)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts captured, got %d", len(prompts))
	}
	for i, p := range prompts {
		if strings.Contains(p, "Conversation so far") {
			t.Errorf("prompt %d should not contain wrapper-injected history:\n%s", i, p)
		}
	}
}

func TestAgentWrapperRegisterWithRegistry(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer registry.Close()

	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	cfg := AgentWrapperConfig{
		Port:        port,
		RegistryURL: registry.URL,
		AgentCard: &a2a.AgentCard{
			Name:    "test-agent",
			Version: "1.0.0",
			URL:     fmt.Sprintf("http://127.0.0.1:%d/", port),
		},
	}

	wrapper := NewAgentWrapper(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := wrapper.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer wrapper.Stop(ctx)

	time.Sleep(100 * time.Millisecond)
}
