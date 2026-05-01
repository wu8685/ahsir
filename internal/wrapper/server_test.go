package wrapper

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestA2AServerHandleMessageSend(t *testing.T) {
	taskStore := NewTaskStore()
	server := NewA2AServer(taskStore, nil)

	msg := a2a.Message{
		Role:  a2a.RoleUser,
		Parts: []a2a.Part{a2a.TextPart{Text: "write a test"}},
	}
	msgData, _ := json.Marshal(msg)
	req := a2a.NewJSONRPCRequest("message/send", msgData)
	body, _ := json.Marshal(req)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp a2a.JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	var task a2a.Task
	json.Unmarshal(resp.Result, &task)
	if task.Status != a2a.TaskStateSubmitted {
		t.Errorf("expected TASK_STATE_SUBMITTED, got %s", task.Status)
	}
}

func TestA2AServerHandleTasksGet(t *testing.T) {
	taskStore := NewTaskStore()
	task := &a2a.Task{ID: "task-1", Status: a2a.TaskStateWorking}
	taskStore.Save(task)

	server := NewA2AServer(taskStore, nil)

	params, _ := json.Marshal(map[string]string{"id": "task-1"})
	req := a2a.NewJSONRPCRequest("tasks/get", params)
	body, _ := json.Marshal(req)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestA2AServerHandleUnknownMethod(t *testing.T) {
	server := NewA2AServer(NewTaskStore(), nil)

	req := a2a.NewJSONRPCRequest("unknown/method", nil)
	body, _ := json.Marshal(req)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp a2a.JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected -32601, got %d", resp.Error.Code)
	}
}

func TestA2AServerHandleInvalidJSON(t *testing.T) {
	server := NewA2AServer(NewTaskStore(), nil)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("not json")))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)

	var resp a2a.JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32700 {
		t.Errorf("expected parse error, got %+v", resp.Error)
	}
}
