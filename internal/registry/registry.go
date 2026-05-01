package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

// Registry manages agent registration and discovery.
type Registry struct {
	mu               sync.RWMutex
	agents           map[string]*agentEntry
	heartbeatTimeout time.Duration
}

type agentEntry struct {
	card     a2a.AgentCard
	lastBeat time.Time
}

// NewRegistry creates a new agent registry.
func NewRegistry(heartbeatTimeout time.Duration) *Registry {
	r := &Registry{
		agents:           make(map[string]*agentEntry),
		heartbeatTimeout: heartbeatTimeout,
	}
	return r
}

// Register adds or updates an agent in the registry.
func (r *Registry) Register(card a2a.AgentCard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	card.Status = "online"
	r.agents[card.Name] = &agentEntry{
		card:     card,
		lastBeat: time.Now(),
	}
}

// Get retrieves an agent by name.
func (r *Registry) Get(name string) (a2a.AgentCard, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.agents[name]
	if !ok {
		return a2a.AgentCard{}, false
	}
	card := entry.card
	if r.heartbeatTimeout > 0 && time.Since(entry.lastBeat) > r.heartbeatTimeout {
		card.Status = "offline"
	}
	return card, true
}

// List returns all registered agents.
func (r *Registry) List() []a2a.AgentCard {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cards := make([]a2a.AgentCard, 0, len(r.agents))
	for _, entry := range r.agents {
		card := entry.card
		if r.heartbeatTimeout > 0 && time.Since(entry.lastBeat) > r.heartbeatTimeout {
			card.Status = "offline"
		}
		cards = append(cards, card)
	}
	return cards
}

// Unregister removes an agent from the registry.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[name]; !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	delete(r.agents, name)
	return nil
}

// Heartbeat updates the last heartbeat time for an agent.
func (r *Registry) Heartbeat(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.agents[name]; ok {
		entry.lastBeat = time.Now()
		entry.card.Status = "online"
	}
}

// HTTPHandler returns an HTTP handler for the registry API.
type HTTPHandler struct {
	registry *Registry
}

// NewHTTPHandler creates a new HTTP handler for the registry.
func NewHTTPHandler(reg *Registry) http.Handler {
	h := &HTTPHandler{registry: reg}
	mux := http.NewServeMux()
	mux.HandleFunc("/agents", h.handleAgents)
	mux.HandleFunc("/agents/", h.handleAgent)
	return mux
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agents", h.handleAgents)
	mux.HandleFunc("/agents/", h.handleAgent)
	mux.ServeHTTP(w, r)
}

func (h *HTTPHandler) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		agents := h.registry.List()
		writeJSON(w, http.StatusOK, agents)
	case http.MethodPost:
		var card a2a.AgentCard
		if err := json.NewDecoder(r.Body).Decode(&card); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h.registry.Register(card)
		writeJSON(w, http.StatusCreated, card)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (h *HTTPHandler) handleAgent(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/agents/")
	if name == "" || name == "/" {
		h.handleAgents(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		card, ok := h.registry.Get(name)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
			return
		}
		writeJSON(w, http.StatusOK, card)
	case http.MethodDelete:
		if err := h.registry.Unregister(name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "unregistered"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
