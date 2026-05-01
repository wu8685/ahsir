package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
)

type mockAgentRouter struct {
	agents []*a2a.AgentCard
}

func (m *mockAgentRouter) ListAgents() []*a2a.AgentCard {
	return m.agents
}

func (m *mockAgentRouter) ChatWithAgent(agentName, message string) (string, error) {
	return "response from " + agentName + ": " + message, nil
}

func (m *mockAgentRouter) GetTaskStatus(agentName, taskID string) (*a2a.Task, error) {
	return &a2a.Task{
		ID:     a2a.TaskID(taskID),
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}, nil
}

func TestMCPServerInitialize(t *testing.T) {
	srv := NewServer(&mockAgentRouter{})

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
		},
		"id": "1",
	}
	data, _ := json.Marshal(req)

	resp, err := srv.HandleMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	json.Unmarshal(resp, &result)
	if result["result"] == nil {
		t.Error("expected result in initialize response")
	}
}

func TestMCPServerListTools(t *testing.T) {
	srv := NewServer(&mockAgentRouter{})

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/list",
		"id":      "2",
	}
	data, _ := json.Marshal(req)

	resp, err := srv.HandleMessage(data)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	json.Unmarshal(resp, &result)

	tools, ok := result["result"].(map[string]interface{})["tools"].([]interface{})
	if !ok {
		t.Fatal("expected tools list")
	}
	if len(tools) < 2 {
		t.Errorf("expected at least 2 tools, got %d", len(tools))
	}
}

func TestMCPServerAgentList(t *testing.T) {
	srv := NewServer(&mockAgentRouter{
		agents: []*a2a.AgentCard{
			{Name: "backend", URL: "http://127.0.0.1:9801/"},
			{Name: "frontend", URL: "http://127.0.0.1:9802/"},
		},
	})

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "agent_list",
		},
		"id": "3",
	}
	data, _ := json.Marshal(req)

	resp, err := srv.HandleMessage(data)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	json.Unmarshal(resp, &result)
	if result["result"] == nil {
		t.Error("expected result from agent_list")
	}
}

func TestMCPServerAgentChat(t *testing.T) {
	srv := NewServer(&mockAgentRouter{})

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "agent_chat",
			"arguments": map[string]interface{}{"agent_name": "backend", "message": "hello"},
		},
		"id": "4",
	}
	data, _ := json.Marshal(req)

	resp, err := srv.HandleMessage(data)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	json.Unmarshal(resp, &result)
	content := result["result"].(map[string]interface{})["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "backend") {
		t.Errorf("expected response to mention backend, got: %s", text)
	}
}
