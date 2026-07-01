package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestAgentWrapperHealthReadyAndAgentCardEndpoints(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	card := &a2a.AgentCard{
		Name:    "health-agent",
		Version: "1.0.0",
		URL:     fmt.Sprintf("http://127.0.0.1:%d/", port),
	}
	w := NewAgentWrapper(AgentWrapperConfig{
		Port:      port,
		AgentCard: card,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Stop(ctx)
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port))

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}

	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz before executor status = %d", resp.StatusCode)
	}

	w.SetupExecutor(
		func(ctx context.Context, contextID string) (Session, error) {
			return NewOneshotSession(func(ctx context.Context, prompt string) (string, error) {
				return "ok", nil
			}), nil
		},
		func() []*a2a.AgentCard { return nil },
		nil,
		0,
		"ready",
	)

	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz after executor status = %d", resp.StatusCode)
	}

	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/.well-known/agent-card.json", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent card status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got a2a.AgentCard
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode agent card: %v body=%s", err, body)
	}
	if got.Name != card.Name {
		t.Fatalf("agent card name = %q, want %q", got.Name, card.Name)
	}
}

func TestAgentWrapperRequiresInternalTokenForA2A(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	card := &a2a.AgentCard{
		Name:    "token-agent",
		Version: "1.0.0",
		URL:     fmt.Sprintf("http://127.0.0.1:%d/", port),
	}
	w := NewAgentWrapper(AgentWrapperConfig{
		Port:          port,
		AgentCard:     card,
		InternalToken: "secret-token",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Stop(ctx)
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	w.SetupExecutor(
		func(ctx context.Context, contextID string) (Session, error) {
			return NewOneshotSession(func(ctx context.Context, prompt string) (string, error) {
				return "authorized", nil
			}), nil
		},
		func() []*a2a.AgentCard { return nil },
		nil,
		0,
		"token",
	)

	body := `{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-token","role":"user","parts":[{"kind":"text","text":"hello"}]}},"id":1}`
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/", port), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/", port), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(InternalTokenHeader, "secret-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200", resp.StatusCode)
	}
}

func waitForHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not become reachable", url)
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

// TestAgentWrapperStartFailsOnPortConflict pins the synchronous-bind
// contract: when the port is already taken, Start must return an error
// immediately instead of swallowing it in the serve goroutine, and the
// wrapper must be retryable (a second Start on a free port succeeds).
func TestAgentWrapperStartFailsOnPortConflict(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	w := NewAgentWrapper(AgentWrapperConfig{
		Port:      port,
		AgentCard: &a2a.AgentCard{Name: "conflict-agent", Version: "1.0.0"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := w.Start(ctx); err == nil {
		t.Fatal("Start must fail when the port is already in use")
		_ = w.Stop(ctx)
	}

	// The failed Start must not poison the wrapper: retry on a free port.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close()

	w.cfg.Port = freePort
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start retry on a free port must succeed, got %v", err)
	}
	_ = w.Stop(ctx)
}

// --- /history endpoint (specs/2026-06-08-shared-context-collaboration.md) ---

// TestAgentWrapperHistoryEndpoint pins the wrapper-side read path for
// transcripts: token-protected, JSON turns for a known context, empty list
// for an unknown one.
func TestAgentWrapperHistoryEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	store := NewTranscriptStore(t.TempDir())
	if err := store.Append("ctx-h", TranscriptTurn{Speaker: "alice", UserText: "q", Reply: "a", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	card := &a2a.AgentCard{
		Name:    "history-agent",
		Version: "1.0.0",
		URL:     fmt.Sprintf("http://127.0.0.1:%d/", port),
	}
	w := NewAgentWrapper(AgentWrapperConfig{
		Port:          port,
		AgentCard:     card,
		InternalToken: "secret-token",
	})
	w.SetTranscriptStore(store)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Stop(ctx)
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port))

	base := fmt.Sprintf("http://127.0.0.1:%d/history?contextId=ctx-h", port)

	// Without the internal token: 401 — transcripts hold full conversation
	// content; the token gate is the sanctioned access path.
	resp, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("history without token status = %d, want 401", resp.StatusCode)
	}

	get := func(url string) (int, []byte) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set(InternalTokenHeader, "secret-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	status, body := get(base)
	if status != http.StatusOK {
		t.Fatalf("history status = %d body=%s", status, body)
	}
	var turns []TranscriptTurn
	if err := json.Unmarshal(body, &turns); err != nil {
		t.Fatalf("decode history: %v body=%s", err, body)
	}
	if len(turns) != 1 || turns[0].Speaker != "alice" || turns[0].Reply != "a" {
		t.Fatalf("history turns mismatch: %+v", turns)
	}

	// Unknown context: empty JSON list, not an error.
	status, body = get(fmt.Sprintf("http://127.0.0.1:%d/history?contextId=never", port))
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("unknown context: status=%d body=%q, want 200 []", status, body)
	}

	// Missing contextId: 400.
	status, _ = get(fmt.Sprintf("http://127.0.0.1:%d/history", port))
	if status != http.StatusBadRequest {
		t.Fatalf("missing contextId status = %d, want 400", status)
	}
}

// TestAgentHeartbeatCarriesAdminToken pins that a configured admin token is
// attached to the registry heartbeat POST (specs/2026-06-08-auth-baseline.md).
func TestAgentHeartbeatCarriesAdminToken(t *testing.T) {
	gotHeader := make(chan string, 1)
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agents" && r.Method == http.MethodPost {
			select {
			case gotHeader <- r.Header.Get(AdminTokenHeader):
			default:
			}
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer reg.Close()

	w := NewAgentWrapper(AgentWrapperConfig{
		RegistryURL: reg.URL,
		AgentCard:   &a2a.AgentCard{Name: "hb", Version: "1.0.0", URL: "http://127.0.0.1:1/"},
		AdminToken:  "hb-token",
	})
	w.registerWithRegistry()

	select {
	case h := <-gotHeader:
		if h != "hb-token" {
			t.Fatalf("heartbeat admin token = %q, want hb-token", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("registry never received a heartbeat POST")
	}
}
