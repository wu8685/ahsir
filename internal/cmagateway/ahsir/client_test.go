package ahsir

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// RegisterAgent must forward both the instances cap and the card's
// pool.session_isolation on the /admin/agents wire, matching the scheduler's
// startAgentRequest shape (issue #24). Decoding into a mirror of that shape
// asserts the exact JSON keys the scheduler reads.
func TestRegisterAgent_ForwardsInstancesAndIsolation(t *testing.T) {
	type poolWire struct {
		SessionIsolation string `json:"session_isolation"`
	}
	type cardWire struct {
		Name string   `json:"name"`
		Pool poolWire `json:"pool"`
	}
	type startWire struct {
		Name      string   `json:"name"`
		Instances int      `json:"instances"`
		Card      cardWire `json:"card"`
	}

	var got startWire
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/agents" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode body: %v (body=%s)", err, body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	card := &AgentCard{
		Name: "cma-coder-v1",
		Pool: PoolConfig{SessionIsolation: "worktree"},
	}
	if err := c.RegisterAgent(context.Background(), "cma-coder-v1", card, 2); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	if got.Instances != 2 {
		t.Errorf("instances = %d, want 2", got.Instances)
	}
	if got.Card.Pool.SessionIsolation != "worktree" {
		t.Errorf("card.pool.session_isolation = %q, want worktree", got.Card.Pool.SessionIsolation)
	}
	if got.Name != "cma-coder-v1" {
		t.Errorf("name = %q, want cma-coder-v1", got.Name)
	}
}

// Zero instances / empty isolation must be omitted from the wire so the
// scheduler falls back to its single-instance, shared-workdir defaults.
func TestRegisterAgent_OmitsDefaults(t *testing.T) {
	var raw map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	card := &AgentCard{Name: "cma-coder-v1"}
	if err := c.RegisterAgent(context.Background(), "cma-coder-v1", card, 0); err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if _, ok := raw["instances"]; ok {
		t.Errorf("instances should be omitted when 0, got %s", raw["instances"])
	}
}

// A 409 from the scheduler ("already running") is success for the versioned
// name model — RegisterAgent must not surface it as an error.
func TestRegisterAgent_ConflictIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if err := c.RegisterAgent(context.Background(), "cma-coder-v1", &AgentCard{Name: "cma-coder-v1"}, 3); err != nil {
		t.Fatalf("409 should be treated as success, got %v", err)
	}
}
