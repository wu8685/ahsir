package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

// AgentWrapperConfig configures an agent wrapper instance.
type AgentWrapperConfig struct {
	Port        int
	RegistryURL string
	AgentCard   *a2a.AgentCard
}

// AgentWrapper ties together the A2A server, task store, and registry heartbeat.
type AgentWrapper struct {
	cfg       AgentWrapperConfig
	taskStore *TaskStore
	server    *A2AServer
	httpSrv   *http.Server
	mu        sync.Mutex
	running   bool
	cancel    context.CancelFunc
}

// NewAgentWrapper creates a new agent wrapper.
func NewAgentWrapper(cfg AgentWrapperConfig) *AgentWrapper {
	return &AgentWrapper{
		cfg:       cfg,
		taskStore: NewTaskStore(),
	}
}

// TaskStore returns the wrapper's task store.
func (w *AgentWrapper) TaskStore() *TaskStore {
	return w.taskStore
}

// SetupExecutor wires the executor (LLM CLI session + agent calling + context
// memory) into the A2A server. The executor's history lookup is bound to the
// wrapper's TaskStore, so completed tasks become short-term memory for
// subsequent message/send calls sharing the same contextId.
func (w *AgentWrapper) SetupExecutor(session *SessionManager, listAgents func() []*a2a.AgentCard, callAgent func(ctx context.Context, agentName, task string) (string, error), maxDepth int, basePrompt string) {
	executor := NewExecutor(ExecutorConfig{
		SendPrompt:    session.Send,
		ListAgents:    listAgents,
		CallAgent:     callAgent,
		LookupHistory: w.taskStore.ListByContextID,
		MaxDepth:      maxDepth,
		BasePrompt:    basePrompt,
	})
	w.server.SetExecutor(executor.Execute)
}

// Start starts the HTTP server and begins registry heartbeat.
func (w *AgentWrapper) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return fmt.Errorf("wrapper already running")
	}

	ctx, w.cancel = context.WithCancel(ctx)

	w.server = NewA2AServer(w.taskStore, nil)

	mux := http.NewServeMux()
	mux.Handle("/", w.server)

	w.httpSrv = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", w.cfg.Port),
		Handler: mux,
	}

	go func() {
		if err := w.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// server error
		}
	}()

	// Start heartbeat to registry if configured
	if w.cfg.RegistryURL != "" {
		go w.heartbeatLoop(ctx)
	}

	w.running = true
	return nil
}

// Stop gracefully shuts down the HTTP server.
func (w *AgentWrapper) Stop(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return nil
	}

	if w.cancel != nil {
		w.cancel()
	}

	if w.httpSrv != nil {
		if err := w.httpSrv.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown server: %w", err)
		}
	}

	w.running = false
	return nil
}

func (w *AgentWrapper) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Register immediately
	w.registerWithRegistry()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.registerWithRegistry()
		}
	}
}

func (w *AgentWrapper) registerWithRegistry() {
	cardData, err := json.Marshal(w.cfg.AgentCard)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		w.cfg.RegistryURL+"/agents", bytes.NewReader(cardData))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
