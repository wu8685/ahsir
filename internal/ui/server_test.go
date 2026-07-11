package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/scheduler"
)

// stubScheduler records the requests it receives and returns canned bodies so
// the console's proxy + aggregation can be exercised without a real scheduler.
type stubScheduler struct {
	srv          *httptest.Server
	lastPath     string
	lastMethod   string
	lastAdminTok string
	invocations  string
}

func newStubScheduler() *stubScheduler {
	s := &stubScheduler{}
	mux := http.NewServeMux()
	mux.HandleFunc("/invocations", func(w http.ResponseWriter, r *http.Request) {
		s.lastPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, s.invocations)
	})
	mux.HandleFunc("/agents", func(w http.ResponseWriter, r *http.Request) {
		s.lastPath = r.URL.Path
		io.WriteString(w, `[{"name":"teacher","url":"http://127.0.0.1:9801","status":"online"}]`)
	})
	mux.HandleFunc("/archived-agents", func(w http.ResponseWriter, r *http.Request) {
		s.lastPath = r.URL.Path
		s.lastAdminTok = r.Header.Get(scheduler.AdminTokenHeader)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"name":"ghost","contexts":[{"contextId":"ctx-1","title":"hi","turns":2,"lastActivity":"2026-07-01T00:00:00Z","lastStatus":"completed"}]}]`)
	})
	mux.HandleFunc("/admin/agents", func(w http.ResponseWriter, r *http.Request) {
		s.lastPath = r.URL.Path
		s.lastMethod = r.Method
		s.lastAdminTok = r.Header.Get(scheduler.AdminTokenHeader)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"name":"x","port":9802}`)
	})
	// Subtree handler for /admin/agents/{name} — the DELETE the console's
	// "停止下线" button hits to stop a running agent.
	mux.HandleFunc("/admin/agents/", func(w http.ResponseWriter, r *http.Request) {
		s.lastPath = r.URL.Path
		s.lastMethod = r.Method
		s.lastAdminTok = r.Header.Get(scheduler.AdminTokenHeader)
		w.WriteHeader(http.StatusNoContent)
	})
	s.srv = httptest.NewServer(mux)
	return s
}

func newTestServer(t *testing.T, schedURL, token string) http.Handler {
	t.Helper()
	srv, err := New(schedURL, token)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}

func TestContextsAggregation(t *testing.T) {
	stub := newStubScheduler()
	defer stub.srv.Close()
	// Two contexts; ctx-A has two agents and an earlier title turn; one record
	// has no contextId and must be dropped; one roundtable record (room id as
	// contextId) must be excluded — rooms belong to the 圆桌 list, not 会话.
	stub.invocations = `[
	  {"agentName":"teacher","contextId":"ctx-A","userText":"summarize this","status":"completed","startedAt":"2026-06-08T10:00:00Z"},
	  {"agentName":"backend","contextId":"ctx-A","userText":"now build it","status":"completed","startedAt":"2026-06-08T10:05:00Z"},
	  {"agentName":"teacher","contextId":"ctx-B","userText":"hi","status":"failed","startedAt":"2026-06-08T11:00:00Z"},
	  {"agentName":"student","contextId":"room-1","userText":"[圆桌·话题] x","status":"completed","startedAt":"2026-06-08T12:00:00Z","source":"roundtable"},
	  {"agentName":"loner","contextId":"","userText":"isolated","status":"completed","startedAt":"2026-06-08T09:00:00Z"}
	]`

	h := newTestServer(t, stub.srv.URL, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/contexts", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out []contextSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d contexts, want 2 (isolated + roundtable dropped): %+v", len(out), out)
	}
	for _, c := range out {
		if c.ContextID == "room-1" {
			t.Fatalf("roundtable room leaked into /api/contexts: %+v", c)
		}
	}
	// Newest-first ordering: ctx-B (11:00) before ctx-A (10:05).
	if out[0].ContextID != "ctx-B" || out[1].ContextID != "ctx-A" {
		t.Fatalf("ordering wrong: %s, %s", out[0].ContextID, out[1].ContextID)
	}
	a := out[1]
	if a.Title != "summarize this" {
		t.Errorf("title = %q, want earliest user text", a.Title)
	}
	if a.Turns != 2 || len(a.Agents) != 2 {
		t.Errorf("ctx-A turns=%d agents=%v, want 2 turns / 2 agents", a.Turns, a.Agents)
	}
	if a.LastStatus != "completed" {
		t.Errorf("ctx-A lastStatus = %q, want completed (latest turn)", a.LastStatus)
	}
	if out[0].LastStatus != "failed" {
		t.Errorf("ctx-B lastStatus = %q, want failed", out[0].LastStatus)
	}
}

func TestProxyAgents(t *testing.T) {
	stub := newStubScheduler()
	defer stub.srv.Close()
	h := newTestServer(t, stub.srv.URL, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastPath != "/agents" {
		t.Errorf("scheduler saw path %q, want /agents (prefix stripped)", stub.lastPath)
	}
	if !strings.Contains(rec.Body.String(), "teacher") {
		t.Errorf("body not relayed: %s", rec.Body.String())
	}
}

// TestProxyArchivedAgents confirms the console reaches the scheduler's
// read-only offline-agent endpoint through the generic /api proxy (no admin
// token needed), so the "归档" rail can list deleted agents.
func TestProxyArchivedAgents(t *testing.T) {
	stub := newStubScheduler()
	defer stub.srv.Close()
	h := newTestServer(t, stub.srv.URL, "admin-secret")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/archived-agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if stub.lastPath != "/archived-agents" {
		t.Errorf("scheduler saw path %q, want /archived-agents (prefix stripped)", stub.lastPath)
	}
	if stub.lastAdminTok != "" {
		t.Errorf("read route must not carry admin token, got %q", stub.lastAdminTok)
	}
	if !strings.Contains(rec.Body.String(), "ghost") {
		t.Errorf("body not relayed: %s", rec.Body.String())
	}
}

func TestAdminTokenInjected(t *testing.T) {
	stub := newStubScheduler()
	defer stub.srv.Close()
	h := newTestServer(t, stub.srv.URL, "secret-token")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/agents", strings.NewReader(`{"name":"x","workspace":"/tmp/x"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastAdminTok != "secret-token" {
		t.Errorf("admin token = %q, want it injected on /admin/*", stub.lastAdminTok)
	}
}

// The console's "停止下线" button issues DELETE /api/admin/agents/{name}. The
// proxy must forward it as a DELETE to the right subtree path AND inject the
// control-plane token — the path carries a /{name} suffix that still has to
// match the /admin/ prefix rule.
func TestStopAgentProxiesDeleteWithToken(t *testing.T) {
	stub := newStubScheduler()
	defer stub.srv.Close()
	h := newTestServer(t, stub.srv.URL, "secret-token")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/agents/teacher", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if stub.lastMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", stub.lastMethod)
	}
	if stub.lastPath != "/admin/agents/teacher" {
		t.Errorf("path = %q, want /admin/agents/teacher", stub.lastPath)
	}
	if stub.lastAdminTok != "secret-token" {
		t.Errorf("admin token = %q, want it injected on the DELETE", stub.lastAdminTok)
	}
}

func TestAdminTokenNotInjectedOnReadRoutes(t *testing.T) {
	stub := newStubScheduler()
	defer stub.srv.Close()
	h := newTestServer(t, stub.srv.URL, "secret-token")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if got := rec.Header().Get(scheduler.AdminTokenHeader); got != "" {
		t.Errorf("token leaked onto a read route header: %q", got)
	}
}

func TestStaticIndexServed(t *testing.T) {
	stub := newStubScheduler()
	defer stub.srv.Close()
	h := newTestServer(t, stub.srv.URL, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ahsir") {
		t.Errorf("index.html not served: %s", rec.Body.String()[:min(200, rec.Body.Len())])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// statefulScheduler is a faithful (LLM-free) stand-in for the real scheduler's
// happy path: an async chat records an invocation + a transcript turn keyed by
// the contextId it was GIVEN, the task reports completed with the reply, and
// /invocations + /history serve that recorded state back. It exists to lock the
// console's full dispatch loop — chat → poll → contexts → history — as one test,
// and in particular to pin the contract surfaced during live verification: the
// client owns the contextId and the console forwards it unchanged, so the
// ledger, the transcript, and the UI all agree on the same id. (When that id is
// dropped, the real scheduler records an empty-contextId invocation and the
// transcript lands elsewhere — contexts and history both come up empty.)
type statefulScheduler struct {
	srv *httptest.Server

	gotChatContextID string // contextId the console forwarded on the chat POST
	reply            string
	// recorded per (agent,contextId)
	turns       map[string][]map[string]any
	invocations []map[string]any
}

func newStatefulScheduler() *statefulScheduler {
	s := &statefulScheduler{
		reply: "pong",
		turns: map[string][]map[string]any{},
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/invocations", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.URL.Query().Get("contextId")
		out := s.invocations
		if ctx != "" {
			out = nil
			for _, inv := range s.invocations {
				if inv["contextId"] == ctx {
					out = append(out, inv)
				}
			}
		}
		json.NewEncoder(w).Encode(out)
	})

	// /agents/{name}/chat | /agents/{name}/tasks/{id} | /agents/{name}/history/{ctx}
	mux.HandleFunc("/agents/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/agents/"), "/"), "/")
		name := parts[0]
		switch {
		case len(parts) == 2 && parts[1] == "chat" && r.Method == http.MethodPost:
			var body struct {
				Message   string `json:"message"`
				ContextID string `json:"contextId"`
				Speaker   string `json:"speaker"`
				Async     bool   `json:"async"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			s.gotChatContextID = body.ContextID
			key := name + "\x00" + body.ContextID
			s.turns[key] = append(s.turns[key], map[string]any{
				"turn": len(s.turns[key]) + 1, "speaker": body.Speaker,
				"userText": body.Message, "reply": s.reply, "status": "completed",
			})
			s.invocations = append(s.invocations, map[string]any{
				"agentName": name, "contextId": body.ContextID, "userText": body.Message,
				"status": "completed", "startedAt": "2026-06-08T21:00:00Z", "speaker": body.Speaker,
			})
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]any{"taskId": "task-1", "contextId": body.ContextID})
		case len(parts) == 3 && parts[1] == "tasks" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{
				"id":     parts[2],
				"status": map[string]any{"state": "completed"},
				"history": []map[string]any{
					{"role": "agent", "parts": []map[string]any{{"kind": "text", "text": s.reply}}},
				},
			})
		case len(parts) == 3 && parts[1] == "history" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(s.turns[name+"\x00"+parts[2]])
		default:
			http.Error(w, "no route: "+r.URL.Path, http.StatusNotFound)
		}
	})

	s.srv = httptest.NewServer(mux)
	return s
}

// TestChatLoop drives the exact sequence the console's app.js performs and the
// sequence we verified live: dispatch an async turn under a client-owned
// contextId, poll the task to completion, then confirm the conversation surfaces
// in /api/contexts and its transcript in /api/.../history.
func TestChatLoop(t *testing.T) {
	stub := newStatefulScheduler()
	defer stub.srv.Close()
	h := newTestServer(t, stub.srv.URL, "")

	const ctxID = "conv-verify-1"

	// 1. async dispatch with a client-supplied contextId.
	rec := httptest.NewRecorder()
	body := `{"message":"are you there?","async":true,"speaker":"console","contextId":"` + ctxID + `"}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agents/reviewer/chat", strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("chat status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var sub struct{ TaskID, ContextID string }
	if err := json.Unmarshal(rec.Body.Bytes(), &sub); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	// The fix surfaced during live verification: the console MUST forward the
	// client's contextId so ledger + transcript agree on it.
	if stub.gotChatContextID != ctxID {
		t.Fatalf("scheduler received contextId %q, want %q (console must forward it)", stub.gotChatContextID, ctxID)
	}
	if sub.ContextID != ctxID || sub.TaskID == "" {
		t.Fatalf("submit = %+v, want taskId set + contextId %q", sub, ctxID)
	}

	// 2. poll task → completed, reply extractable from agent history.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/reviewer/tasks/"+sub.TaskID, nil))
	var task struct {
		Status  struct{ State string } `json:"status"`
		History []struct {
			Role  string `json:"role"`
			Parts []struct {
				Kind, Text string
			} `json:"parts"`
		} `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.Status.State != "completed" {
		t.Fatalf("task state = %q, want completed", task.Status.State)
	}
	if got := task.History[0].Parts[0].Text; got != "pong" {
		t.Errorf("reply = %q, want pong", got)
	}

	// 3. /api/contexts now aggregates the conversation.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/contexts", nil))
	var ctxs []contextSummary
	json.Unmarshal(rec.Body.Bytes(), &ctxs)
	if len(ctxs) != 1 {
		t.Fatalf("contexts = %d, want 1: %+v", len(ctxs), ctxs)
	}
	if ctxs[0].ContextID != ctxID || ctxs[0].Title != "are you there?" || ctxs[0].Turns != 1 {
		t.Errorf("context summary wrong: %+v", ctxs[0])
	}
	if len(ctxs[0].Agents) != 1 || ctxs[0].Agents[0] != "reviewer" {
		t.Errorf("context agents = %v, want [reviewer]", ctxs[0].Agents)
	}

	// 4. per-agent history in that context replays the turn.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents/reviewer/history/"+ctxID, nil))
	var turns []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &turns)
	if len(turns) != 1 {
		t.Fatalf("history turns = %d, want 1: %s", len(turns), rec.Body.String())
	}
	if turns[0]["userText"] != "are you there?" || turns[0]["reply"] != "pong" {
		t.Errorf("history turn wrong: %+v", turns[0])
	}
}

// --- Left-rail layout invariants (issue #27) ---------------------------------
//
// The left rail is a flex column: #rooms (.roomlist) and #archived
// (.archived-list) are fixed-height siblings around the flexible #contexts
// (.sessions) list. If the two auxiliary lists are capped too high, .sessions
// gets starved to ~0 height and the conversation list disappears. These tests
// pin the CSS invariants that keep #contexts usable, reading the SAME embedded
// assets the server ships so a regression in the stylesheet fails the build.

// cssDecl returns the value of `prop` inside the rule block for `selector`,
// searching the given embedded asset. Returns "" if not found.
func cssDecl(t *testing.T, asset, selector, prop string) string {
	t.Helper()
	b, err := assetsFS.ReadFile("assets/" + asset)
	if err != nil {
		t.Fatalf("read %s: %v", asset, err)
	}
	css := string(b)
	// Find "<selector>{ ... }" (selector at a rule boundary).
	re := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("selector %q not found in %s", selector, asset)
	}
	pre := regexp.MustCompile(`(?:^|;)\s*` + regexp.QuoteMeta(prop) + `\s*:\s*([^;]+)`)
	pm := pre.FindStringSubmatch(m[1])
	if pm == nil {
		return ""
	}
	return strings.TrimSpace(pm[1])
}

// vhValue parses a "26vh" style length and asserts the unit is vh.
func vhValue(t *testing.T, raw string) float64 {
	t.Helper()
	if !strings.HasSuffix(raw, "vh") {
		t.Fatalf("length %q is not in vh — units must be unified to vh (issue #27)", raw)
	}
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSuffix(raw, "vh"), "%f", &v); err != nil {
		t.Fatalf("parse vh from %q: %v", raw, err)
	}
	return v
}

func TestLeftRailAuxListsDoNotStarveContexts(t *testing.T) {
	roomsRaw := cssDecl(t, "index.html", ".roomlist", "max-height")
	archRaw := cssDecl(t, "app.css", ".archived-list", "max-height")

	rooms := vhValue(t, roomsRaw)   // fails if not vh
	arch := vhValue(t, archRaw)     // fails if archived kept its old "38%"

	// Each auxiliary list stays modest on its own.
	if rooms > 30 {
		t.Errorf(".roomlist max-height = %gvh, want <= 30vh so #contexts survives", rooms)
	}
	if arch > 30 {
		t.Errorf(".archived-list max-height = %gvh, want <= 30vh so #contexts survives", arch)
	}
	// Together they must leave the flexible #contexts list real estate: with
	// .nav + .left-foot chrome (~10vh), rooms+archived over ~60vh would squeeze
	// #contexts toward 0. Keep the pair bounded well under that.
	if rooms+arch > 60 {
		t.Errorf(".roomlist(%gvh)+.archived-list(%gvh)=%gvh exceeds 60vh; #contexts risks starvation", rooms, arch, rooms+arch)
	}
}

// The belt-and-suspenders guard from the issue: .sessions must carry a positive
// min-height floor so it can never collapse to a 0-height (invisible) scroll box
// even if the flex math is pushed to an extreme.
func TestContextsListHasMinHeightFloor(t *testing.T) {
	raw := cssDecl(t, "app.css", ".sessions", "min-height")
	if raw == "" || raw == "0" {
		t.Fatalf(".sessions min-height = %q, want a positive floor so it never collapses to 0", raw)
	}
	if !strings.HasSuffix(raw, "px") {
		t.Fatalf(".sessions min-height = %q, want an absolute px floor", raw)
	}
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSuffix(raw, "px"), "%f", &v); err != nil {
		t.Fatalf("parse px from %q: %v", raw, err)
	}
	if v <= 0 {
		t.Errorf(".sessions min-height = %gpx, want > 0", v)
	}
}
