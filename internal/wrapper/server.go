package wrapper

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/wu8685/ahsir/internal/a2a"
)

// ProcessMessageFunc is called when a message/send request arrives.
type ProcessMessageFunc func(msg *a2a.Message) (*a2a.Task, error)

// A2AServer handles incoming A2A JSON-RPC requests over HTTP.
type A2AServer struct {
	taskStore *TaskStore
	processor ProcessMessageFunc
}

// NewA2AServer creates a new A2A JSON-RPC server.
func NewA2AServer(taskStore *TaskStore, processor ProcessMessageFunc) *A2AServer {
	return &A2AServer{
		taskStore: taskStore,
		processor: processor,
	}
}

// ServeHTTP handles JSON-RPC requests.
func (s *A2AServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req a2a.JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp := a2a.NewJSONRPCError("", -32700, "Parse error", nil)
		json.NewEncoder(w).Encode(resp)
		return
	}

	switch req.Method {
	case "message/send":
		s.handleMessageSend(w, &req)
	case "tasks/get":
		s.handleTasksGet(w, &req)
	default:
		resp := a2a.NewJSONRPCError(req.ID, -32601, "Method not found", nil)
		json.NewEncoder(w).Encode(resp)
	}
}

func (s *A2AServer) handleMessageSend(w http.ResponseWriter, req *a2a.JSONRPCRequest) {
	var msg a2a.Message
	if err := json.Unmarshal(req.Params, &msg); err != nil {
		resp := a2a.NewJSONRPCError(req.ID, -32602, "Invalid params", nil)
		json.NewEncoder(w).Encode(resp)
		return
	}

	task := &a2a.Task{
		ID:      uuid.New().String(),
		Status:  a2a.TaskStateSubmitted,
		Message: msg,
	}

	if s.processor != nil {
		result, err := s.processor(&msg)
		if err != nil {
			resp := a2a.NewJSONRPCError(req.ID, -32603, "Internal error", nil)
			json.NewEncoder(w).Encode(resp)
			return
		}
		task = result
	}

	s.taskStore.Save(task)

	result, _ := json.Marshal(task)
	resp := a2a.NewJSONRPCResponse(req.ID, result)
	json.NewEncoder(w).Encode(resp)
}

func (s *A2AServer) handleTasksGet(w http.ResponseWriter, req *a2a.JSONRPCRequest) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		resp := a2a.NewJSONRPCError(req.ID, -32602, "Invalid params", nil)
		json.NewEncoder(w).Encode(resp)
		return
	}

	task, ok := s.taskStore.Get(params.ID)
	if !ok {
		resp := a2a.NewJSONRPCError(req.ID, -32602, "Task not found", nil)
		json.NewEncoder(w).Encode(resp)
		return
	}

	result, _ := json.Marshal(task)
	resp := a2a.NewJSONRPCResponse(req.ID, result)
	json.NewEncoder(w).Encode(resp)
}
