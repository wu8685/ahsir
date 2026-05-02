package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

// agentEntry tracks an agent card and its heartbeat.
type agentEntry struct {
	card     *a2a.AgentCard
	status   string
	lastBeat time.Time
}

// Registry manages agent registration and discovery.
type Registry struct {
	mu               sync.RWMutex
	agents           map[string]*agentEntry
	heartbeatTimeout time.Duration
}

// NewRegistry creates a new agent registry.
func NewRegistry(heartbeatTimeout time.Duration) *Registry {
	return &Registry{
		agents:           make(map[string]*agentEntry),
		heartbeatTimeout: heartbeatTimeout,
	}
}

// Register adds or updates an agent in the registry.
func (r *Registry) Register(card *a2a.AgentCard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agents[strings.ToLower(card.Name)] = &agentEntry{
		card:     card,
		status:   "online",
		lastBeat: time.Now(),
	}
}

// Get retrieves an agent card by name (case-insensitive).
func (r *Registry) Get(name string) (*a2a.AgentCard, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.agents[strings.ToLower(name)]
	if !ok {
		return nil, false
	}
	if r.heartbeatTimeout > 0 && time.Since(entry.lastBeat) > r.heartbeatTimeout {
		entry.status = "offline"
	}
	return entry.card, true
}

// List returns all registered agent cards.
func (r *Registry) List() []*a2a.AgentCard {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cards := make([]*a2a.AgentCard, 0, len(r.agents))
	for _, entry := range r.agents {
		if r.heartbeatTimeout > 0 && time.Since(entry.lastBeat) > r.heartbeatTimeout {
			entry.status = "offline"
		}
		cards = append(cards, entry.card)
	}
	return cards
}

// GetStatus returns the status of an agent (case-insensitive).
func (r *Registry) GetStatus(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.agents[strings.ToLower(name)]
	if !ok {
		return "unknown"
	}
	if r.heartbeatTimeout > 0 && time.Since(entry.lastBeat) > r.heartbeatTimeout {
		return "offline"
	}
	return entry.status
}

// Unregister removes an agent from the registry (case-insensitive).
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := strings.ToLower(name)
	if _, ok := r.agents[key]; !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	delete(r.agents, key)
	return nil
}

// Heartbeat updates the last heartbeat time for an agent (case-insensitive).
func (r *Registry) Heartbeat(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.agents[strings.ToLower(name)]; ok {
		entry.lastBeat = time.Now()
		entry.status = "online"
	}
}

// HTTPHandler exposes the registry API over HTTP.
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
		cards := h.registry.List()
		// Add status to each card via metadata
		type cardWithStatus struct {
			*a2a.AgentCard
			Status string `json:"status"`
		}
		result := make([]cardWithStatus, len(cards))
		for i, c := range cards {
			result[i] = cardWithStatus{
				AgentCard: c,
				Status:    h.registry.GetStatus(c.Name),
			}
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodPost:
		var card a2a.AgentCard
		if err := json.NewDecoder(r.Body).Decode(&card); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h.registry.Register(&card)
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
