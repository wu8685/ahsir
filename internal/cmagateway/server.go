// Package cmagateway exposes the CMA (Anthropic Managed Agents) compatible HTTP
// surface. Ported from cma-service internal/api; the eventbus mount was left
// behind in cma-service (this package is the pure CMA facade, hosted by ahsir).
package cmagateway

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/wu8685/ahsir/internal/cmagateway/ahsir"
	"github.com/wu8685/ahsir/internal/cmagateway/cma"
	"github.com/wu8685/ahsir/internal/cmagateway/store"
	"github.com/wu8685/ahsir/internal/cmagateway/translate"
)

type Server struct {
	cfg   Config
	store *store.Store
	ahsir *ahsir.Client
	rt    translate.RuntimeDefaults

	regMu      sync.Mutex
	registered map[string]bool // ahsir agent name -> registered this process
}

func New(cfg Config, st *store.Store, ac *ahsir.Client) *Server {
	return &Server{
		cfg:        cfg,
		store:      st,
		ahsir:      ac,
		rt:         translate.RuntimeDefaults{Provider: cfg.RuntimeProvider, BaseURL: cfg.RuntimeBaseURL, APIKey: cfg.RuntimeAPIKey},
		registered: map[string]bool{},
	}
}

// Handler builds the routed, authenticated http.Handler for the CMA surface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/agents", s.createAgent)
	mux.HandleFunc("GET /v1/agents", s.listAgents)
	mux.HandleFunc("GET /v1/agents/{id}", s.getAgent)
	mux.HandleFunc("POST /v1/agents/{id}", s.updateAgent)
	mux.HandleFunc("POST /v1/agents/{id}/archive", s.archiveAgent)

	mux.HandleFunc("POST /v1/environments", s.createEnvironment)
	mux.HandleFunc("GET /v1/environments", s.listEnvironments)
	mux.HandleFunc("GET /v1/environments/{id}", s.getEnvironment)
	mux.HandleFunc("POST /v1/environments/{id}", s.updateEnvironment)
	mux.HandleFunc("POST /v1/environments/{id}/archive", s.archiveEnvironment)
	mux.HandleFunc("DELETE /v1/environments/{id}", s.deleteEnvironment)

	mux.HandleFunc("POST /v1/sessions", s.createSession)
	mux.HandleFunc("GET /v1/sessions", s.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", s.getSession)
	mux.HandleFunc("POST /v1/sessions/{id}", s.updateSession)
	mux.HandleFunc("POST /v1/sessions/{id}/archive", s.archiveSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.deleteSession)

	mux.HandleFunc("POST /v1/sessions/{id}/events", s.sendEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.listEvents)
	mux.HandleFunc("GET /v1/sessions/{id}/events/stream", s.streamEvents)

	return s.auth(mux)
}

// auth gates on x-api-key. Empty allowlist => allow all (local/zero-config).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.APIKeys) > 0 {
			key := r.Header.Get("x-api-key")
			if key == "" {
				key = r.Header.Get("anthropic-api-key")
			}
			if !s.cfg.APIKeys[key] {
				writeErr(w, http.StatusUnauthorized, "authentication_error", "invalid or missing x-api-key")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ----- helpers -----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, cma.APIError{Type: "error", Error: cma.Err{Type: typ, Message: msg}})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func newEvent(typ string) cma.Event {
	now := time.Now().UTC()
	return cma.Event{ID: cma.NewID(cma.PrefixEvent), Type: typ, ProcessedAt: &now}
}
