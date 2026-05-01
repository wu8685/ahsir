package wrapper

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
	"github.com/wu8685/ahsir/internal/transport"
)

// AgentWrapperConfig configures an agent wrapper instance.
type AgentWrapperConfig struct {
	Port        int
	RegistryURL string
	AgentCard   a2a.AgentCard
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
	w.registerWithRegistry(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.registerWithRegistry(ctx)
		}
	}
}

func (w *AgentWrapper) registerWithRegistry(ctx context.Context) {
	client := transport.NewHTTPClient(w.cfg.RegistryURL+"/agents", 5*time.Second)
	req := a2a.NewJSONRPCRequest("agent/register", nil)
	// Use raw HTTP POST to register
	_, err := client.Send(ctx, req)
	if err != nil {
		// Registration failed, will retry on next tick
		_ = err
	}
}
