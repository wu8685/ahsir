package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// gatewayHandler is the scheduler's user-facing HTTP entry point. It owns the
// listener, intercepts the chat and task-status endpoints, and forwards every
// other request to the registry handler so a single port serves both:
//
//	GET    /agents                         (registry: list)
//	POST   /agents                         (registry: register)
//	GET    /agents/{name}                  (registry: read)
//	DELETE /agents/{name}                  (registry: unregister)
//	POST   /agents/{name}/chat             (gateway: forward message)
//	GET    /agents/{name}/tasks/{taskID}   (gateway: forward task status)
//
// Routing is done by hand instead of via ServeMux pattern wildcards because
// builds where GODEBUG defaults to httpmuxgo121=1 (Go 1.21 ServeMux behavior)
// treat "{name}" as a literal segment.
type gatewayHandler struct {
	sch       *Scheduler
	registry  http.Handler // delegate for non-gateway routes
}

func newGatewayHandler(sch *Scheduler, registry http.Handler) *gatewayHandler {
	return &gatewayHandler{sch: sch, registry: registry}
}

// chatRequest is the body for POST /agents/{name}/chat.
//
// ContextID is optional — when set, the scheduler forwards it as the A2A
// message's contextId so the agent's SessionPool reuses an existing
// session for that conversation (cross-call memory). Empty contextId
// means each call is an isolated turn with no continuity.
type chatRequest struct {
	Message   string `json:"message"`
	ContextID string `json:"contextId,omitempty"`
}

// chatResponse is the body returned for POST /agents/{name}/chat.
type chatResponse struct {
	Response string `json:"response"`
}

func (g *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// /config/timeouts: clients (e.g. the MCP shim) fetch this on startup
	// to align their own outer-envelope http.Client.Timeout with the
	// scheduler's gateway timeout, so timeout settings live in only one
	// place (ahsir.yaml).
	if r.URL.Path == "/config/timeouts" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{
			"chat":        g.sch.cfg.Timeouts.ChatTimeout().String(),
			"task_status": g.sch.cfg.Timeouts.TaskStatusTimeout().String(),
		})
		return
	}

	// /admin/agents (POST) and /admin/agents/{name} (DELETE) — the dynamic
	// agent lifecycle API. Kept under a distinct /admin/ prefix so it
	// can't collide with the registry CRUD shape on /agents/*. No auth
	// because the scheduler is localhost-trusted in the current model;
	// if we ever bind a non-loopback address this needs a signature
	// scheme on these two endpoints.
	if strings.HasPrefix(r.URL.Path, "/admin/agents") {
		g.handleAdmin(w, r)
		return
	}

	// Only paths starting with /agents/ can possibly be a gateway request;
	// anything else (including /agents and /agents/) goes straight to registry.
	if !strings.HasPrefix(r.URL.Path, "/agents/") {
		g.registry.ServeHTTP(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/agents/")
	if rest == "" {
		g.registry.ServeHTTP(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	// /agents/{name} -> registry CRUD on a single agent
	if len(parts) == 1 {
		g.registry.ServeHTTP(w, r)
		return
	}
	name := parts[0]
	switch {
	case len(parts) == 2 && parts[1] == "chat" && r.Method == http.MethodPost:
		g.handleChat(w, r, name)
	case len(parts) == 3 && parts[1] == "tasks" && r.Method == http.MethodGet:
		g.handleTask(w, r, name, parts[2])
	default:
		// Unknown sub-resource under /agents/{name}/... — fall through to
		// registry, which will 404 / 405 as appropriate.
		g.registry.ServeHTTP(w, r)
	}
}

func (g *gatewayHandler) handleChat(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "agent name is required")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if req.Message == "" {
		writeJSONError(w, http.StatusBadRequest, "message is required")
		return
	}

	reply, err := g.sch.ChatWithAgent(name, req.ContextID, req.Message)
	if err != nil {
		// Distinguish "agent not found" from generic upstream failures so
		// callers (e.g. MCP shim) can surface a useful error instead of a
		// raw 500.
		if _, ok := g.sch.registry.Get(name); !ok {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, chatResponse{Response: reply})
}

func (g *gatewayHandler) handleTask(w http.ResponseWriter, r *http.Request, name, taskID string) {
	if name == "" || taskID == "" {
		writeJSONError(w, http.StatusBadRequest, "agent name and task id are required")
		return
	}

	task, err := g.sch.GetTaskStatus(name, taskID)
	if err != nil {
		if _, ok := g.sch.registry.Get(name); !ok {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, task)
}

// startAgentRequest is the body for POST /admin/agents — kick off a new
// agent subprocess against an existing workspace (caller is responsible
// for having scaffolded the agent-card.yaml there). port == 0 lets the
// scheduler allocate from the configured range.
type startAgentRequest struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Port      int    `json:"port,omitempty"`
}

type startAgentResponse struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// handleAdmin dispatches /admin/agents endpoints:
//
//	POST   /admin/agents          → start (body: startAgentRequest)
//	DELETE /admin/agents/{name}   → stop the named running agent
//
// Anything else returns 405.
func (g *gatewayHandler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/agents")
	switch {
	case (rest == "" || rest == "/") && r.Method == http.MethodPost:
		g.handleAdminStart(w, r)
	case strings.HasPrefix(rest, "/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(rest, "/")
		g.handleAdminStop(w, r, name)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed for "+r.URL.Path)
	}
}

func (g *gatewayHandler) handleAdminStart(w http.ResponseWriter, r *http.Request) {
	var req startAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Workspace == "" {
		writeJSONError(w, http.StatusBadRequest, "workspace is required")
		return
	}

	port, err := g.sch.StartAgent(AgentConfig{
		Name:      req.Name,
		Workspace: req.Workspace,
		Port:      req.Port,
	})
	if err != nil {
		// Distinguish "already running" (409) from misconfig (500) so the
		// CLI / caller can surface the right hint.
		msg := err.Error()
		if strings.Contains(msg, "already running") {
			writeJSONError(w, http.StatusConflict, msg)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, msg)
		return
	}
	writeJSON(w, http.StatusCreated, startAgentResponse{Name: req.Name, Port: port})
}

func (g *gatewayHandler) handleAdminStop(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := g.sch.StopAgent(name); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
