package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

// freePort finds an available TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// mockA2AServer starts an httptest server that speaks basic A2A JSON-RPC.
func mockA2AServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			ID      string          `json:"id"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "message/send":
			task := a2a.NewSubmittedTask(a2a.TaskInfo{}, nil)
			task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
			task.History = []*a2a.Message{
				a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "response from agent"}),
			}
			result, _ := json.Marshal(task)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"result":  json.RawMessage(result),
				"id":      req.ID,
			})
		case "tasks/get":
			var params struct {
				ID string `json:"id"`
			}
			json.Unmarshal(req.Params, &params)
			task := &a2a.Task{
				ID:     a2a.TaskID(params.ID),
				Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			}
			result, _ := json.Marshal(task)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"result":  json.RawMessage(result),
				"id":      req.ID,
			})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"error":   map[string]interface{}{"code": -32601, "message": "Method not found"},
				"id":      req.ID,
			})
		}
	}))
	return server, server.URL
}

func TestNewScheduler(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	if sch.Registry() == nil {
		t.Error("expected non-nil registry")
	}
}

func TestSchedulerListAgents(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	agents := sch.ListAgents()
	if agents == nil {
		t.Error("expected non-nil agent list")
	}
}

func TestSchedulerChatWithAgent(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)

	_, err := sch.ChatWithAgent("nonexistent", "", "hello")
	if err == nil {
		t.Error("expected error for non-existent agent")
	}

	// Start a mock A2A server and register it
	mockSrv, mockURL := mockA2AServer(t)
	defer mockSrv.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:              "test-agent",
		Version:           "1.0.0",
		URL:               mockURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})
	resp, err := sch.ChatWithAgent("test-agent", "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if resp == "" {
		t.Error("expected non-empty response")
	}
}

func TestSchedulerStartStop(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify registry is actually listening
	time.Sleep(50 * time.Millisecond)
	conn, err := net.DialTimeout("tcp", sch.httpSrv.Addr, 500*time.Millisecond)
	if err != nil {
		t.Fatal("registry not listening:", err)
	}
	conn.Close()

	sch.Stop()
}

func TestSchedulerGetTaskStatus(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)

	// Start a mock A2A server and register it
	mockSrv, mockURL := mockA2AServer(t)
	defer mockSrv.Close()

	sch.Registry().Register(&a2a.AgentCard{
		Name:              "test-agent",
		URL:               mockURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	task, err := sch.GetTaskStatus("test-agent", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(task.ID) != "task-1" {
		t.Errorf("expected task-1, got %s", task.ID)
	}
}

// TestIntegrationFullFlow tests the full lifecycle: start scheduler with registry,
// register a mock agent via HTTP, send messages via A2A, and query task status.
func TestIntegrationFullFlow(t *testing.T) {
	cfg := &Config{
		Registry: RegistryConfig{
			Host: "127.0.0.1",
			Port: freePort(t),
		},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sch.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sch.Stop()

	// Give the registry server time to start
	time.Sleep(50 * time.Millisecond)

	// Start a mock A2A agent server
	mockSrv, mockURL := mockA2AServer(t)
	defer mockSrv.Close()

	registryURL := fmt.Sprintf("http://%s:%d", cfg.Registry.Host, cfg.Registry.Port)

	// Step 1: Register agent via HTTP
	card := a2a.AgentCard{
		Name:              "integration-agent",
		Description:       "Integration test agent",
		Version:           "1.0.0",
		URL:               mockURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
		Skills:            []a2a.AgentSkill{{Name: "testing"}},
	}
	cardData, _ := json.Marshal(card)
	resp, err := http.Post(registryURL+"/agents", "application/json", bytes.NewReader(cardData))
	if err != nil {
		t.Fatalf("register agent via HTTP: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	// Step 2: List agents via HTTP
	resp, err = http.Get(registryURL + "/agents")
	if err != nil {
		t.Fatalf("list agents via HTTP: %v", err)
	}
	var agents []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&agents); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0]["name"] != "integration-agent" {
		t.Errorf("expected integration-agent, got %v", agents[0]["name"])
	}

	// Step 3: Get agent via HTTP
	resp, err = http.Get(registryURL + "/agents/integration-agent")
	if err != nil {
		t.Fatalf("get agent via HTTP: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(body))
	}

	// Step 4: Chat with agent via scheduler
	msg, err := sch.ChatWithAgent("integration-agent", "", "hello integration")
	if err != nil {
		t.Fatalf("chat with agent: %v", err)
	}
	if msg == "" {
		t.Error("expected non-empty response from chat")
	}
	t.Logf("Chat response: %s", msg)

	// Step 5: Get task status via scheduler
	task, err := sch.GetTaskStatus("integration-agent", "task-integration-1")
	if err != nil {
		t.Fatalf("get task status: %v", err)
	}
	if string(task.ID) != "task-integration-1" {
		t.Errorf("expected task-integration-1, got %s", task.ID)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("expected completed, got %s", task.Status.State)
	}

	// Step 6: Verify agent is listed via the AgentRouter interface
	listed := sch.ListAgents()
	if len(listed) != 1 {
		t.Fatalf("expected 1 agent via ListAgents, got %d", len(listed))
	}
	if listed[0].Name != "integration-agent" {
		t.Errorf("expected integration-agent, got %s", listed[0].Name)
	}

	t.Log("Integration test completed successfully")
}
