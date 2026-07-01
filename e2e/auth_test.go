//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// TestAuthBaseline_E2E proves the control-plane auth chain end to end against a
// real `ahsir start` subprocess (specs/2026-06-08-auth-baseline.md):
//
//   - the scheduler enforces AHSIR_ADMIN_TOKEN on the control plane;
//   - agents the scheduler spawned still register (they got the token via
//     --admin-token) — proven by them coming ONLINE through the gated registry;
//   - /admin/agents and registry write reject requests without the token (401);
//   - the data/read plane stays open (no token needed).
func TestAuthBaseline_E2E(t *testing.T) {
	const token = "e2e-admin-secret"
	t.Setenv("AHSIR_ADMIN_TOKEN", token)

	// setupE2E waits for both agents to be listening AND registered. With the
	// registry write gated, that wait only succeeds if the agents' heartbeats
	// carried the token — so reaching this point already proves the spawn-time
	// --admin-token plumbing works.
	fix := setupE2E(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", fix.registryPort)

	do := func(method, path, tok string, body []byte) int {
		var r *http.Request
		var err error
		if body != nil {
			r, err = http.NewRequest(method, base+path, bytes.NewReader(body))
		} else {
			r, err = http.NewRequest(method, base+path, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		if tok != "" {
			r.Header.Set("X-Ahsir-Admin-Token", tok)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	// Control plane without token → 401.
	startBody, _ := json.Marshal(map[string]string{"name": "intruder", "workspace": "/tmp/x"})
	if st := do(http.MethodPost, "/admin/agents", "", startBody); st != http.StatusUnauthorized {
		t.Errorf("POST /admin/agents without token = %d, want 401", st)
	}
	if st := do(http.MethodDelete, "/admin/agents/teacher", "", nil); st != http.StatusUnauthorized {
		t.Errorf("DELETE /admin/agents/teacher without token = %d, want 401", st)
	}

	// Registry write without token → 401 (overwrite attack blocked).
	overwrite, _ := json.Marshal(map[string]any{"name": "teacher", "version": "9.9.9", "url": "http://evil/"})
	if st := do(http.MethodPost, "/agents", "", overwrite); st != http.StatusUnauthorized {
		t.Errorf("POST /agents without token = %d, want 401", st)
	}

	// Data/read plane stays open.
	for _, p := range []string{"/agents", "/agents/teacher", "/invocations", "/config/timeouts"} {
		if st := do(http.MethodGet, p, "", nil); st == http.StatusUnauthorized {
			t.Errorf("GET %s = 401, want open (data plane)", p)
		}
	}

	// Control plane WITH the token is accepted at the auth layer (DELETE of a
	// non-running agent is idempotent → not a 401).
	if st := do(http.MethodDelete, "/admin/agents/nonexistent", token, nil); st == http.StatusUnauthorized {
		t.Errorf("DELETE with valid token = 401, want accepted")
	}
}
