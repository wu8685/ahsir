package registry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestRegisterAgent(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{
		Name:        "backend",
		Description: "Backend agent",
		Version:     "1.0.0",
		Endpoint:    "http://127.0.0.1:9801/",
		Skills:      []a2a.AgentSkill{{Name: "api-design"}},
	}
	reg.Register(card)

	agents := reg.List()
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Name != "backend" {
		t.Errorf("expected backend, got %s", agents[0].Name)
	}
	if agents[0].Status != "online" {
		t.Errorf("expected online, got %s", agents[0].Status)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9801/"}
	reg.Register(card)
	card2 := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9802/"}
	reg.Register(card2)

	agents := reg.List()
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Endpoint != "http://127.0.0.1:9802/" {
		t.Errorf("expected updated endpoint, got %s", agents[0].Endpoint)
	}
}

func TestGetAgent(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{
		Name:        "frontend",
		Description: "Frontend agent",
		Version:     "1.0.0",
		Endpoint:    "http://127.0.0.1:9802/",
		Skills:      []a2a.AgentSkill{{Name: "react"}},
	}
	reg.Register(card)

	found, ok := reg.Get("frontend")
	if !ok {
		t.Fatal("expected to find agent")
	}
	if found.Description != "Frontend agent" {
		t.Errorf("unexpected description: %s", found.Description)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestUnregisterAgent(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{Name: "data", Endpoint: "http://127.0.0.1:9803/"}
	reg.Register(card)
	if err := reg.Unregister("data"); err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 0 {
		t.Error("expected empty list")
	}
}

func TestUnregisterNotFound(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	err := reg.Unregister("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestHeartbeat(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9801/"}
	reg.Register(card)

	reg.Heartbeat("backend")

	agent, _ := reg.Get("backend")
	if agent.Status != "online" {
		t.Errorf("expected online after heartbeat, got %s", agent.Status)
	}
}

func TestHeartbeatTimeout(t *testing.T) {
	reg := NewRegistry(50 * time.Millisecond)
	card := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9801/"}
	reg.Register(card)

	time.Sleep(100 * time.Millisecond)

	agent, _ := reg.Get("backend")
	if agent.Status != "offline" {
		t.Errorf("expected offline after timeout, got %s", agent.Status)
	}
}

func TestRegistryHTTPHandler(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	handler := NewHTTPHandler(reg)

	// Register via HTTP
	card := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9801/", Skills: []a2a.AgentSkill{}}
	body := mustMarshal(t, card)
	req := httptest.NewRequest(http.MethodPost, "/agents", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// List via HTTP
	req, _ = http.NewRequest(http.MethodGet, "/agents", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Get via HTTP
	req, _ = http.NewRequest(http.MethodGet, "/agents/backend", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Delete via HTTP
	req, _ = http.NewRequest(http.MethodDelete, "/agents/backend", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify empty
	req, _ = http.NewRequest(http.MethodGet, "/agents", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if len(reg.List()) != 0 {
		t.Error("expected empty after delete")
	}
}

func mustMarshal(t *testing.T, v interface{}) *bytes.Buffer {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewBuffer(data)
}
