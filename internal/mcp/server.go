package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/a2aproject/a2a-go/a2a"
)

// AgentRouter is the interface the MCP server uses to communicate with agents.
//
// ChatWithAgent's contextID parameter is propagated as the A2A message's
// contextId so the agent's SessionPool can reuse a session across multiple
// calls in the same conversation. Empty contextID means each call is
// isolated (executor auto-generates a fresh contextID per task).
type AgentRouter interface {
	ListAgents() []*a2a.AgentCard
	ChatWithAgent(agentName, contextID, message string) (string, error)
	GetTaskStatus(agentName, taskID string) (*a2a.Task, error)
}

// Server implements an MCP server over stdio transport.
type Server struct {
	router AgentRouter
}

// NewServer creates a new MCP server.
func NewServer(router AgentRouter) *Server {
	return &Server{router: router}
}

// HandleMessage processes an incoming JSON-RPC message from the MCP client.
func (s *Server) HandleMessage(data []byte) ([]byte, error) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		ID      json.RawMessage `json:"id"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return s.errorResponse(nil, -32700, "Parse error")
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID)
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		return s.handleToolsCall(req.ID, req.Params)
	default:
		return s.errorResponse(req.ID, -32601, "Method not found")
	}
}

func (s *Server) handleInitialize(id json.RawMessage) ([]byte, error) {
	result := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "ahsir-mcp",
			"version": "1.0.0",
		},
	}
	return s.resultResponse(id, result)
}

func (s *Server) handleToolsList(id json.RawMessage) ([]byte, error) {
	tools := []map[string]interface{}{
		{
			"name":        "agent_list",
			"description": "List all registered agents with name, description, skills, and status",
			"inputSchema": map[string]interface{}{
				"type": "object",
			},
		},
		{
			"name":        "agent_chat",
			"description": "Send a message to a specific agent and return the response. Pass context_id to reuse a prior conversation (the agent's SessionPool keys on it).",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_name": map[string]string{"type": "string"},
					"message":    map[string]string{"type": "string"},
					"context_id": map[string]string{"type": "string"},
				},
				"required": []string{"agent_name", "message"},
			},
		},
		{
			"name":        "agent_task_status",
			"description": "Query a task's status on a specific agent",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_name": map[string]string{"type": "string"},
					"task_id":    map[string]string{"type": "string"},
				},
				"required": []string{"agent_name", "task_id"},
			},
		},
	}
	return s.resultResponse(id, map[string]interface{}{"tools": tools})
}

func (s *Server) handleToolsCall(id json.RawMessage, params json.RawMessage) ([]byte, error) {
	var call struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return s.errorResponse(id, -32602, "Invalid params")
	}

	switch call.Name {
	case "agent_list":
		return s.handleAgentList(id)
	case "agent_chat":
		agentName, _ := call.Arguments["agent_name"].(string)
		message, _ := call.Arguments["message"].(string)
		contextID, _ := call.Arguments["context_id"].(string)
		return s.handleAgentChat(id, agentName, contextID, message)
	case "agent_task_status":
		agentName, _ := call.Arguments["agent_name"].(string)
		taskID, _ := call.Arguments["task_id"].(string)
		return s.handleAgentTaskStatus(id, agentName, taskID)
	default:
		return s.errorResponse(id, -32601, fmt.Sprintf("Unknown tool: %s", call.Name))
	}
}

func (s *Server) handleAgentList(id json.RawMessage) ([]byte, error) {
	agents := s.router.ListAgents()
	type agentSummary struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		URL         string   `json:"url"`
		Skills      []string `json:"skills"`
		Version     string   `json:"version"`
	}
	summaries := make([]agentSummary, len(agents))
	for i, a := range agents {
		skills := make([]string, len(a.Skills))
		for j, sk := range a.Skills {
			skills[j] = sk.Name
		}
		summaries[i] = agentSummary{
			Name:        a.Name,
			Description: a.Description,
			URL:         a.URL,
			Skills:      skills,
			Version:     a.Version,
		}
	}
	data, _ := json.Marshal(summaries)
	text := string(data)
	if text == "null" {
		text = "[]"
	}
	content := []map[string]interface{}{
		{"type": "text", "text": text},
	}
	return s.resultResponse(id, map[string]interface{}{"content": content})
}

func (s *Server) handleAgentChat(id json.RawMessage, agentName, contextID, message string) ([]byte, error) {
	response, err := s.router.ChatWithAgent(agentName, contextID, message)
	if err != nil {
		return s.errorResponse(id, -32603, err.Error())
	}
	content := []map[string]interface{}{
		{"type": "text", "text": response},
	}
	return s.resultResponse(id, map[string]interface{}{"content": content})
}

func (s *Server) handleAgentTaskStatus(id json.RawMessage, agentName, taskID string) ([]byte, error) {
	task, err := s.router.GetTaskStatus(agentName, taskID)
	if err != nil {
		return s.errorResponse(id, -32603, err.Error())
	}
	data, _ := json.Marshal(task)
	content := []map[string]interface{}{
		{"type": "text", "text": string(data)},
	}
	return s.resultResponse(id, map[string]interface{}{"content": content})
}

func (s *Server) resultResponse(id json.RawMessage, result interface{}) ([]byte, error) {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"result":  result,
		"id":      id,
	}
	return json.Marshal(resp)
}

func (s *Server) errorResponse(id json.RawMessage, code int, message string) ([]byte, error) {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
		"id": id,
	}
	return json.Marshal(resp)
}
