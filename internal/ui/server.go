// Package ui implements the standalone ahsir web console: a small HTTP server,
// independent of the scheduler process, that serves a single-page interface and
// proxies the scheduler's existing gateway API.
//
// Design constraints (see docs/ui-design/):
//   - It runs as its OWN process (`ahsir ui`), started optionally. It does not
//     import or embed the scheduler — it talks to a *running* scheduler over the
//     same HTTP gateway the CLI uses.
//   - It holds NO LLM and performs NO orchestration. Multi-agent collaboration
//     happens inside the agents themselves (agent→agent delegation); the console
//     only lets the operator pick which agent to talk to, dispatch a turn, and
//     inspect the result.
//   - The spine of the UI is the A2A contextId, not a UI-invented "session": one
//     contextId chains several agents' sessions, and the scheduler already maps
//     (agent, contextId) → that agent's transcript. So the left rail lists
//     contextIds derived from the invocation ledger, and per-agent history is a
//     straight proxy to /agents/{name}/history/{contextId}.
//
// Routing:
//   - /api/contexts            → aggregated from the scheduler's /invocations
//   - /api/*                   → reverse-proxied to the scheduler (admin routes
//     get the control-plane token injected)
//   - everything else          → embedded static assets (the SPA)
package ui

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wu8685/ahsir/internal/scheduler"
	"github.com/wu8685/ahsir/internal/wrapper"
)

//go:embed assets/*
var assetsFS embed.FS

// Server is the web console. Construct with New and mount via Handler.
type Server struct {
	schedulerURL string
	adminToken   string
	httpc        *http.Client
	proxy        *httputil.ReverseProxy
	static       http.Handler
	uploadDir    string // where composer file-drops are materialised
}

// New builds a console server targeting the scheduler at schedulerURL
// (e.g. "http://127.0.0.1:9800"). adminToken, when non-empty, is attached to
// proxied /admin/* control-plane requests so agent start/stop works; an empty
// token lets those requests proceed tokenless (the scheduler answers 401 with a
// hint when it actually requires one).
func New(schedulerURL, adminToken string) (*Server, error) {
	base, err := url.Parse(strings.TrimSuffix(schedulerURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse scheduler URL %q: %w", schedulerURL, err)
	}

	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, fmt.Errorf("mount assets: %w", err)
	}

	s := &Server{
		schedulerURL: base.String(),
		adminToken:   adminToken,
		httpc:        &http.Client{Timeout: 30 * time.Second},
		static:       http.FileServer(http.FS(sub)),
		uploadDir:    wrapper.ResolveUploadDir(),
	}

	s.proxy = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			// Strip the /api prefix the SPA uses and forward to the scheduler.
			r.URL.Scheme = base.Scheme
			r.URL.Host = base.Host
			r.Host = base.Host
			r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
			// Control-plane routes need the admin token; read/chat routes don't.
			if s.adminToken != "" && strings.HasPrefix(r.URL.Path, "/admin/") {
				r.Header.Set(scheduler.AdminTokenHeader, s.adminToken)
			}
		},
	}
	return s, nil
}

// SetUploadDir overrides where composer file-drops are materialised (default
// $TMPDIR/ahsir-uploads). Point it at a directory the locally-run agents are
// allowed to read — the same dir the operator adds to each agent's
// allowed_paths — so a sandboxed agent can reach the dropped file. An empty
// dir is ignored (keeps the default).
func (s *Server) SetUploadDir(dir string) {
	if dir != "" {
		s.uploadDir = dir
	}
}

// UploadDir reports where dropped files land, so callers (the `ahsir ui`
// command) can log it and remind the operator to allow-list it for agents.
func (s *Server) UploadDir() string { return s.uploadDir }

// Handler returns the root http.Handler for the console.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Aggregation endpoint — computed, not proxied.
	mux.HandleFunc("/api/contexts", s.handleContexts)
	// File-drop sink — local copy so a sandboxed agent can read the path.
	mux.HandleFunc("/api/upload", s.handleUpload)
	// Everything else under /api proxies straight to the scheduler gateway.
	mux.Handle("/api/", s.proxy)
	// The SPA and its assets.
	mux.Handle("/", s.static)
	return mux
}

// maxUploadBytes caps a single console upload request. Files dropped into the
// composer are copied to the upload dir so a locally-run agent can read them;
// the cap keeps a stray drag from filling the disk.
const maxUploadBytes = 64 << 20 // 64 MiB

// handleUpload receives files dragged onto the composer and writes each to the
// console's upload dir, returning the absolute on-disk path to inject into the
// prompt. Browsers hide the original local path of a dragged file, so the SPA
// uploads the bytes instead; we materialise them somewhere the operator's
// locally-run agent is allowed to read. Local-only by design: the returned path
// is valid because the console, the upload dir, and the agent share one host.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, "parse upload: "+err.Error())
		return
	}
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "no file in upload")
		return
	}
	type saved struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	out := make([]saved, 0, len(files))
	for _, fh := range files {
		// Keep only the base name (defeats ../ traversal and strips any client
		// directory), then drop it in a per-file random subdir so the original
		// filename survives in the injected path without colliding.
		name := filepath.Base(filepath.FromSlash(fh.Filename))
		if name == "" || name == "." || name == string(filepath.Separator) {
			name = "file"
		}
		token, err := randToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "token: "+err.Error())
			return
		}
		dir := filepath.Join(s.uploadDir, token)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			writeError(w, http.StatusInternalServerError, "mkdir: "+err.Error())
			return
		}
		dst := filepath.Join(dir, name)
		if err := saveUpload(fh, dst); err != nil {
			writeError(w, http.StatusInternalServerError, "save: "+err.Error())
			return
		}
		out = append(out, saved{Name: name, Path: dst})
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out})
}

func saveUpload(fh *multipart.FileHeader, dst string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, src)
	return err
}

func randToken() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// contextSummary is one row in the left rail: a contextId with the agents that
// participated, the most recent activity, and a human-readable title taken from
// the first user message recorded against it.
type contextSummary struct {
	ContextID    string   `json:"contextId"`
	Title        string   `json:"title"`
	Agents       []string `json:"agents"`
	Turns        int      `json:"turns"`
	LastActivity string   `json:"lastActivity"`
	LastStatus   string   `json:"lastStatus"`
	InvocationID string   `json:"invocationId,omitempty"`
	UserText     string   `json:"userText,omitempty"`
	Speaker      string   `json:"speaker,omitempty"`
	StartedAt    string   `json:"startedAt,omitempty"`
	DurationMS   int64    `json:"durationMs,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// invocation mirrors the fields the scheduler's /invocations endpoint emits
// (see scheduler.invocationView). Only the fields the console needs are kept.
type invocation struct {
	ID         string `json:"id"`
	AgentName  string `json:"agentName"`
	ContextID  string `json:"contextId"`
	UserText   string `json:"userText"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
	Source     string `json:"source"`
	Speaker    string `json:"speaker"`
	DurationMS int64  `json:"durationMs"`
	Error      string `json:"error"`
}

// handleContexts fetches the scheduler's invocation ledger and folds it into
// one row per contextId, newest first. Invocations with no contextId (isolated
// one-shot turns) are skipped — they aren't resumable conversations.
func (s *Server) handleContexts(w http.ResponseWriter, r *http.Request) {
	resp, err := s.httpc.Get(s.schedulerURL + "/invocations")
	if err != nil {
		writeError(w, http.StatusBadGateway, "reach scheduler: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeError(w, http.StatusBadGateway, "scheduler /invocations: "+resp.Status)
		return
	}

	var records []invocation
	if err := json.NewDecoder(resp.Body).Decode(&records); err != nil {
		writeError(w, http.StatusBadGateway, "decode /invocations: "+err.Error())
		return
	}

	type acc struct {
		summary   contextSummary
		agentSeen map[string]bool
		firstAt   string // earliest StartedAt seen — anchors the title
	}
	byCtx := map[string]*acc{}
	order := []string{}
	for _, rec := range records {
		if rec.ContextID == "" {
			continue
		}
		// Roundtable turns share the ledger (so `trace`/轨迹 see them) but their
		// contextId is a room id, not a 1:1 conversation — they belong to the
		// 圆桌 list (/rooms), not the 会话 list. Skip them here so a room doesn't
		// also surface as a regular conversation with a relay-delta title.
		if rec.Source == "roundtable" {
			continue
		}
		a, ok := byCtx[rec.ContextID]
		if !ok {
			a = &acc{
				summary:   contextSummary{ContextID: rec.ContextID},
				agentSeen: map[string]bool{},
			}
			byCtx[rec.ContextID] = a
			order = append(order, rec.ContextID)
		}
		a.summary.Turns++
		if rec.AgentName != "" && !a.agentSeen[rec.AgentName] {
			a.agentSeen[rec.AgentName] = true
			a.summary.Agents = append(a.summary.Agents, rec.AgentName)
		}
		// Title: the user text of the earliest invocation in this context.
		if rec.UserText != "" && (a.firstAt == "" || rec.StartedAt < a.firstAt) {
			a.firstAt = rec.StartedAt
			a.summary.Title = rec.UserText
		}
		// Last activity / status: the latest invocation in this context.
		if rec.StartedAt > a.summary.LastActivity {
			a.summary.LastActivity = rec.StartedAt
			a.summary.LastStatus = rec.Status
			a.summary.InvocationID = rec.ID
			a.summary.UserText = rec.UserText
			a.summary.Speaker = rec.Speaker
			a.summary.StartedAt = rec.StartedAt
			a.summary.DurationMS = rec.DurationMS
			a.summary.Error = rec.Error
		}
	}

	out := make([]contextSummary, 0, len(order))
	for _, id := range order {
		out = append(out, byCtx[id].summary)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivity > out[j].LastActivity
	})

	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
