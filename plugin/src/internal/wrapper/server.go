package wrapper

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

// ProcessMessageFunc is called when a message/send request arrives.
// It receives the context, incoming message, and returns the task created.
type ProcessMessageFunc func(ctx context.Context, msg *a2a.Message) (*a2a.Task, error)

// ProcessMessageStreamFunc is called when a message/stream request arrives.
// It returns an event sequence that the SDK relays back to the caller as SSE.
// Implementations should yield TaskStatusUpdateEvents per delta and a final
// *a2a.Task with a terminal state.
type ProcessMessageStreamFunc func(ctx context.Context, msg *a2a.Message) iter.Seq2[a2a.Event, error]

// A2AServer wraps the SDK's JSON-RPC handler for agent serving.
type A2AServer struct {
	handler        http.Handler
	executor       ProcessMessageFunc
	executorStream ProcessMessageStreamFunc
	tasks          *TaskStore
	// selfName is the receiving agent's name, surfaced in receive logs so the
	// scheduler tee makes inter-agent traffic readable. Optional — empty means
	// no name in log output.
	selfName string

	// asyncMu guards asyncCancels — the per-task cancel hooks for
	// non-blocking sends. tasks/cancel fires the hook so a task still
	// waiting in the per-context turn queue leaves the queue instead of
	// eventually running for nobody.
	asyncMu      sync.Mutex
	asyncCancels map[a2a.TaskID]context.CancelFunc
}

// NewA2AServer creates a new A2A JSON-RPC server.
func NewA2AServer(taskStore *TaskStore, executor ProcessMessageFunc) *A2AServer {
	s := &A2AServer{
		executor:     executor,
		tasks:        taskStore,
		asyncCancels: make(map[a2a.TaskID]context.CancelFunc),
	}
	s.handler = a2asrv.NewJSONRPCHandler(s)
	return s
}

// SetExecutor sets the message processor function.
func (s *A2AServer) SetExecutor(executor ProcessMessageFunc) {
	s.executor = executor
}

// SetExecutorStream sets the streaming counterpart used by message/stream.
// Optional — without it, OnSendMessageStream returns no events and the
// transport hint of "Streaming: true" on the agent card is effectively a
// no-op.
func (s *A2AServer) SetExecutorStream(executor ProcessMessageStreamFunc) {
	s.executorStream = executor
}

// SetSelfName tags this server with the agent name it represents. Used only
// in log messages so operators can see "[teacher] receive: ..." in the
// scheduler terminal.
func (s *A2AServer) SetSelfName(name string) {
	s.selfName = name
}

// ServeHTTP handles incoming JSON-RPC requests.
func (s *A2AServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// OnGetTask handles 'tasks/get'.
func (s *A2AServer) OnGetTask(ctx context.Context, query *a2a.TaskQueryParams) (*a2a.Task, error) {
	task, ok := s.tasks.Get(query.ID)
	if !ok {
		return nil, fmt.Errorf("task %s not found", query.ID)
	}
	return task, nil
}

// OnListTasks handles 'tasks/list'.
func (s *A2AServer) OnListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	tasks := s.tasks.List()
	return &a2a.ListTasksResponse{Tasks: tasks}, nil
}

// OnCancelTask handles 'tasks/cancel'. The stored task is replaced with a
// cancelled copy (never mutated in place — StateOf readers rely on that), and
// any async execution still queued for it is released from the turn queue.
func (s *A2AServer) OnCancelTask(ctx context.Context, id *a2a.TaskIDParams) (*a2a.Task, error) {
	// Fire the registered cancel FIRST, before the store lookup. A streaming
	// (message/stream) turn does not persist its task to the store until it
	// completes, so tasks.Get misses it mid-stream — checking the store first
	// would return "not found" and never reach the cancel, leaving the turn
	// running. asyncCancels is populated for both the async message/send path
	// and (now) the streaming path.
	s.asyncMu.Lock()
	cancel := s.asyncCancels[id.ID]
	s.asyncMu.Unlock()
	if cancel != nil {
		cancel()
	}

	task, ok := s.tasks.Get(id.ID)
	if !ok {
		if cancel != nil {
			// In-flight task we just signalled but which isn't persisted yet:
			// synthesize a canceled result so the caller gets a clean response.
			return &a2a.Task{ID: id.ID, Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
		}
		return nil, fmt.Errorf("task %s not found", id.ID)
	}
	cancelled := *task
	cancelled.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
	s.tasks.Save(&cancelled)
	return &cancelled, nil
}

// OnSendMessage handles 'message/send'.
func (s *A2AServer) OnSendMessage(ctx context.Context, params *a2a.MessageSendParams) (a2a.SendMessageResult, error) {
	started := time.Now()
	// One log line per inbound A2A message so operators can see who is being
	// hit, with which contextID, and a preview of the user/peer text. A2A
	// messages don't carry sender identity, so caller name is only visible
	// from the peer's outbound log (executor "[X → Y] A2A_CALL").
	name := s.selfName
	if name == "" {
		name = "agent"
	}
	preview := truncateForLog(messageText(params.Message), 300)
	// Format mirrors the streaming variant — mode=send|stream is the
	// operator-facing signal distinguishing message/send from
	// message/stream. The `receive: contextID=` prefix is unchanged from
	// pre-streaming versions so log greps over old runs still work.
	log.Printf("[%s] receive: contextID=%s msgID=%s mode=send text=%q", name, params.Message.ContextID, params.Message.ID, preview)

	// Non-blocking send (configuration.blocking=false): save a submitted
	// task, run the executor in the background, return immediately. The
	// caller polls tasks/get. Restart semantics are documented in the spec:
	// the store is memory-only, so a taskId 404 after restart degrades to
	// `ahsir history <agent> <contextId>`.
	if s.executor != nil && params.Config != nil && params.Config.Blocking != nil && !*params.Config.Blocking {
		task := s.sendNonBlocking(params, name)
		log.Printf("[%s] send accepted contextID=%s msgID=%s taskID=%s mode=send blocking=false took=%v", name, params.Message.ContextID, params.Message.ID, task.ID, time.Since(started))
		return task, nil
	}

	if s.executor != nil {
		task, err := s.executor(ctx, params.Message)
		if err != nil {
			// Log here so failures show up in the agent process's stdout
			// (which the scheduler tees into its own terminal). Without
			// this, JSON-RPC errors are only visible to whoever is calling
			// the endpoint — the scheduler operator sees nothing.
			log.Printf("[%s] send done contextID=%s msgID=%s state=failed took=%v err=%v", name, params.Message.ContextID, params.Message.ID, time.Since(started), err)
			log.Printf("OnSendMessage: executor failed: %v", err)
			return nil, asA2AError(err)
		}
		// Persist completed task so subsequent message/send calls sharing the
		// same contextId can replay history.
		if task != nil {
			s.tasks.Save(task)
		}
		state := a2a.TaskStateCompleted
		history := 0
		if task != nil {
			state = task.Status.State
			history = len(task.History)
		}
		log.Printf("[%s] send done contextID=%s msgID=%s state=%s history=%d took=%v", name, params.Message.ContextID, params.Message.ID, state, history, time.Since(started))
		return task, nil
	}
	task := a2a.NewSubmittedTask(a2a.TaskInfo{}, params.Message)
	s.tasks.Save(task)
	log.Printf("[%s] send done contextID=%s msgID=%s state=%s history=%d took=%v executor=false", name, params.Message.ContextID, params.Message.ID, task.Status.State, len(task.History), time.Since(started))
	return task, nil
}

// sendNonBlocking accepts a message for background execution: persists a
// `submitted` task, launches the executor in a goroutine, and returns the
// submitted task for the caller to poll. Setting Message.TaskID before the
// executor runs makes the executor's own NewSubmittedTask reuse OUR task ID,
// so tasks/get, the transcript's taskId, and the returned handle all agree.
func (s *A2AServer) sendNonBlocking(params *a2a.MessageSendParams, name string) *a2a.Task {
	params.Message.TaskID = ""
	submitted := a2a.NewSubmittedTask(params.Message, params.Message)
	params.Message.TaskID = submitted.ID
	// Pin the contextID the same way as TaskID, and for the same reason. When
	// the caller sends no contextID, NewSubmittedTask mints a fresh one onto
	// `submitted` — but the background goroutine below calls the executor with
	// params.Message, whose own NewSubmittedTask would otherwise mint a SECOND,
	// different contextID for the session + transcript. Writing the minted id
	// back makes the executor reuse it, so the returned task handle, the
	// session, and the on-disk transcript all key on the same contextID. Without
	// this, an async caller gets back a contextID that `history`/`trace` can
	// never resolve (the transcript lives under the other id).
	params.Message.ContextID = submitted.ContextID
	s.tasks.Save(submitted)

	ctx, cancel := context.WithCancel(context.Background())
	s.asyncMu.Lock()
	s.asyncCancels[submitted.ID] = cancel
	s.asyncMu.Unlock()

	go func() {
		defer func() {
			s.asyncMu.Lock()
			delete(s.asyncCancels, submitted.ID)
			s.asyncMu.Unlock()
			cancel()
		}()

		// A cancel can land before this goroutine is even scheduled — honour
		// it without touching the provider.
		if ctx.Err() != nil {
			log.Printf("[%s] async send cancelled before start contextID=%s taskID=%s", name, params.Message.ContextID, submitted.ID)
			return
		}

		// Announce working with a fresh copy — the submitted value already
		// escaped to the JSON-RPC response writer and must not be mutated.
		working := *submitted
		working.Status = a2a.TaskStatus{State: a2a.TaskStateWorking}
		s.tasks.Save(&working)

		task, err := s.executor(ctx, params.Message)

		// A cancel that landed while we were queued/running wins: keep the
		// canceled state, discard the result. Checked two ways because of a
		// real race — our Save(working) above can overwrite the canceled
		// state a fast tasks/cancel already stored. ctx.Err() is the
		// authoritative signal: only OnCancelTask holds this cancel func.
		if state, ok := s.tasks.StateOf(submitted.ID); (ok && state == a2a.TaskStateCanceled) || ctx.Err() != nil {
			cancelled := *submitted
			cancelled.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
			s.tasks.Save(&cancelled)
			log.Printf("[%s] async send cancelled contextID=%s taskID=%s", name, params.Message.ContextID, submitted.ID)
			return
		}
		if err != nil {
			log.Printf("[%s] async send failed contextID=%s taskID=%s err=%v", name, params.Message.ContextID, submitted.ID, err)
			failed := *submitted
			failed.Status = a2a.TaskStatus{
				State:   a2a.TaskStateFailed,
				Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: err.Error()}),
			}
			s.tasks.Save(&failed)
			return
		}
		if task != nil {
			// Defensive: executor derives its task from Message.TaskID, so
			// IDs already match — pin them anyway so a future executor
			// change can't silently strand pollers.
			task.ID = submitted.ID
			task.ContextID = submitted.ContextID
			s.tasks.Save(task)
		}
		log.Printf("[%s] async send done contextID=%s taskID=%s", name, params.Message.ContextID, submitted.ID)
	}()
	return submitted
}

// OnResubscribeToTask handles 'tasks/resubscribe'.
func (s *A2AServer) OnResubscribeToTask(ctx context.Context, id *a2a.TaskIDParams) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		_ = yield
	}
}

// OnSendMessageStream handles 'message/stream'. Yields TaskStatusUpdateEvents
// for each text delta and a final *a2a.Task at completion. Persists the
// terminal task so subsequent tasks/get and resubscribe calls work — same as
// OnSendMessage.
func (s *A2AServer) OnSendMessageStream(ctx context.Context, params *a2a.MessageSendParams) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		started := time.Now()
		name := s.selfName
		if name == "" {
			name = "agent"
		}
		preview := truncateForLog(messageText(params.Message), 300)
		// mode=stream is the operator-facing signal that this turn is being
		// served over SSE — pairs with the mode=send line in OnSendMessage.
		// Grep-friendly: `grep "mode=stream"` to find every streaming turn.
		// `receive: contextID=` prefix matches OnSendMessage exactly so old
		// log greps keep working; mode=... is the discriminator.
		log.Printf("[%s] receive: contextID=%s msgID=%s mode=stream text=%q", name, params.Message.ContextID, params.Message.ID, preview)

		if s.executorStream == nil {
			return
		}

		// Make the streaming turn cancelable via tasks/cancel. Wrap ctx and,
		// once the task id is known (from the first event that carries one),
		// register the cancel under that id so OnCancelTask aborts the in-flight
		// provider turn — the same mechanism the async message/send path uses.
		// Without this, message/stream registered no cancel, so tasks/cancel
		// only marked the task canceled while generation ran to completion.
		streamCtx, cancel := context.WithCancel(ctx)
		var cancelTaskID a2a.TaskID
		registerCancel := func(taskID a2a.TaskID) {
			if taskID == "" || cancelTaskID != "" {
				return
			}
			cancelTaskID = taskID
			s.asyncMu.Lock()
			s.asyncCancels[taskID] = cancel
			s.asyncMu.Unlock()
		}
		defer func() {
			if cancelTaskID != "" {
				s.asyncMu.Lock()
				delete(s.asyncCancels, cancelTaskID)
				s.asyncMu.Unlock()
			}
			cancel()
		}()

		// Counters / timer for the per-stream summary log line. Status-update
		// frames with text-bearing parts count as deltas; non-text status
		// updates (the initial "working" announcement) and the terminal Task
		// don't. The summary is logged on stream end regardless of how it
		// ended (normal, error yield, client disconnect).
		var deltaCount, deltaBytes int
		defer func() {
			log.Printf("[%s] stream done contextID=%s msgID=%s deltas=%d bytes=%d took=%v", name, params.Message.ContextID, params.Message.ID, deltaCount, deltaBytes, time.Since(started))
		}()

		for ev, err := range s.executorStream(streamCtx, params.Message) {
			// Persist completed Tasks so message/send-style follow-ups can
			// recover history (mirrors OnSendMessage's behaviour). Register the
			// cancel as soon as a task id is visible on any event.
			if task, ok := ev.(*a2a.Task); ok && task != nil {
				registerCancel(task.ID)
				s.tasks.Save(task)
			}
			// Cheap delta counter so the summary log line carries useful
			// throughput info. Inspect status updates with a text-bearing
			// message; anything else (initial working announcement, terminal
			// Task) doesn't get counted.
			if su, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
				registerCancel(su.TaskID)
				if su.Status.Message != nil {
					for _, part := range su.Status.Message.Parts {
						if tp, ok := part.(a2a.TextPart); ok && tp.Text != "" {
							deltaCount++
							deltaBytes += len(tp.Text)
						}
					}
				}
			}
			if !yield(ev, asA2AError(err)) {
				return
			}
		}
	}
}

// asA2AError converts executor errors that carry caller-actionable semantics
// into *a2a.Error so their message survives the JSON-RPC boundary — the SDK
// collapses plain errors to a generic "internal error". Currently only the
// busy condition needs this: its "agent busy:" text is the wire marker the
// scheduler gateway maps to HTTP 409 (see ErrTurnInFlight). nil and all other
// errors pass through unchanged.
func asA2AError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTurnInFlight) {
		return a2a.NewError(a2a.ErrServerError, ErrTurnInFlight.Error())
	}
	return err
}

// OnGetTaskPushConfig handles push config get.
func (s *A2AServer) OnGetTaskPushConfig(ctx context.Context, params *a2a.GetTaskPushConfigParams) (*a2a.TaskPushConfig, error) {
	return &a2a.TaskPushConfig{}, nil
}

// OnListTaskPushConfig handles push config list.
func (s *A2AServer) OnListTaskPushConfig(ctx context.Context, params *a2a.ListTaskPushConfigParams) ([]*a2a.TaskPushConfig, error) {
	return nil, nil
}

// OnSetTaskPushConfig handles push config set.
func (s *A2AServer) OnSetTaskPushConfig(ctx context.Context, params *a2a.TaskPushConfig) (*a2a.TaskPushConfig, error) {
	return params, nil
}

// OnDeleteTaskPushConfig handles push config delete.
func (s *A2AServer) OnDeleteTaskPushConfig(ctx context.Context, params *a2a.DeleteTaskPushConfigParams) error {
	return nil
}

// OnGetExtendedAgentCard returns the extended agent card.
func (s *A2AServer) OnGetExtendedAgentCard(ctx context.Context) (*a2a.AgentCard, error) {
	return nil, nil
}
