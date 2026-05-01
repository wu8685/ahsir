package wrapper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestA2AClientSendMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req a2a.JSONRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "message/send" {
			t.Errorf("unexpected method: %s", req.Method)
		}
		resp := a2a.NewJSONRPCResponse(req.ID, json.RawMessage(`{"status":"received"}`))
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewA2AClient(server.URL, 5*time.Second)
	resp, err := client.SendMessage(context.Background(), "test message", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}
}

func TestA2AClientGetTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req a2a.JSONRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		task := a2a.Task{
			ID:     "task-1",
			Status: a2a.TaskStateCompleted,
		}
		data, _ := json.Marshal(task)
		resp := a2a.NewJSONRPCResponse(req.ID, data)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewA2AClient(server.URL, 5*time.Second)
	task, err := client.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "task-1" {
		t.Errorf("expected task-1, got %s", task.ID)
	}
	if task.Status != a2a.TaskStateCompleted {
		t.Errorf("expected completed, got %s", task.Status)
	}
}

func TestA2AClientRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req a2a.JSONRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := a2a.NewJSONRPCResponse(req.ID, json.RawMessage(`{"ok":true}`))
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewA2AClient(server.URL, 5*time.Second)
	client.SetMaxRetries(3)
	_, err := client.SendMessage(context.Background(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}
