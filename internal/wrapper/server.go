package wrapper

import (
	"context"
	"fmt"
	"iter"
	"log"
	"net/http"
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
}

// NewA2AServer creates a new A2A JSON-RPC server.
func NewA2AServer(taskStore *TaskStore, executor ProcessMessageFunc) *A2AServer {
	s := &A2AServer{
		executor: executor,
		tasks:    taskStore,
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

// OnCancelTask handles 'tasks/cancel'.
func (s *A2AServer) OnCancelTask(ctx context.Context, id *a2a.TaskIDParams) (*a2a.Task, error) {
	task, ok := s.tasks.Get(id.ID)
	if !ok {
		return nil, fmt.Errorf("task %s not found", id.ID)
	}
	task.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
	s.tasks.Save(task)
	return task, nil
}

// OnSendMessage handles 'message/send'.
func (s *A2AServer) OnSendMessage(ctx context.Context, params *a2a.MessageSendParams) (a2a.SendMessageResult, error) {
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

	if s.executor != nil {
		task, err := s.executor(ctx, params.Message)
		if err != nil {
			// Log here so failures show up in the agent process's stdout
			// (which the scheduler tees into its own terminal). Without
			// this, JSON-RPC errors are only visible to whoever is calling
			// the endpoint — the scheduler operator sees nothing.
			log.Printf("OnSendMessage: executor failed: %v", err)
			return nil, err
		}
		// Persist completed task so subsequent message/send calls sharing the
		// same contextId can replay history.
		if task != nil {
			s.tasks.Save(task)
		}
		return task, nil
	}
	task := a2a.NewSubmittedTask(a2a.TaskInfo{}, params.Message)
	s.tasks.Save(task)
	return task, nil
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

		// Counters / timer for the per-stream summary log line. Status-update
		// frames with text-bearing parts count as deltas; non-text status
		// updates (the initial "working" announcement) and the terminal Task
		// don't. The summary is logged on stream end regardless of how it
		// ended (normal, error yield, client disconnect).
		started := time.Now()
		var deltaCount, deltaBytes int
		defer func() {
			log.Printf("[%s] stream done contextID=%s msgID=%s deltas=%d bytes=%d took=%v", name, params.Message.ContextID, params.Message.ID, deltaCount, deltaBytes, time.Since(started))
		}()

		for ev, err := range s.executorStream(ctx, params.Message) {
			// Persist completed Tasks so message/send-style follow-ups can
			// recover history (mirrors OnSendMessage's behaviour). Non-Task
			// events (status updates) are transient and skipped.
			if task, ok := ev.(*a2a.Task); ok && task != nil {
				s.tasks.Save(task)
			}
			// Cheap delta counter so the summary log line carries useful
			// throughput info. Inspect status updates with a text-bearing
			// message; anything else (initial working announcement, terminal
			// Task) doesn't get counted.
			if su, ok := ev.(*a2a.TaskStatusUpdateEvent); ok && su.Status.Message != nil {
				for _, part := range su.Status.Message.Parts {
					if tp, ok := part.(a2a.TextPart); ok && tp.Text != "" {
						deltaCount++
						deltaBytes += len(tp.Text)
					}
				}
			}
			if !yield(ev, err) {
				return
			}
		}
	}
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
