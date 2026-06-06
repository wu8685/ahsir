package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/wrapper"
)

const a2aProxyPrefix = "/a2a/"

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
	sch      *Scheduler
	registry http.Handler // delegate for non-gateway routes
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
	if strings.HasPrefix(r.URL.Path, a2aProxyPrefix) {
		g.handleA2AProxy(w, r)
		return
	}

	// /config/timeouts: CLI clients fetch this on startup
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

	if r.URL.Path == "/agents" && r.Method == http.MethodGet {
		g.handlePublicAgents(w, r)
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
		if r.Method == http.MethodGet {
			g.handlePublicAgents(w, r)
			return
		}
		g.registry.ServeHTTP(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	// /agents/{name} -> registry CRUD on a single agent
	if len(parts) == 1 {
		if r.Method == http.MethodGet {
			g.handlePublicAgent(w, r, parts[0])
			return
		}
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

func (g *gatewayHandler) handlePublicAgents(w http.ResponseWriter, r *http.Request) {
	cards := g.sch.registry.List()
	type cardWithStatus struct {
		*a2a.AgentCard
		Status string `json:"status"`
	}
	result := make([]cardWithStatus, len(cards))
	for i, card := range cards {
		result[i] = cardWithStatus{
			AgentCard: g.publicAgentCard(r, card),
			Status:    g.sch.registry.GetStatus(card.Name),
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (g *gatewayHandler) handlePublicAgent(w http.ResponseWriter, r *http.Request, name string) {
	card, ok := g.sch.registry.Get(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "agent not found")
		return
	}
	writeJSON(w, http.StatusOK, g.publicAgentCard(r, card))
}

func (g *gatewayHandler) publicAgentCard(r *http.Request, card *a2a.AgentCard) *a2a.AgentCard {
	if card == nil {
		return nil
	}
	publicCard := *card
	if shouldExposeViaScheduler(card.URL) {
		publicCard.URL = externalBaseURL(r) + a2aProxyPrefix + url.PathEscape(card.Name)
	}
	return &publicCard
}

func shouldExposeViaScheduler(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || host == "::1" || (ip != nil && ip.IsLoopback())
}

func externalBaseURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
}

func (g *gatewayHandler) handleA2AProxy(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, a2aProxyPrefix)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "agent name is required")
		return
	}
	if i := strings.Index(name, "/"); i >= 0 {
		name = name[:i]
	}
	decodedName, err := url.PathUnescape(name)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid agent name")
		return
	}

	card, ok := g.sch.registry.Get(decodedName)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "agent not found")
		return
	}
	target, err := url.Parse(card.URL)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "invalid agent URL: "+err.Error())
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	meta := metadataFromA2AJSON(decodedName, body)
	inv := g.sch.ledger.Begin(meta)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		g.sch.ledger.Fail(inv.ID, err)
		writeJSONError(w, http.StatusBadGateway, "create upstream request: "+err.Error())
		return
	}
	upstreamReq.Header = r.Header.Clone()
	removeHopByHopHeaders(upstreamReq.Header)
	if token := g.sch.agentInternalToken(decodedName); token != "" {
		upstreamReq.Header.Set(wrapper.InternalTokenHeader, token)
	}

	upstreamResp, err := http.DefaultTransport.RoundTrip(upstreamReq)
	if err != nil {
		g.sch.ledger.Fail(inv.ID, err)
		writeJSONError(w, http.StatusBadGateway, "proxy "+decodedName+": "+err.Error())
		return
	}
	defer upstreamResp.Body.Close()
	if upstreamResp.StatusCode >= 500 {
		g.sch.ledger.FailMessage(inv.ID, fmt.Sprintf("upstream status %d", upstreamResp.StatusCode))
	} else {
		g.sch.ledger.Complete(inv.ID)
	}

	copyHeader(w.Header(), upstreamResp.Header)
	removeHopByHopHeaders(w.Header())
	w.WriteHeader(upstreamResp.StatusCode)
	_, _ = io.Copy(flushWriter{ResponseWriter: w}, upstreamResp.Body)
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

	inv := g.sch.ledger.Begin(InvocationMetadata{
		Source:    InvocationSourceChatGateway,
		AgentName: name,
		Method:    "message/send",
		ContextID: req.ContextID,
		UserText:  req.Message,
	})
	reply, err := g.sch.ChatWithAgent(name, req.ContextID, req.Message)
	if err != nil {
		g.sch.ledger.Fail(inv.ID, err)
		// Distinguish "agent not found" from generic upstream failures so
		// callers can surface a useful error instead of a raw 500.
		if _, ok := g.sch.registry.Get(name); !ok {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	g.sch.ledger.Complete(inv.ID)

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

	// Log the inbound request BEFORE attempting the spawn so a stuck
	// startAgent (slow exec, port conflict, missing binary) is still
	// visible in the scheduler tee. The success line is already emitted
	// by startAgent itself ("Agent X started on port Y (pid: Z)").
	log.Printf("admin: start agent %q (workspace=%s, port=%d)", req.Name, req.Workspace, req.Port)

	port, err := g.sch.StartAgent(AgentConfig{
		Name:      req.Name,
		Workspace: req.Workspace,
		Port:      req.Port,
	})
	if err != nil {
		log.Printf("admin: start agent %q failed: %v", req.Name, err)
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
	// Log before StopAgent so we see the intent even if the cleanup hangs.
	// StopAgent itself is intentionally idempotent on missing agents, so
	// this line fires for both real stops and no-op cleanup calls — the
	// monitor goroutine in startAgent will emit its own "Agent X exited"
	// line when the subprocess actually dies.
	log.Printf("admin: stop agent %q", name)
	if err := g.sch.StopAgent(name); err != nil {
		log.Printf("admin: stop agent %q failed: %v", name, err)
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

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func removeHopByHopHeaders(h http.Header) {
	for _, header := range h.Values("Connection") {
		for _, field := range strings.Split(header, ",") {
			if field = strings.TrimSpace(field); field != "" {
				h.Del(field)
			}
		}
	}
	for _, header := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		h.Del(header)
	}
}

type flushWriter struct {
	http.ResponseWriter
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}
