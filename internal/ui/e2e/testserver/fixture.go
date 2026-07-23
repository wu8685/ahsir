package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

type fixture struct {
	mu       sync.RWMutex
	scenario string
}

func newFixture() *fixture { return &fixture{scenario: "core"} }

var agents = []map[string]any{{
	"name": "live-codex", "url": "http://127.0.0.1:9802", "status": "online",
	"version": "1.2.3-e2e", "description": "deterministic live agent",
	"skills": []map[string]string{{"name": "coding"}},
}}

var liveHistory = []map[string]any{{
	"turn": 1, "speaker": "operator", "userText": "Existing core question",
	"reply": "Existing core reply", "status": "completed",
	"ts": "2026-07-23T02:00:01Z", "durationMs": 1000,
}}

func coreInvocations() []map[string]any {
	rows := make([]map[string]any, 0, 18)
	for i := 1; i <= 18; i++ {
		title := fmt.Sprintf("Core conversation %02d", i)
		if i == 1 {
			title = "Existing core question"
		}
		rows = append(rows, map[string]any{
			"agentName": "live-codex", "contextId": fmt.Sprintf("ctx-live-%02d", i),
			"userText": title, "status": "completed", "speaker": "operator",
			"source": "console", "startedAt": fmt.Sprintf("2026-07-23T02:%02d:00Z", i),
			"durationMs": 1000,
		})
	}
	return rows
}

func coreRooms() []map[string]any {
	rows := make([]map[string]any, 0, 14)
	for i := 1; i <= 14; i++ {
		rows = append(rows, map[string]any{
			"id": fmt.Sprintf("room-%02d", i), "topic": fmt.Sprintf("Core room %02d", i),
			"mode": "collaboration", "status": "stopped", "participants": []string{"live-codex"},
			"organizer": "live-codex",
		})
	}
	return rows
}

func coreArchived() []map[string]any {
	contexts := make([]map[string]any, 0, 8)
	for i := 1; i <= 8; i++ {
		title := fmt.Sprintf("Archived context %02d", i)
		if i == 1 {
			title = "Archived core context"
		}
		contexts = append(contexts, map[string]any{
			"contextId": fmt.Sprintf("ctx-archived-%02d", i), "title": title, "turns": 1,
			"lastStatus": "completed", "lastActivity": fmt.Sprintf("2026-07-22T02:%02d:00Z", i),
		})
	}
	return []map[string]any{{"name": "archived-kimi", "contexts": contexts}}
}

func (f *fixture) currentScenario() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.scenario
}

func (f *fixture) setScenario(scenario string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scenario = scenario
}

func (f *fixture) controlHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/__test/reset" {
			http.NotFound(w, r)
			return
		}

		scenario := r.URL.Query().Get("scenario")
		switch scenario {
		case "core", "empty", "scheduler-error":
			f.setScenario(scenario)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unknown scenario", http.StatusBadRequest)
		}
	})
}

func (f *fixture) schedulerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.currentScenario() == "scheduler-error" {
			writeFixtureJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "fixture scheduler unavailable"})
			return
		}
		empty := f.currentScenario() == "empty"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/agents":
			if empty {
				writeFixtureJSON(w, http.StatusOK, []any{})
				return
			}
			writeFixtureJSON(w, http.StatusOK, agents)
		case r.Method == http.MethodGet && r.URL.Path == "/archived-agents":
			if empty {
				writeFixtureJSON(w, http.StatusOK, []any{})
				return
			}
			writeFixtureJSON(w, http.StatusOK, coreArchived())
		case r.Method == http.MethodGet && r.URL.Path == "/rooms":
			if empty {
				writeFixtureJSON(w, http.StatusOK, []any{})
				return
			}
			writeFixtureJSON(w, http.StatusOK, coreRooms())
		case r.Method == http.MethodGet && r.URL.Path == "/invocations":
			if empty {
				writeFixtureJSON(w, http.StatusOK, []any{})
				return
			}
			rows := coreInvocations()
			if id := r.URL.Query().Get("contextId"); id != "" {
				filtered := rows[:0]
				for _, row := range rows {
					if row["contextId"] == id {
						filtered = append(filtered, row)
					}
				}
				rows = filtered
			}
			writeFixtureJSON(w, http.StatusOK, rows)
		case r.Method == http.MethodGet && r.URL.Path == "/agents/live-codex/history/ctx-live-01":
			writeFixtureJSON(w, http.StatusOK, liveHistory)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/agents/live-codex/history/"):
			writeFixtureJSON(w, http.StatusOK, []any{})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/agents/archived-kimi/history/"):
			writeFixtureJSON(w, http.StatusOK, []map[string]any{{
				"turn": 1, "speaker": "operator", "userText": "Archived retained question",
				"reply": "Archived retained reply", "status": "completed",
				"ts": "2026-07-22T02:00:01Z", "durationMs": 1000,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/agents/live-codex/config":
			writeFixtureJSON(w, http.StatusOK, map[string]string{"path": "/tmp/e2e/agent-card.yaml", "yaml": "name: live-codex"})
		case r.Method == http.MethodPost && r.URL.Path == "/agents/live-codex/chat":
			writeFixtureJSON(w, http.StatusAccepted, map[string]string{"taskId": "task-live-01", "contextId": "ctx-live-01"})
		case r.Method == http.MethodGet && r.URL.Path == "/agents/live-codex/tasks/task-live-01":
			writeFixtureJSON(w, http.StatusOK, map[string]any{
				"status":  map[string]string{"state": "completed"},
				"history": []map[string]any{{"role": "agent", "parts": []map[string]string{{"kind": "text", "text": "E2E fixed reply"}}}},
			})
		default:
			http.NotFound(w, r)
		}
	})
}

func writeFixtureJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
