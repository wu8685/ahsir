package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// --- non-blocking send (specs/2026-06-08-shared-context-collaboration.md) ---

// slowExecutor returns an executor that blocks until release is fed one
// token, then returns a completed task echoing the message text.
func slowExecutor(release <-chan struct{}, calls *int32) ProcessMessageFunc {
	return func(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
		atomic.AddInt32(calls, 1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		task := a2a.NewSubmittedTask(msg, msg)
		task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
		task.History = append(task.History, a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "echo: " + messageText(msg)}))
		return task, nil
	}
}

// TestSendNonBlockingPinsGeneratedContextID guards the async coherence fix: an
// async send with NO contextID must hand the executor the SAME contextID that
// was minted onto the returned task handle. Otherwise the executor's own
// NewSubmittedTask mints a second, different id for the session + transcript,
// and the caller's handle resolves to nothing in `history`/`trace`.
func TestSendNonBlockingPinsGeneratedContextID(t *testing.T) {
	store := NewTaskStore()
	seen := make(chan string, 1)
	exec := func(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
		seen <- msg.ContextID
		// Mirror the real executor: it derives its task (and thus the
		// session/transcript contextID) from the message, minting a fresh one
		// only when the message carries none.
		task := a2a.NewSubmittedTask(msg, msg)
		task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
		return task, nil
	}
	srv := NewA2AServer(store, exec)

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "no ctx"})
	// Deliberately leave msg.ContextID empty.
	blocking := false
	params := &a2a.MessageSendParams{Message: msg, Config: &a2a.MessageSendConfig{Blocking: &blocking}}

	r, err := srv.OnSendMessage(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	submitted, ok := r.(*a2a.Task)
	if !ok {
		t.Fatalf("result type = %T", r)
	}
	if submitted.ContextID == "" {
		t.Fatal("submitted task has no contextID")
	}

	var execCtx string
	select {
	case execCtx = <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("executor never ran")
	}
	if execCtx != submitted.ContextID {
		t.Fatalf("executor saw contextID %q but the returned handle is %q — async mismatch bug", execCtx, submitted.ContextID)
	}
}

func pollTaskState(t *testing.T, store *TaskStore, id a2a.TaskID, want a2a.TaskState) *a2a.Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if task, ok := store.Get(id); ok && task.Status.State == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, _ := store.Get(id)
	t.Fatalf("task %s never reached %s (last: %+v)", id, want, task)
	return nil
}

// TestOnSendMessageNonBlockingReturnsSubmitted: blocking=false answers
// immediately with a submitted task; tasks/get converges to the same terminal
// state and text the blocking path would produce.
func TestOnSendMessageNonBlockingReturnsSubmitted(t *testing.T) {
	store := NewTaskStore()
	release := make(chan struct{})
	var calls int32
	srv := NewA2AServer(store, slowExecutor(release, &calls))

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "async hi"})
	msg.ContextID = "ctx-async"
	blocking := false
	params := &a2a.MessageSendParams{
		Message: msg,
		Config:  &a2a.MessageSendConfig{Blocking: &blocking},
	}

	// Run the send in a goroutine with a deadline: an implementation that
	// ignores blocking=false hangs here (the executor is held), and a hung
	// test is a useless RED. The select converts "still blocked" into a
	// clean failure.
	type sendResult struct {
		result a2a.SendMessageResult
		err    error
	}
	done := make(chan sendResult, 1)
	go func() {
		r, err := srv.OnSendMessage(context.Background(), params)
		done <- sendResult{r, err}
	}()
	var got sendResult
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("non-blocking send did not return while the executor was held — blocking=false not honoured")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	task, ok := got.result.(*a2a.Task)
	if !ok {
		t.Fatalf("result type = %T, want *a2a.Task", got.result)
	}
	if task.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("immediate state = %s, want submitted", task.Status.State)
	}
	if task.ContextID != "ctx-async" {
		t.Fatalf("task contextID = %q", task.ContextID)
	}

	close(release)
	final := pollTaskState(t, store, task.ID, a2a.TaskStateCompleted)
	if got := taskToString(final); got != "echo: async hi" {
		t.Fatalf("final text = %q, want the executor's reply", got)
	}
}

// TestOnSendMessageBlockingUnchanged: with Config absent the call holds until
// the executor finishes — today's semantics.
func TestOnSendMessageBlockingUnchanged(t *testing.T) {
	store := NewTaskStore()
	release := make(chan struct{})
	var calls int32
	srv := NewA2AServer(store, slowExecutor(release, &calls))

	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "sync hi"})
	result, err := srv.OnSendMessage(context.Background(), &a2a.MessageSendParams{Message: msg})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := result.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("blocking send must return the terminal task, got %+v", result)
	}
}

// TestCancelledQueuedTaskNeverExecutes: an async task waiting in the
// per-context queue is skipped when cancelled — its provider turn never runs.
func TestCancelledQueuedTaskNeverExecutes(t *testing.T) {
	probe := newGateProbeSession()
	pool := NewSessionPool(func(ctx context.Context, contextID, resumeID string) (Session, error) {
		return probe, nil
	}, time.Minute, time.Minute)
	t.Cleanup(pool.Stop)

	store := NewTaskStore()
	executor := NewExecutor(ExecutorConfig{
		OpenSession: pool.LookupOrCreate,
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    3,
		BasePrompt:  "helper",
	})
	srv := NewA2AServer(store, executor.Execute)

	blocking := false
	send := func(text string) *a2a.Task {
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: text})
		msg.ContextID = "ctx-cancel-queue"
		result, err := srv.OnSendMessage(context.Background(), &a2a.MessageSendParams{
			Message: msg,
			Config:  &a2a.MessageSendConfig{Blocking: &blocking},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.(*a2a.Task)
	}

	// Task 1 occupies the gate (probe holds it until released).
	t1 := send("turn-1")
	// Wait until turn-1 actually reached the provider.
	deadline := time.Now().Add(2 * time.Second)
	for len(probe.executed()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Task 2 queues behind it.
	t2 := send("turn-2")

	// Cancel task 2 while it waits.
	if _, err := srv.OnCancelTask(context.Background(), &a2a.TaskIDParams{ID: t2.ID}); err != nil {
		t.Fatal(err)
	}
	pollTaskState(t, store, t2.ID, a2a.TaskStateCanceled)

	// Release turn 1; it must complete; turn-2 must never reach the provider.
	probe.release <- struct{}{}
	pollTaskState(t, store, t1.ID, a2a.TaskStateCompleted)
	for _, label := range probe.executed() {
		if strings.Contains(label, "turn-2") {
			t.Fatal("cancelled queued task executed anyway")
		}
	}
}
