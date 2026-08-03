package ahsir

import (
	"context"
	"encoding/json"
	"errors"
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

// The card must serialize RuntimeConfig.AgentIdleTimeout under the exact wire
// key the scheduler reads (runtime.agent_idle_timeout), and omit it when unset
// so ahsir falls back to its 10m default (issue #17).
func TestCard_AgentIdleTimeoutWire(t *testing.T) {
	// Pinned resident -> the key must be present with the given value.
	card := &AgentCard{Name: "cma-coder-v1", Runtime: RuntimeConfig{AgentIdleTimeout: "0"}}
	b, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire struct {
		Runtime struct {
			AgentIdleTimeout string `json:"agent_idle_timeout"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v (json=%s)", err, b)
	}
	if wire.Runtime.AgentIdleTimeout != "0" {
		t.Errorf("runtime.agent_idle_timeout = %q, want 0", wire.Runtime.AgentIdleTimeout)
	}

	// Unset -> the key must be omitted entirely.
	b2, _ := json.Marshal(&AgentCard{Name: "cma-coder-v1"})
	var raw struct {
		Runtime map[string]json.RawMessage `json:"runtime"`
	}
	if err := json.Unmarshal(b2, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw.Runtime["agent_idle_timeout"]; ok {
		t.Errorf("agent_idle_timeout should be omitted when empty, got %s", raw.Runtime["agent_idle_timeout"])
	}
}

// A generic or explicitly incompatible 409 cannot prove that the scheduler's
// running process has the requested immutable version/card/instance cap.
// Treating either as success is the persisted-CMA drift bug: dispatch proceeds
// against an arbitrary runtime merely because its name collides.
func TestRegisterAgent_UnverifiedConflictIsError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "generic legacy conflict", body: `{"error":"agent already running"}`},
		{name: "incompatible configuration", body: `{"error":"agent already running with incompatible configuration"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := New(srv.URL, "")
			err := c.RegisterAgent(context.Background(), "cma-coder-v1", &AgentCard{Name: "cma-coder-v1", Version: "1"}, 3)
			if err == nil {
				t.Fatal("unverified 409 was treated as success")
			}
			var statusErr *AdminHTTPError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusConflict {
				t.Fatalf("error = %T %v, want typed 409 AdminHTTPError", err, err)
			}
		})
	}
}
