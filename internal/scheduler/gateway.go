package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/wu8685/ahsir/internal/obs"
	"github.com/wu8685/ahsir/internal/wrapper"
)

const a2aProxyPrefix = "/a2a/"

// AdminTokenHeader carries the control-plane admin token on requests to the
// privileged endpoints (/admin/agents lifecycle + registry write). See
// specs/2026-06-08-auth-baseline.md.
const AdminTokenHeader = "X-Ahsir-Admin-Token"

// adminAuthorized reports whether a request may invoke a control-plane
// operation. When no admin token is configured (s.adminToken() == "") auth is
// disabled and every request passes — the degenerate local/zero-config case.
func (g *gatewayHandler) adminAuthorized(r *http.Request) bool {
	token := g.sch.adminToken()
	if token == "" {
		return true
	}
	return r.Header.Get(AdminTokenHeader) == token
}

// isRegistryWrite reports whether a request is a mutating registry operation
// (POST /agents, DELETE /agents/{name}) — the writes that must be gated. GET
// reads and the gateway's own chat/task/history routes are not registry writes.
func isRegistryWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost:
		return r.URL.Path == "/agents"
	case http.MethodDelete, http.MethodPut, http.MethodPatch:
		// /agents/{name} mutations, but not deeper gateway routes
		// (/agents/{name}/chat etc. are POST and handled before delegation).
		rest := strings.TrimPrefix(r.URL.Path, "/agents/")
		return rest != "" && !strings.Contains(rest, "/")
	}
	return false
}

// maxRequestBodyBytes bounds every gateway request body. Without a cap a
// single oversized POST buffers unbounded memory (the A2A proxy reads the
// whole body for ledger metadata extraction before forwarding). 10 MiB is
// far above any realistic prompt while still bounding the damage.
const maxRequestBodyBytes = 10 << 20

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
	// Speaker is the self-claimed sender identity for shared-context
	// attribution (`ahsir chat --as`). Forwarded as A2A message metadata
	// and recorded in the ledger. Advisory at the local-machine trust
	// level — becomes the verified principal when ingress auth lands.
	Speaker string `json:"speaker,omitempty"`
	// Async submits the turn without waiting: the response is 202 with the
	// taskId to poll (`ahsir status` / tasks/get) instead of the reply.
	Async bool `json:"async,omitempty"`
}

// chatResponse is the body returned for POST /agents/{name}/chat.
type chatResponse struct {
	Response string `json:"response"`
}

// asyncChatResponse is the 202 body for async chat: the handle to poll and
// the (possibly agent-generated) contextId for transcript lookups.
type asyncChatResponse struct {
	TaskID    string `json:"taskId"`
	ContextID string `json:"contextId,omitempty"`
}

func (g *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read-only Prometheus scrape endpoint (§6.B lock-in item 2: each process
	// exposes its own /metrics; the scheduler does not proxy-aggregate).
	if r.URL.Path == "/metrics" && r.Method == http.MethodGet {
		promhttp.HandlerFor(g.sch.MetricsGatherer(), promhttp.HandlerOpts{
			EnableOpenMetrics: true, // required for exemplar exposition
		}).ServeHTTP(w, r)
		return
	}

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

	// /invocations: read-only view over the scheduler's invocation ledger.
	// Backs `ahsir trace <contextId>` (and `ahsir trace` for a recent
	// overview). UserText in these records is already bounded at write time
	// (see boundUserText), so this endpoint never exposes more than the
	// ledger file itself holds.
	if r.URL.Path == "/invocations" && r.Method == http.MethodGet {
		g.handleInvocations(w, r)
		return
	}
	if r.URL.Path == "/context-events" && r.Method == http.MethodGet {
		g.handleContextEvents(w, r)
		return
	}
	if r.URL.Path == "/context-events/stream" && r.Method == http.MethodGet {
		g.handleContextEventsStream(w, r)
		return
	}

	// /archived-agents: read-only view of offline agents (deleted/stopped) whose
	// managed workspaces still hold transcripts under .ahsir/agents/*. Backs the
	// console's "归档" section. No admin token — it exposes no more than the
	// transcript files themselves, same posture as /invocations and /history.
	if r.URL.Path == "/archived-agents" && r.Method == http.MethodGet {
		g.handleArchivedAgents(w, r)
		return
	}

	// /rooms: roundtable (multi-agent group chat) engine. Read/dispatch routes,
	// not control plane — no admin token (same posture as chat). The console
	// reaches these through its /api/* reverse proxy.
	if r.URL.Path == "/rooms" || strings.HasPrefix(r.URL.Path, "/rooms/") {
		g.handleRooms(w, r)
		return
	}

	// /admin/agents (POST) and /admin/agents/{name} (DELETE) — the dynamic
	// agent lifecycle API. Kept under a distinct /admin/ prefix so it
	// can't collide with the registry CRUD shape on /agents/*. No auth
	// because the scheduler is localhost-trusted in the current model;
	// if we ever bind a non-loopback address this needs a signature
	// scheme on these two endpoints.
	if strings.HasPrefix(r.URL.Path, "/admin/agents") {
		if !g.adminAuthorized(r) {
			writeJSONError(w, http.StatusUnauthorized, "admin token required ("+AdminTokenHeader+")")
			return
		}
		g.handleAdmin(w, r)
		return
	}

	// Registry write (POST /agents, DELETE /agents/{name}) is control plane —
	// gate it before delegating. GET reads stay open. This is also the agent
	// heartbeat path; scheduler-spawned agents present the token.
	if isRegistryWrite(r) && !g.adminAuthorized(r) {
		writeJSONError(w, http.StatusUnauthorized, "admin token required ("+AdminTokenHeader+")")
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
	case len(parts) == 3 && parts[1] == "history" && r.Method == http.MethodGet:
		g.handleHistory(w, r, name, parts[2])
	case len(parts) == 2 && parts[1] == "config" && r.Method == http.MethodGet:
		g.handleAgentConfig(w, r, name)
	default:
		// Unknown sub-resource under /agents/{name}/... — fall through to
		// registry, which will 404 / 405 as appropriate.
		g.registry.ServeHTTP(w, r)
	}
}

// invocationView is the wire shape for one ledger record on /invocations.
type invocationView struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	AgentName  string `json:"agentName"`
	Method     string `json:"method,omitempty"`
	ContextID  string `json:"contextId,omitempty"`
	MessageID  string `json:"messageId,omitempty"`
	UserText   string `json:"userText,omitempty"`
	Speaker    string `json:"speaker,omitempty"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
	DurationMS int64  `json:"durationMs,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (g *gatewayHandler) handleInvocations(w http.ResponseWriter, r *http.Request) {
	contextID := r.URL.Query().Get("contextId")
	records := g.sch.ledger.Snapshot()
	out := make([]invocationView, 0, len(records))
	for _, rec := range records {
		if contextID != "" && rec.ContextID != contextID {
			continue
		}
		v := invocationView{
			ID:        rec.ID,
			Source:    string(rec.Source),
			AgentName: rec.AgentName,
			Method:    rec.Method,
			ContextID: rec.ContextID,
			MessageID: rec.MessageID,
			UserText:  rec.UserText,
			Speaker:   rec.Speaker,
			Status:    string(rec.Status),
			StartedAt: rec.StartedAt.Format(time.RFC3339),
		}
		if !rec.FinishedAt.IsZero() && !rec.StartedAt.IsZero() {
			v.DurationMS = rec.FinishedAt.Sub(rec.StartedAt).Milliseconds()
		}
		v.Error = rec.Error
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *gatewayHandler) handlePublicAgents(w http.ResponseWriter, r *http.Request) {
	cards := g.sch.registry.List()
	type cardWithStatus struct {
		*a2a.AgentCard
		Status string `json:"status"`
	}
	result := make([]cardWithStatus, 0, len(cards))
	for _, card := range cards {
		// Hide pooled instance children (base#n): a card that backs a worker pool
		// still presents as a single agent to users and to peer-agent discovery
		// (issue #18). Callers chat the base name; the scheduler fans out.
		if isInstanceChild(card.Name) {
			continue
		}
		result = append(result, cardWithStatus{
			AgentCard: g.publicAgentCard(r, card),
			Status:    g.sch.registry.GetStatus(card.Name),
		})
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

	// Activator: transparently wake an idle-stopped agent BEFORE resolving its
	// dial target, so an A2A dispatch after a scale-to-zero re-spawns the
	// runtime on a fresh process/port instead of dialing the dead cached
	// endpoint (issue #20). The CLI chat path already does this via
	// ChatWithAgentAs; the public /a2a/{agent} proxy — the path Hetairoi's
	// autonomous loop dispatches through — was the one entrypoint that skipped
	// it, so the first dispatch after any idle period reliably hit
	// "connection refused" and the session went terminal. ensureAwake is a
	// no-op when the agent is already up or was explicitly stopped/never
	// existed; the agentDialTarget lookup below still decides genuine not-found.
	if err := g.sch.ensureAwake(decodedName); err != nil {
		writeJSONError(w, http.StatusBadGateway, "wake "+decodedName+": "+err.Error())
		return
	}

	// Resolve via agentDialTarget so a scheduler-managed agent is reached at
	// its recorded local address (not the registry card URL, which an
	// unauthenticated registration can overwrite) and only such agents
	// receive the internal token.
	card, internalToken, ok := g.sch.agentDialTarget(decodedName)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "agent not found")
		return
	}
	target, err := url.Parse(card.URL)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "invalid agent URL: "+err.Error())
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
			return
		}
		writeJSONError(w, http.StatusBadRequest, "read request body: "+err.Error())
		return
	}
	meta := metadataFromA2AJSON(decodedName, body)
	inv := g.sch.ledger.Begin(meta)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		// Constructing our own request failed — an internal fault, not the
		// upstream's. Label at the site (§6 red line: never parse it back out).
		g.sch.ledger.FailResult(inv.ID, obs.ResultInternalError, err)
		writeJSONError(w, http.StatusBadGateway, "create upstream request: "+err.Error())
		return
	}
	upstreamReq.Header = r.Header.Clone()
	removeHopByHopHeaders(upstreamReq.Header)
	if internalToken != "" {
		upstreamReq.Header.Set(wrapper.InternalTokenHeader, internalToken)
	}

	upstreamResp, err := http.DefaultTransport.RoundTrip(upstreamReq)
	if err != nil {
		// Reaching the agent failed (connection refused / caller ctx gone).
		// Cancellation is the caller giving up, not an upstream fault.
		g.sch.ledger.FailResult(inv.ID, classifyProxyError(err), err)
		writeJSONError(w, http.StatusBadGateway, "proxy "+decodedName+": "+err.Error())
		return
	}
	defer upstreamResp.Body.Close()

	copyHeader(w.Header(), upstreamResp.Header)
	removeHopByHopHeaders(w.Header())
	w.WriteHeader(upstreamResp.StatusCode)
	var copyErr error
	if strings.Contains(upstreamResp.Header.Get("Content-Type"), "text/event-stream") {
		copyErr = copyObservedSSE(flushWriter{ResponseWriter: w}, upstreamResp.Body, func(payload []byte) {
			g.observeA2AFrame(inv, payload)
		})
	} else {
		_, copyErr = io.Copy(flushWriter{ResponseWriter: w}, upstreamResp.Body)
	}

	// Settle the ledger only AFTER the body has been relayed: for SSE
	// streams the interesting failures happen mid-copy, and marking the
	// invocation complete at header time would record interrupted streams
	// as successes — recovery and `trace` views would then lie.
	switch {
	case upstreamResp.StatusCode >= 500:
		g.sch.ledger.FailMessageResult(inv.ID, obs.ResultUpstreamError, fmt.Sprintf("upstream status %d", upstreamResp.StatusCode))
	case copyErr != nil:
		g.sch.ledger.FailMessageResult(inv.ID, obs.ResultUpstreamError, fmt.Sprintf("response stream interrupted: %v", copyErr))
	default:
		g.sch.ledger.Complete(inv.ID)
	}
}

func copyObservedSSE(w io.Writer, r io.Reader, onData func([]byte)) error {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := w.Write(line); writeErr != nil {
				return writeErr
			}
			trimmed := bytes.TrimSpace(line)
			if bytes.HasPrefix(trimmed, []byte("data:")) && onData != nil {
				onData(bytes.TrimSpace(trimmed[len("data:"):]))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (g *gatewayHandler) observeA2AFrame(inv InvocationRecord, payload []byte) {
	var frame struct {
		Result struct {
			Kind   string `json:"kind"`
			ID     string `json:"id"`
			TaskID string `json:"taskId"`
			Status struct {
				State   string `json:"state"`
				Message *struct {
					Parts []struct {
						Kind string                     `json:"kind"`
						Text string                     `json:"text"`
						Data map[string]json.RawMessage `json:"data"`
					} `json:"parts"`
				} `json:"message"`
			} `json:"status"`
		} `json:"result"`
	}
	if json.Unmarshal(payload, &frame) != nil {
		return
	}
	base := LiveEvent{InvocationID: inv.ID, ContextID: inv.ContextID, AgentName: inv.AgentName}
	if frame.Result.Kind == "task" {
		base.Type, base.State = "terminal", frame.Result.Status.State
		g.sch.liveEvents.Publish(base)
		return
	}
	if frame.Result.Kind != "status-update" {
		return
	}
	if frame.Result.Status.Message == nil || len(frame.Result.Status.Message.Parts) == 0 {
		base.Type, base.State = "status", frame.Result.Status.State
		g.sch.liveEvents.Publish(base)
		return
	}
	for _, part := range frame.Result.Status.Message.Parts {
		ev := base
		if part.Kind == "text" {
			ev.Type, ev.Content = "text_delta", part.Text
			g.sch.liveEvents.Publish(ev)
			continue
		}
		if part.Kind != "data" {
			continue
		}
		_ = json.Unmarshal(part.Data["ev"], &ev.Type)
		_ = json.Unmarshal(part.Data["id"], &ev.ToolUseID)
		_ = json.Unmarshal(part.Data["tool_use_id"], &ev.ToolUseID)
		_ = json.Unmarshal(part.Data["name"], &ev.Name)
		_ = json.Unmarshal(part.Data["content"], &ev.Content)
		_ = json.Unmarshal(part.Data["is_error"], &ev.IsError)
		if raw := part.Data["input"]; len(raw) > 0 {
			ev.Input = append(json.RawMessage(nil), raw...)
		}
		if ev.Type != "" {
			g.sch.liveEvents.Publish(ev)
		}
	}
}

// classifyProxyError buckets a transport-level failure reaching an agent into
// the §7 taxonomy at the site it occurs. Caller cancellation/timeout is the
// caller's own doing (not an upstream fault); everything else is upstream.
func classifyProxyError(err error) obs.Result {
	switch {
	case errors.Is(err, context.Canceled):
		return obs.ResultCancel
	case errors.Is(err, context.DeadlineExceeded):
		return obs.ResultTimeout
	default:
		return obs.ResultUpstreamError
	}
}

// classifyChatError buckets a chat-path error into the §7 taxonomy at the
// gateway. Turn-level types (wrapper.ErrTurnInFlight) don't survive the A2A
// process boundary, so the "agent busy:" wire marker — the same signal
// writeChatError uses for its 409 — is how the gateway recognizes backpressure
// here. This is NOT the forbidden "parse the ledger's free-text error" (§6):
// it is the established cross-process wire contract, evaluated at the site.
func classifyChatError(err error) obs.Result {
	switch {
	case err == nil:
		return obs.ResultDone
	case errors.Is(err, context.Canceled):
		return obs.ResultCancel
	case errors.Is(err, context.DeadlineExceeded):
		return obs.ResultTimeout
	case strings.Contains(err.Error(), "agent busy:"):
		return obs.ResultBusy
	default:
		return obs.ResultUpstreamError
	}
}

func (g *gatewayHandler) handleChat(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "agent name is required")
		return
	}

	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}
	if req.Message == "" {
		writeJSONError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Own the contextID when the caller didn't supply one. Leaving it empty
	// used to defer id-minting to the agent, with two problems: the ledger
	// recorded an empty contextID (so `trace`/contexts grouping never saw the
	// turn), and the async path minted the id twice — the handle returned to
	// the caller didn't match the contextID the transcript was stored under
	// (see wrapper.sendNonBlocking). Minting here, once, before anything else
	// uses it keeps the ledger record, the agent session, the transcript, and
	// the returned handle all keyed on the same id. UUIDv7 mirrors what the
	// a2a library mints, so the shape is unchanged for callers.
	if req.ContextID == "" {
		req.ContextID = uuid.Must(uuid.NewV7()).String()
	}

	inv := g.sch.ledger.Begin(InvocationMetadata{
		Source:    InvocationSourceChatGateway,
		AgentName: name,
		Method:    "message/send",
		ContextID: req.ContextID,
		UserText:  req.Message,
		Speaker:   req.Speaker,
	})

	// Async: submit, mark queued, settle in the background, answer 202 with
	// the task handle. The error mapping below is shared with the sync path.
	if req.Async {
		task, release, err := g.sch.ChatWithAgentAsync(name, req.ContextID, req.Speaker, req.Message)
		if err != nil {
			g.sch.ledger.FailResult(inv.ID, classifyChatError(err), err)
			g.writeChatError(w, name, req.ContextID, err)
			return
		}
		g.sch.ledger.Queued(inv.ID)
		// release holds the instance pool's in-flight count until the async turn
		// settles (issue #18) — hand it to the settler.
		go g.sch.settleAsyncInvocation(inv.ID, name, string(task.ID), release)
		writeJSON(w, http.StatusAccepted, asyncChatResponse{
			TaskID:    string(task.ID),
			ContextID: task.ContextID,
		})
		return
	}

	reply, err := g.sch.ChatWithAgentAs(name, req.ContextID, req.Speaker, req.Message)
	if err != nil {
		g.sch.ledger.FailResult(inv.ID, classifyChatError(err), err)
		g.writeChatError(w, name, req.ContextID, err)
		return
	}
	g.sch.ledger.Complete(inv.ID)

	writeJSON(w, http.StatusOK, chatResponse{Response: reply})
}

// writeChatError maps upstream chat errors onto HTTP statuses — shared by
// the sync and async chat paths.
func (g *gatewayHandler) writeChatError(w http.ResponseWriter, name, contextID string, err error) {
	// Same-context collision: with queueing enabled this now means the turn
	// QUEUE is full (or queue_depth=0 kept fail-fast semantics). 409 (not
	// 502) so callers can tell "retry shortly" apart from "the agent is
	// broken". Error types don't survive the A2A boundary — the
	// "agent busy:" substring is the wire marker (see wrapper.ErrTurnInFlight).
	if strings.Contains(err.Error(), "agent busy:") {
		msg := fmt.Sprintf("agent busy: a turn is already running for this conversation (contextId %q); retry after the current turn completes", contextID)
		writeJSONError(w, http.StatusConflict, msg)
		return
	}
	// Distinguish "agent not found" from generic upstream failures so
	// callers can surface a useful error instead of a raw 500.
	if _, ok := g.sch.registry.Get(name); !ok {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSONError(w, http.StatusBadGateway, err.Error())
}

// handleHistory proxies GET /agents/{name}/history/{contextId} to the agent's
// transcript endpoint. Backs `ahsir history <agent> <contextId>`.
// handleAgentConfig serves an agent's agent-card.yaml (read-only) plus its
// on-disk path, so the console can show the full config and tell the operator
// which file to edit. Editing happens in a real editor; the console only reads.
func (g *gatewayHandler) handleAgentConfig(w http.ResponseWriter, r *http.Request, name string) {
	path, data, err := g.sch.AgentConfigFile(name)
	if err != nil {
		status := http.StatusNotFound
		if path != "" {
			// Agent exists but the file couldn't be read — report the path too.
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]string{"error": err.Error(), "path": path})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path, "yaml": string(data)})
}

func (g *gatewayHandler) handleArchivedAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := g.sch.ArchivedAgents()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agents == nil {
		agents = []ArchivedAgent{}
	}
	writeJSON(w, http.StatusOK, agents)
}

func (g *gatewayHandler) handleHistory(w http.ResponseWriter, r *http.Request, name, rawContextID string) {
	contextID, err := url.PathUnescape(rawContextID)
	if err != nil || contextID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid contextId")
		return
	}
	// An agent that isn't registered can't be proxied — but if it's an
	// archived/offline agent its transcripts persist on disk, so serve those
	// read-only. This is what keeps a deleted agent's history viewable.
	if _, ok := g.sch.registry.Get(name); !ok {
		turns, err := g.sch.ArchivedAgentHistory(name, contextID)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if turns == nil {
			turns = []wrapper.TranscriptTurn{}
		}
		writeJSON(w, http.StatusOK, turns)
		return
	}
	turns, err := g.sch.AgentHistory(name, contextID)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	if turns == nil {
		turns = []wrapper.TranscriptTurn{}
	}
	writeJSON(w, http.StatusOK, turns)
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
//
// Card enables dynamic (inline) registration: when present, the scheduler
// scaffolds the workspace and writes its .a2a/agent-card.yaml from this card
// before spawning, so callers (e.g. cma-service, which registers one agent per
// CMA agent version) need not pre-stage anything on disk. When Card is set and
// Workspace is empty, a managed workspace is allocated under .ahsir/agents/.
type startAgentRequest struct {
	Name      string `json:"name"`
	Workspace string `json:"workspace"`
	Workdir   string `json:"workdir,omitempty"`
	Port      int    `json:"port,omitempty"`
	// Instances caps how many concurrent runtime instances this card may back
	// (issue #18). Zero/1 = single instance (unchanged). >1 lets the scheduler
	// pool isolated-workspace instances on demand for safe parallel dispatch.
	Instances int                      `json:"instances,omitempty"`
	Card      *wrapper.AgentCardConfig `json:"card,omitempty"`
}

type startAgentResponse struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// handleAdmin dispatches /admin/agents endpoints:
//
//	POST   /admin/agents                  → start (body: startAgentRequest)
//	POST   /admin/agents/{name}/restart   → restart (re-read agent-card.yaml)
//	DELETE /admin/agents/{name}           → stop the named running agent
//
// Anything else returns 405.
func (g *gatewayHandler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/admin/agents")
	switch {
	case (rest == "" || rest == "/") && r.Method == http.MethodPost:
		g.handleAdminStart(w, r)
	case strings.HasSuffix(rest, "/restart") && r.Method == http.MethodPost:
		name := strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/restart")
		g.handleAdminRestart(w, r, name)
	case strings.HasPrefix(rest, "/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(rest, "/")
		g.handleAdminStop(w, r, name)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed for "+r.URL.Path)
	}
}

// handleAdminRestart restarts an agent so it re-reads its (edited) agent-card.yaml.
func (g *gatewayHandler) handleAdminRestart(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}
	log.Printf("admin: restart agent %q", name)
	port, err := g.sch.RestartAgent(name)
	if err != nil {
		log.Printf("admin: restart agent %q failed: %v", name, err)
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, startAgentResponse{Name: name, Port: port})
}

func (g *gatewayHandler) handleAdminStart(w http.ResponseWriter, r *http.Request) {
	var req startAgentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Resolve the workspace. With an inline card we may allocate a managed
	// workspace and scaffold the agent-card.yaml; without one, the workspace
	// must already exist (pre-staged card) as before.
	workspace := req.Workspace
	if req.Card != nil {
		if workspace == "" {
			workspace = g.sch.cfg.ManagedAgentWorkspace(req.Name)
			if workspace == "" {
				writeJSONError(w, http.StatusBadRequest, "workspace is required (no managed workspace dir for in-memory config)")
				return
			}
		}
		if err := wrapper.WriteCard(workspace, req.Card); err != nil {
			log.Printf("admin: scaffold inline card for %q failed: %v", req.Name, err)
			writeJSONError(w, http.StatusInternalServerError, "scaffold inline card: "+err.Error())
			return
		}
		log.Printf("admin: scaffolded inline card for %q at %s/.a2a/agent-card.yaml", req.Name, workspace)
	} else if workspace == "" {
		writeJSONError(w, http.StatusBadRequest, "workspace is required")
		return
	}

	// Log the inbound request BEFORE attempting the spawn so a stuck
	// startAgent (slow exec, port conflict, missing binary) is still
	// visible in the scheduler tee. The success line is already emitted
	// by startAgent itself ("Agent X started on port Y (pid: Z)").
	log.Printf("admin: start agent %q (workspace=%s, port=%d)", req.Name, workspace, req.Port)

	port, err := g.sch.StartAgent(AgentConfig{
		Name:      req.Name,
		Workspace: workspace,
		Workdir:   req.Workdir,
		Port:      req.Port,
		Instances: req.Instances,
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

// createRoomRequest is the body for POST /rooms.
type createRoomRequest struct {
	Topic        string   `json:"topic"`
	Participants []string `json:"participants"`
	Mode         string   `json:"mode,omitempty"` // "relay" (default) | "roundtable"
	Organizer    string   `json:"organizer,omitempty"`
	MaxChain     int      `json:"maxChain,omitempty"`
	Moderator    string   `json:"moderator,omitempty"` // roundtable: judge/summary agent
	Budget       int      `json:"budget,omitempty"`    // roundtable: max rounds (0 = default)
	Message      string   `json:"message,omitempty"`   // optional opening (operator)
}

// sayRequest is the body for POST /rooms/{id}/say.
type sayRequest struct {
	Text    string `json:"text"`
	Speaker string `json:"speaker,omitempty"`
}

// handleRooms dispatches the roundtable endpoints:
//
//	GET    /rooms              → list
//	POST   /rooms              → create (createRoomRequest)
//	GET    /rooms/{id}         → one room view
//	POST   /rooms/{id}/say     → operator message (sayRequest)
//	POST   /rooms/{id}/stop    → halt
func (g *gatewayHandler) handleRooms(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/rooms"), "/")

	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, g.sch.rooms.List())
		case http.MethodPost:
			var req createRoomRequest
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
				return
			}
			var view *RoomView
			var err error
			if RoomMode(req.Mode) == RoomModeRoundtable {
				view, err = g.sch.rooms.CreateRoundtableRoom(req.Topic, req.Participants, req.Moderator, req.Budget, req.Message)
			} else {
				view, err = g.sch.rooms.CreateRoom(req.Topic, req.Participants, req.Organizer, req.MaxChain, req.Message)
			}
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			writeJSON(w, http.StatusCreated, view)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed for /rooms")
		}
		return
	}

	parts := strings.Split(rest, "/")
	id := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		view, err := g.sch.rooms.Get(id)
		g.writeRoomResult(w, view, err)
	case len(parts) == 2 && parts[1] == "say" && r.Method == http.MethodPost:
		var req sayRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
		if req.Text == "" {
			writeJSONError(w, http.StatusBadRequest, "text is required")
			return
		}
		view, err := g.sch.rooms.Say(id, req.Text, req.Speaker)
		g.writeRoomResult(w, view, err)
	case len(parts) == 2 && parts[1] == "stop" && r.Method == http.MethodPost:
		view, err := g.sch.rooms.Stop(id)
		g.writeRoomResult(w, view, err)
	default:
		writeJSONError(w, http.StatusNotFound, "no such room route: "+r.URL.Path)
	}
}

// writeRoomResult maps room operation errors onto HTTP statuses: "not found" →
// 404, "stopped" → 409, anything else → 400.
func (g *gatewayHandler) writeRoomResult(w http.ResponseWriter, view *RoomView, err error) {
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "not found"):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case strings.Contains(err.Error(), "stopped"):
			writeJSONError(w, http.StatusConflict, err.Error())
		default:
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, view)
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
