package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/wu8685/ahsir/internal/registry"
)

func TestAgentLifecyclesClassifyConfiguredOnlineAndIdle(t *testing.T) {
	cfg := &Config{Agents: []AgentConfig{
		{Name: "not-started", Workspace: t.TempDir()},
		{Name: "online", Workspace: t.TempDir()},
		{Name: "idle", Workspace: t.TempDir()},
	}}
	sch := New(cfg)
	sch.registry.Register(&a2a.AgentCard{Name: "online", URL: "http://127.0.0.1:1"})

	sch.mu.Lock()
	sch.idleStopped["idle"] = cfg.Agents[2]
	sch.setLifecycleLocked("idle", AgentLifecycleIdle, "scale-to-zero", "idle timeout elapsed", 0, time.Time{})
	sch.mu.Unlock()

	got := lifecycleByName(sch.AgentLifecycles())
	assertLifecycle(t, got["not-started"], AgentLifecycleStopped, "configured-not-started", SeverityWarning, false)
	assertLifecycle(t, got["online"], AgentLifecycleOnline, "healthy", SeverityOK, false)
	assertLifecycle(t, got["idle"], AgentLifecycleIdle, "scale-to-zero", SeverityInfo, true)
}

func TestAgentLifecycleFailureStatesRetainMachineReadableReasons(t *testing.T) {
	sch := New(&Config{})
	next := time.Now().Add(time.Minute).UTC().Truncate(time.Second)

	sch.mu.Lock()
	sch.setLifecycleLocked("invalid", AgentLifecycleInvalidConfig, "invalid-runtime", "runtime.apiKey references unset env vars: API_KEY", 0, time.Time{})
	sch.setLifecycleLocked("crashed", AgentLifecycleRestartBackoff, "process-exit", "process exited with status 1", 3, next)
	sch.setLifecycleLocked("unhealthy", AgentLifecycleHealthFailed, "health-threshold", "health check failed: status=500", 2, next)
	sch.mu.Unlock()

	got := lifecycleByName(sch.AgentLifecycles())
	assertLifecycle(t, got["invalid"], AgentLifecycleInvalidConfig, "invalid-runtime", SeverityError, false)
	assertLifecycle(t, got["crashed"], AgentLifecycleRestartBackoff, "process-exit", SeverityError, false)
	assertLifecycle(t, got["unhealthy"], AgentLifecycleHealthFailed, "health-threshold", SeverityError, false)
	if got["crashed"].RestartAttempt != 3 || got["crashed"].RestartAt == nil || !got["crashed"].RestartAt.Equal(next) {
		t.Fatalf("crash retry metadata = %+v", got["crashed"])
	}
}

func TestAgentLifecyclesClassifiesStaleConfiguredRemoteHeartbeat(t *testing.T) {
	sch := New(&Config{Agents: []AgentConfig{{Name: "remote", Remote: "https://agent.example"}}})
	sch.registry = registry.NewRegistry(time.Nanosecond)
	sch.registry.Register(&a2a.AgentCard{Name: "remote", URL: "https://agent.example"})
	time.Sleep(time.Millisecond)

	got := lifecycleByName(sch.AgentLifecycles())["remote"]
	assertLifecycle(t, got, AgentLifecycleHealthFailed, "heartbeat-timeout", SeverityError, false)
}

func TestStopAgentRecordsOperatorStopped(t *testing.T) {
	sch := New(&Config{Agents: []AgentConfig{{Name: "reviewer", Workspace: t.TempDir()}}})
	sch.desired["reviewer"] = sch.cfg.Agents[0]

	if err := sch.StopAgent("reviewer"); err != nil {
		t.Fatal(err)
	}
	got := lifecycleByName(sch.AgentLifecycles())["reviewer"]
	assertLifecycle(t, got, AgentLifecycleStopped, "operator-stopped", SeverityWarning, false)
}

func TestStopUnknownAgentDoesNotCreateLifecycleEntry(t *testing.T) {
	sch := New(&Config{})
	if err := sch.StopAgent("ghost"); err != nil {
		t.Fatal(err)
	}
	if _, exists := lifecycleByName(sch.AgentLifecycles())["ghost"]; exists {
		t.Fatal("idempotent stop created a phantom lifecycle entry")
	}
}

func TestDiagnosticsAgentsEndpointReturnsLifecycleJSON(t *testing.T) {
	sch := New(&Config{Agents: []AgentConfig{{Name: "idle", Workspace: t.TempDir()}}})
	sch.mu.Lock()
	sch.setLifecycleLocked("idle", AgentLifecycleIdle, "scale-to-zero", "idle timeout elapsed", 0, time.Time{})
	sch.mu.Unlock()

	h := newGatewayHandler(sch, registry.NewHTTPHandler(sch.registry))
	req := httptest.NewRequest(http.MethodGet, "/diagnostics/agents", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []AgentLifecycleSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "idle" || got[0].State != AgentLifecycleIdle || !got[0].Wakeable {
		t.Fatalf("unexpected lifecycle response: %+v", got)
	}
}

func TestSchedulerStartIsolatesInvalidRuntimeConfig(t *testing.T) {
	invalidWorkspace := writeLifecycleCard(t, "invalid", `runtime:
  provider: zhipu
  apiKey: ${AHSIR_TEST_MISSING_KEY}
`)
	validWorkspace := writeLifecycleCard(t, "valid", "runtime:\n  provider: echo\n")
	oldKey, hadKey := os.LookupEnv("AHSIR_TEST_MISSING_KEY")
	_ = os.Unsetenv("AHSIR_TEST_MISSING_KEY")
	t.Cleanup(func() {
		if hadKey {
			_ = os.Setenv("AHSIR_TEST_MISSING_KEY", oldKey)
		} else {
			_ = os.Unsetenv("AHSIR_TEST_MISSING_KEY")
		}
	})

	cfg := &Config{
		Agents: []AgentConfig{
			{Name: "invalid", Workspace: invalidWorkspace, Port: 23001},
			{Name: "valid", Workspace: validWorkspace, Port: 23002},
		},
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: 0},
		PortRange: PortRange{Start: 23001, End: 23010},
	}
	sch := New(cfg)
	sch.supervisor.HealthCheckEnabled = false
	sch.findLocalListener = func(int) (localProcessInfo, bool, error) { return localProcessInfo{}, false, nil }
	sch.agentCommand = func(ctx context.Context, _ string, _ AgentConfig, _ string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "sleep 30")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sch.Start(ctx); err != nil {
		t.Fatalf("scheduler should isolate invalid agent config: %v", err)
	}
	t.Cleanup(sch.Stop)

	sch.mu.Lock()
	_, invalidStarted := sch.agents["invalid"]
	_, validStarted := sch.agents["valid"]
	sch.mu.Unlock()
	if invalidStarted || !validStarted {
		t.Fatalf("started states: invalid=%v valid=%v", invalidStarted, validStarted)
	}
	invalid := lifecycleByName(sch.AgentLifecycles())["invalid"]
	assertLifecycle(t, invalid, AgentLifecycleInvalidConfig, "invalid-runtime", SeverityError, false)
	if !strings.Contains(invalid.Reason, "AHSIR_TEST_MISSING_KEY") {
		t.Fatalf("reason is not actionable: %q", invalid.Reason)
	}
}

func writeLifecycleCard(t *testing.T, name, extra string) string {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(workspace, ".a2a"), 0o755); err != nil {
		t.Fatal(err)
	}
	card := "name: " + name + "\nversion: 1.0.0\nskills: []\n" + extra
	if err := os.WriteFile(filepath.Join(workspace, ".a2a", "agent-card.yaml"), []byte(card), 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func lifecycleByName(in []AgentLifecycleSnapshot) map[string]AgentLifecycleSnapshot {
	out := make(map[string]AgentLifecycleSnapshot, len(in))
	for _, item := range in {
		out[item.Name] = item
	}
	return out
}

func assertLifecycle(t *testing.T, got AgentLifecycleSnapshot, state AgentLifecycleState, reason string, severity AgentLifecycleSeverity, wakeable bool) {
	t.Helper()
	if got.State != state || got.ReasonCode != reason || got.Severity != severity || got.Wakeable != wakeable {
		t.Fatalf("lifecycle = %+v, want state=%s reason=%s severity=%s wakeable=%v", got, state, reason, severity, wakeable)
	}
}
