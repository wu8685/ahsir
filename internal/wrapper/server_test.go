package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
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

// TestA2AServerSavesTaskFromExecutor verifies that tasks returned by the
// executor are persisted to the TaskStore — this is what enables history
// lookups for subsequent message/send calls in the same context.
func TestA2AServerSavesTaskFromExecutor(t *testing.T) {
	taskStore := NewTaskStore()
	execFn := func(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
		task := a2a.NewSubmittedTask(msg, msg)
		task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
		return task, nil
	}
	server := NewA2AServer(taskStore, execFn)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "first turn"})
	msg.ContextID = "ctx-keep"

	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "message/send",
		"params":  &a2a.MessageSendParams{Message: msg},
		"id":      "save-test",
	}
	body, _ := json.Marshal(reqBody)
	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := taskStore.ListByContextID("ctx-keep"); len(got) != 1 {
		t.Fatalf("expected 1 task saved for ctx-keep, got %d", len(got))
	}
}

// TestA2AServerStreamingPersistsFinalTask verifies that OnSendMessageStream
// forwards events from the configured stream executor and persists the final
// *a2a.Task to the TaskStore — same persistence contract as OnSendMessage so
// follow-up tasks/get calls work.
func TestA2AServerStreamingPersistsFinalTask(t *testing.T) {
	taskStore := NewTaskStore()
	server := NewA2AServer(taskStore, nil)

	streamFn := func(ctx context.Context, msg *a2a.Message) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			task := a2a.NewSubmittedTask(msg, msg)
			// One delta-equivalent status update.
			deltaMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "hi"})
			if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, deltaMsg), nil) {
				return
			}
			task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
			yield(task, nil)
		}
	}
	server.SetExecutorStream(streamFn)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "stream me"})
	msg.ContextID = "ctx-stream"

	var got []a2a.Event
	for ev, err := range server.OnSendMessageStream(context.Background(), &a2a.MessageSendParams{Message: msg}) {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events (status + task), got %d", len(got))
	}
	if _, ok := got[0].(*a2a.TaskStatusUpdateEvent); !ok {
		t.Errorf("want TaskStatusUpdateEvent first, got %T", got[0])
	}
	if _, ok := got[1].(*a2a.Task); !ok {
		t.Errorf("want *a2a.Task last, got %T", got[1])
	}
	if saved := taskStore.ListByContextID("ctx-stream"); len(saved) != 1 {
		t.Fatalf("want 1 task persisted for ctx-stream, got %d", len(saved))
	}
}

// TestA2AServerStreamingWithoutExecutorIsNoop guards the unconfigured path:
// no panic, no yields.
func TestA2AServerStreamingWithoutExecutorIsNoop(t *testing.T) {
	server := NewA2AServer(NewTaskStore(), nil)
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "x"})
	count := 0
	for range server.OnSendMessageStream(context.Background(), &a2a.MessageSendParams{Message: msg}) {
		count++
	}
	if count != 0 {
		t.Errorf("want 0 events without executor, got %d", count)
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
