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

// SetupExecutor wires the executor (Session factory + agent calling) into
// the A2A server. openSession is typically SessionPool.LookupOrCreate so
// multiple message/send calls sharing a contextID hit the same long-running
// claude process — conversation history is held by claude itself, not by
// the wrapper.
func (w *AgentWrapper) SetupExecutor(openSession func(ctx context.Context, contextID string) (Session, error), listAgents func() []*a2a.AgentCard, callAgent func(ctx context.Context, agentName, contextID, task string) (string, error), maxDepth int, basePrompt string) {
	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSession,
		ListAgents:  listAgents,
		CallAgent:   callAgent,
		MaxDepth:    maxDepth,
		BasePrompt:  basePrompt,
		SelfName:    w.agentName(),
	})
	w.server.SetExecutor(executor.Execute)
}

// agentName returns the agent's own name from the configured card, or "" if
// no card was supplied. Used to tag inter-agent log lines.
func (w *AgentWrapper) agentName() string {
	if w.cfg.AgentCard == nil {
		return ""
	}
	return w.cfg.AgentCard.Name
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
	w.server.SetSelfName(w.agentName())

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
