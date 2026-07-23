package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFixtureResetAndCoreRoutes(t *testing.T) {
	f := newFixture()
	req := httptest.NewRequest(http.MethodPost, "/__test/reset?scenario=core", nil)
	w := httptest.NewRecorder()
	f.controlHandler().ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d", w.Code)
	}

	for _, path := range []string{"/agents", "/archived-agents", "/rooms", "/invocations"} {
		w = httptest.NewRecorder()
		f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, w.Code)
		}
	}
}

func TestFixtureChatCompletes(t *testing.T) {
	f := newFixture()
	w := httptest.NewRecorder()
	f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(
		http.MethodPost, "/agents/live-codex/chat", strings.NewReader(`{"message":"E2E ping"}`),
	))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "task-live-01") {
		t.Fatalf("chat = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/agents/live-codex/tasks/task-live-01", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "E2E fixed reply") {
		t.Fatalf("task = %d %s", w.Code, w.Body.String())
	}
}

func TestSchedulerErrorScenario(t *testing.T) {
	f := newFixture()
	f.setScenario("scheduler-error")
	w := httptest.NewRecorder()
	f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/agents", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestFixtureControlRejectsUnsupportedRequests(t *testing.T) {
	f := newFixture()

	for _, tc := range []struct {
		name   string
		method string
		target string
		want   int
	}{
		{name: "non-POST", method: http.MethodGet, target: "/__test/reset?scenario=core", want: http.StatusMethodNotAllowed},
		{name: "non-POST unknown path", method: http.MethodGet, target: "/__test/unknown", want: http.StatusMethodNotAllowed},
		{name: "unknown scenario", method: http.MethodPost, target: "/__test/reset?scenario=other", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			f.controlHandler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.target, nil))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestFixtureEmptyScenarioAndInvocationFilter(t *testing.T) {
	f := newFixture()

	w := httptest.NewRecorder()
	f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/invocations?contextId=ctx-live-01", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("filtered invocations status = %d", w.Code)
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode filtered invocations: %v", err)
	}
	if len(rows) != 1 || rows[0]["userText"] != "Existing core question" {
		t.Fatalf("filtered invocations = %#v", rows)
	}

	f.setScenario("empty")
	for _, path := range []string{"/agents", "/archived-agents", "/rooms", "/invocations"} {
		w = httptest.NewRecorder()
		f.schedulerHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
			t.Errorf("GET %s = %d %q, want 200 []", path, w.Code, w.Body.String())
		}
	}
}

func TestRequestLoggerRecordsDefaultAndExplicitStatus(t *testing.T) {
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	var logs bytes.Buffer
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")

	h := requestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/written" {
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/written?q=1", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/explicit", nil))

	got := logs.String()
	if !strings.Contains(got, "method=GET path=/written?q=1 status=200") {
		t.Fatalf("default status log missing: %q", got)
	}
	if !strings.Contains(got, "method=POST path=/explicit status=204") {
		t.Fatalf("explicit status log missing: %q", got)
	}
}
