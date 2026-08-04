package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/scheduler"
)

func TestRenderAgentLifecycleHumanSeverity(t *testing.T) {
	items := []scheduler.AgentLifecycleSnapshot{
		{Name: "ready", State: scheduler.AgentLifecycleOnline, Reason: "healthy", Severity: scheduler.SeverityOK},
		{Name: "cold", State: scheduler.AgentLifecycleIdle, Reason: "scale-to-zero", Severity: scheduler.SeverityInfo, Wakeable: true},
		{Name: "bad", State: scheduler.AgentLifecycleInvalidConfig, Reason: "runtime.apiKey references unset env vars: API_KEY", Severity: scheduler.SeverityError},
	}
	var out bytes.Buffer
	failed := renderAgentLifecycles(&out, items)
	if !failed {
		t.Fatal("invalid config must make doctor fail")
	}
	text := out.String()
	for _, want := range []string{"✓ agent ready", "○ agent cold", "✗ agent bad"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestDoctorJSONReportPreservesLifecycleClassification(t *testing.T) {
	report := doctorReport{
		OK: false,
		Agents: []scheduler.AgentLifecycleSnapshot{{
			Name: "cold", State: scheduler.AgentLifecycleIdle, ReasonCode: "scale-to-zero",
			Severity: scheduler.SeverityInfo, Wakeable: true,
		}},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var decoded doctorReport
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Agents) != 1 || decoded.Agents[0].State != scheduler.AgentLifecycleIdle || !decoded.Agents[0].Wakeable {
		t.Fatalf("json parity lost: %+v", decoded)
	}
}
