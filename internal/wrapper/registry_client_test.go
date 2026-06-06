package wrapper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestRegistryAgentCallerUsesRegistryReturnedA2AURL(t *testing.T) {
	var a2aHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents/teacher":
			writeTestJSON(t, w, a2a.AgentCard{
				Name:               "teacher",
				Version:            "1.0.0",
				URL:                "http://" + r.Host + "/a2a/teacher",
				PreferredTransport: a2a.TransportProtocolJSONRPC,
			})
		case "/a2a/teacher":
			a2aHits++
			writeTestA2AReply(t, w, "scheduler-visible reply")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	call := RegistryAgentCaller(srv.URL)
	reply, err := call(context.Background(), "teacher", "ctx-1", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "scheduler-visible reply" {
		t.Fatalf("reply = %q", reply)
	}
	if a2aHits != 1 {
		t.Fatalf("expected one scheduler A2A proxy hit, got %d", a2aHits)
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatal(err)
	}
}

func writeTestA2AReply(t *testing.T, w http.ResponseWriter, text string) {
	t.Helper()
	task := a2a.NewSubmittedTask(a2a.TaskInfo{}, nil)
	task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	task.History = []*a2a.Message{
		a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: text}),
	}
	result, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, w, map[string]any{
		"jsonrpc": "2.0",
		"result":  json.RawMessage(result),
		"id":      "test",
	})
}
