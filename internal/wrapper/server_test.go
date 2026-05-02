package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestA2AServerHandleMessageSend(t *testing.T) {
	taskStore := NewTaskStore()
	server := NewA2AServer(taskStore, nil)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "write a test"})
	params := &a2a.MessageSendParams{Message: msg}

	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "message/send",
		"params":  params,
		"id":      "1",
	}
	body, _ := json.Marshal(reqBody)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestA2AServerHandleTasksGet(t *testing.T) {
	taskStore := NewTaskStore()
	task := a2a.NewSubmittedTask(a2a.TaskInfo{}, a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hello"}))
	taskStore.Save(task)

	server := NewA2AServer(taskStore, nil)

	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tasks/get",
		"params":  map[string]string{"id": string(task.ID)},
		"id":      "2",
	}
	body, _ := json.Marshal(reqBody)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestA2AServerHandleUnknownMethod(t *testing.T) {
	server := NewA2AServer(NewTaskStore(), nil)

	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "unknown/method",
		"id":      "3",
	}
	body, _ := json.Marshal(reqBody)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestA2AServerWithExecutor(t *testing.T) {
	taskStore := NewTaskStore()

	execFn := func(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
		task := a2a.NewSubmittedTask(a2a.TaskInfo{}, msg)
		task.Status = a2a.TaskStatus{State: a2a.TaskStateWorking}
		return task, nil
	}

	server := NewA2AServer(taskStore, execFn)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "do work"})
	params := &a2a.MessageSendParams{Message: msg}

	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "message/send",
		"params":  params,
		"id":      "4",
	}
	body, _ := json.Marshal(reqBody)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
