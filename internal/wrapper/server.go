package wrapper

import (
	"context"
	"fmt"
	"iter"
	"log"
	"net/http"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
)

// ProcessMessageFunc is called when a message/send request arrives.
// It receives the context, incoming message, and returns the task created.
type ProcessMessageFunc func(ctx context.Context, msg *a2a.Message) (*a2a.Task, error)

// A2AServer wraps the SDK's JSON-RPC handler for agent serving.
type A2AServer struct {
	handler  http.Handler
	executor ProcessMessageFunc
	tasks    *TaskStore
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

// OnSendMessageStream handles 'message/stream'.
func (s *A2AServer) OnSendMessageStream(ctx context.Context, params *a2a.MessageSendParams) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		_ = yield
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
