package scheduler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/registry"
)

func TestPreflightFilesystemAllowsRootsDescendantsAndMultipleRoots(t *testing.T) {
	workdir := t.TempDir()
	rootA := filepath.Join(workdir, "project-a")
	rootB := filepath.Join(workdir, "project-b")
	mustMkdirAll(t, filepath.Join(rootA, "sub"))
	mustMkdirAll(t, rootB)
	sch := schedulerWithFilesystemCard(t, workdir, true, []string{"project-a", rootB})

	for _, required := range [][]string{
		{rootA},
		{filepath.Join(rootA, "sub")},
		{filepath.Join(rootA, "sub"), rootB},
		{"project-a/sub"},
	} {
		if err := sch.PreflightFilesystem("reviewer", required); err != nil {
			t.Fatalf("required %v rejected: %v", required, err)
		}
	}
}

func TestPreflightFilesystemRejectsSiblingTraversalAndDisabled(t *testing.T) {
	workdir := t.TempDir()
	allowed := filepath.Join(workdir, "project")
	sibling := filepath.Join(workdir, "project-evil")
	secret := filepath.Join(workdir, "secret")
	mustMkdirAll(t, allowed)
	mustMkdirAll(t, sibling)
	mustMkdirAll(t, secret)
	sch := schedulerWithFilesystemCard(t, workdir, true, []string{allowed})

	for _, denied := range []string{sibling, filepath.Join(allowed, "..", "secret")} {
		err := sch.PreflightFilesystem("reviewer", []string{denied})
		assertPreflightCode(t, err, "filesystem_access_denied", denied)
	}

	disabled := schedulerWithFilesystemCardNamed(t, "disabled", workdir, false, nil)
	err := disabled.PreflightFilesystem("disabled", []string{allowed})
	assertPreflightCode(t, err, "filesystem_disabled", allowed)
}

func TestPreflightFilesystemResolvesSymlinks(t *testing.T) {
	workdir := t.TempDir()
	allowed := filepath.Join(workdir, "allowed")
	outside := filepath.Join(workdir, "outside")
	mustMkdirAll(t, allowed)
	mustMkdirAll(t, outside)
	insideLink := filepath.Join(allowed, "escape")
	outsideLink := filepath.Join(outside, "enter")
	if err := os.Symlink(outside, insideLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(allowed, outsideLink); err != nil {
		t.Fatal(err)
	}
	sch := schedulerWithFilesystemCard(t, workdir, true, []string{allowed})

	err := sch.PreflightFilesystem("reviewer", []string{insideLink})
	assertPreflightCode(t, err, "filesystem_access_denied", insideLink)
	if err := sch.PreflightFilesystem("reviewer", []string{outsideLink}); err != nil {
		t.Fatalf("symlink targeting allowed root rejected: %v", err)
	}
}

func TestPreflightFilesystemRejectsMissingAndUnknownPolicy(t *testing.T) {
	workdir := t.TempDir()
	allowed := filepath.Join(workdir, "allowed")
	mustMkdirAll(t, allowed)
	sch := schedulerWithFilesystemCard(t, workdir, true, []string{allowed})

	missing := filepath.Join(allowed, "missing")
	assertPreflightCode(t, sch.PreflightFilesystem("reviewer", []string{missing}), "filesystem_path_invalid", missing)
	assertPreflightCode(t, sch.PreflightFilesystem("remote", []string{allowed}), "filesystem_policy_unavailable", allowed)
}

func TestChatGatewayRejectsFilesystemMismatchBeforeWakeAndLedger(t *testing.T) {
	workdir := t.TempDir()
	allowed := filepath.Join(workdir, "allowed")
	denied := filepath.Join(workdir, "denied")
	mustMkdirAll(t, allowed)
	mustMkdirAll(t, denied)
	sch := schedulerWithFilesystemCard(t, workdir, true, []string{allowed})
	sch.mu.Lock()
	sch.running = true
	sch.ctx = t.Context()
	sch.idleStopped["reviewer"] = sch.desired["reviewer"]
	sch.mu.Unlock()

	h := newGatewayHandler(sch, registry.NewHTTPHandler(sch.registry))
	body := `{"message":"review","requiredPaths":[` + mustJSON(t, denied) + `]}`
	req := httptest.NewRequest(http.MethodPost, "/agents/reviewer/chat", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["code"] != "filesystem_access_denied" || got["agent"] != "reviewer" || got["path"] != denied {
		t.Fatalf("unexpected structured error: %#v", got)
	}
	if strings.Contains(rec.Body.String(), allowed) {
		t.Fatalf("response leaked allowlist: %s", rec.Body.String())
	}
	if len(sch.Invocations().Snapshot()) != 0 {
		t.Fatal("denied dispatch must not create a ledger invocation")
	}
	sch.mu.Lock()
	_, stillIdle := sch.idleStopped["reviewer"]
	_, started := sch.agents["reviewer"]
	sch.mu.Unlock()
	if !stillIdle || started {
		t.Fatalf("denied dispatch changed wake state: idle=%v started=%v", stillIdle, started)
	}
}

func TestA2AGatewayRejectsFilesystemMismatchBeforeWake(t *testing.T) {
	workdir := t.TempDir()
	allowed := filepath.Join(workdir, "allowed")
	denied := filepath.Join(workdir, "denied")
	mustMkdirAll(t, allowed)
	mustMkdirAll(t, denied)
	sch := schedulerWithFilesystemCard(t, workdir, true, []string{allowed})
	sch.mu.Lock()
	sch.running = true
	sch.ctx = t.Context()
	sch.idleStopped["reviewer"] = sch.desired["reviewer"]
	sch.mu.Unlock()

	payload := map[string]any{
		"jsonrpc": "2.0", "method": "message/send", "id": 1,
		"params": map[string]any{"message": map[string]any{
			"messageId": "m1", "role": "user", "parts": []map[string]any{{"kind": "text", "text": "review"}},
			"metadata": map[string]any{"requiredFilesystemPaths": []string{denied}},
		}},
	}
	raw, _ := json.Marshal(payload)
	h := newGatewayHandler(sch, registry.NewHTTPHandler(sch.registry))
	req := httptest.NewRequest(http.MethodPost, "/a2a/reviewer", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "filesystem_access_denied") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	sch.mu.Lock()
	_, stillIdle := sch.idleStopped["reviewer"]
	sch.mu.Unlock()
	if !stillIdle {
		t.Fatal("native A2A denial woke the agent")
	}
}

func TestA2AGatewayRejectsMalformedRequiredPaths(t *testing.T) {
	for _, value := range []string{`"/tmp"`, `null`, `[""]`} {
		sch := New(&Config{})
		payload := `{"jsonrpc":"2.0","method":"message/send","params":{"message":{"metadata":{"requiredFilesystemPaths":` + value + `}}},"id":1}`
		h := newGatewayHandler(sch, registry.NewHTTPHandler(sch.registry))
		req := httptest.NewRequest(http.MethodPost, "/a2a/reviewer", strings.NewReader(payload))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("value=%s status=%d body=%s", value, rec.Code, rec.Body.String())
		}
	}
}

func schedulerWithFilesystemCard(t *testing.T, workdir string, enabled bool, allowed []string) *Scheduler {
	return schedulerWithFilesystemCardNamed(t, "reviewer", workdir, enabled, allowed)
}

func schedulerWithFilesystemCardNamed(t *testing.T, name, workdir string, enabled bool, allowed []string) *Scheduler {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), name)
	mustMkdirAll(t, filepath.Join(workspace, ".a2a"))
	card := "name: " + name + "\nversion: 1.0.0\nskills: []\nfilesystem:\n  enabled: " + mustJSON(t, enabled) + "\n"
	if len(allowed) > 0 {
		card += "  allowed_paths:\n"
		for _, path := range allowed {
			card += "    - " + mustJSON(t, path) + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, ".a2a", "agent-card.yaml"), []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := AgentConfig{Name: name, Workspace: workspace, Workdir: workdir}
	sch := New(&Config{Agents: []AgentConfig{cfg}})
	sch.desired[name] = cfg
	return sch
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertPreflightCode(t *testing.T, err error, code, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s for %s", code, path)
	}
	preflight, ok := err.(*FilesystemPreflightError)
	if !ok || preflight.Code != code || preflight.Path != path {
		t.Fatalf("error = %#v, want code=%s path=%s", err, code, path)
	}
}
