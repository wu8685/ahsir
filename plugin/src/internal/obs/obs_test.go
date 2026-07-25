package obs

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestValidateLabels_AllowsBoundedSet(t *testing.T) {
	if err := ValidateLabels(LabelAgent, LabelSource, LabelResult, LabelProvider, LabelOutcome, LabelReason, LabelTargetAgent); err != nil {
		t.Fatalf("bounded labels rejected: %v", err)
	}
}

func TestValidateLabels_RejectsHighCardinalityIDs(t *testing.T) {
	// §5 red line: every casing/underscore variant a coder might reach for
	// must be rejected as a high-cardinality id, not slip through.
	for _, name := range []string{
		"contextID", "contextId", "context_id",
		"messageID", "messageId", "message_id",
		"invocationID", "invocation_id",
		"sessionID", "session_id",
		"traceID", "spanID",
	} {
		err := ValidateLabels(name)
		if err == nil {
			t.Fatalf("label %q should have been rejected as high-cardinality", name)
		}
		if !strings.Contains(err.Error(), "high-cardinality") {
			t.Fatalf("label %q rejected with wrong reason: %v", name, err)
		}
	}
}

func TestValidateLabels_RejectsUnknownKey(t *testing.T) {
	err := ValidateLabels("region")
	if err == nil {
		t.Fatal("unknown label key should be rejected")
	}
	if strings.Contains(err.Error(), "high-cardinality") {
		t.Fatalf("unknown key misreported as high-cardinality: %v", err)
	}
}

func requirePanic(t *testing.T, wantSubstr string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q, got none", wantSubstr)
		}
		if msg, ok := r.(string); ok && !strings.Contains(msg, wantSubstr) {
			t.Fatalf("panic %q does not contain %q", msg, wantSubstr)
		}
	}()
	fn()
}

func TestNewCounterVec_ContractViolationsPanic(t *testing.T) {
	reg := NewRegistry()
	requirePanic(t, "subsystem", func() {
		NewCounterVec(reg, "bogus", "requests_total", "help", LabelAgent)
	})
	requirePanic(t, "_total", func() {
		NewCounterVec(reg, SubsystemGateway, "requests", "help", LabelAgent)
	})
	requirePanic(t, "high-cardinality", func() {
		NewCounterVec(reg, SubsystemGateway, "requests_total", "help", "contextID")
	})
}

func TestNewHistogramVec_RequiresBaseUnit(t *testing.T) {
	reg := NewRegistry()
	requirePanic(t, "base unit", func() {
		NewHistogramVec(reg, SubsystemTurn, "duration", "help", nil, LabelAgent)
	})
	// A valid unit must not panic.
	NewHistogramVec(reg, SubsystemTurn, "duration_seconds", "help", nil, LabelAgent)
}

func TestNewGaugeVec_NoUnitRequired(t *testing.T) {
	reg := NewRegistry()
	// Gauges are plain levels — inflight has no unit and must be accepted.
	g := NewGaugeVec(reg, SubsystemGateway, "requests_inflight", "help", LabelAgent, LabelSource)
	g.WithLabelValues("a", "chat_gateway").Set(3)
	if got := testutil.ToFloat64(g.WithLabelValues("a", "chat_gateway")); got != 3 {
		t.Fatalf("gauge value = %v, want 3", got)
	}
}

func TestRegistry_IsolationAndGather(t *testing.T) {
	reg1 := NewRegistry()
	reg2 := NewRegistry()
	c1 := NewCounterVec(reg1, SubsystemGateway, "requests_total", "help", LabelAgent)
	c1.WithLabelValues("a").Inc()

	// reg2 shares nothing with reg1 — building the same-named metric on a
	// separate registry must not panic (no shared global) and must gather 0.
	NewCounterVec(reg2, SubsystemGateway, "requests_total", "help", LabelAgent)

	if n := testutil.CollectAndCount(c1); n != 1 {
		t.Fatalf("reg1 counter series = %d, want 1", n)
	}
	mfs, err := reg2.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather reg2: %v", err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if m.GetCounter().GetValue() != 0 {
				t.Fatalf("reg2 leaked a value from reg1: %v", m)
			}
		}
	}
}

func TestNewCounterVec_DuplicateRegistrationNoPanic(t *testing.T) {
	// §4.3: registering the same collector twice on one registry must be
	// tolerated (AlreadyRegisteredError swallowed), never a panic.
	reg := NewRegistry()
	NewCounterVec(reg, SubsystemGateway, "requests_total", "help", LabelAgent)
	// Second identical construction on the same registry: no panic.
	NewCounterVec(reg, SubsystemGateway, "requests_total", "help", LabelAgent)
}

func TestGuardedMetricNamesFollowConvention(t *testing.T) {
	reg := NewRegistry()
	c := NewCounterVec(reg, SubsystemGateway, "requests_total", "help", LabelAgent)
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	c.WithLabelValues("a").Inc()
	mfs, _ = reg.Gatherer().Gather()
	if len(mfs) != 1 {
		t.Fatalf("want 1 metric family, got %d", len(mfs))
	}
	if got := mfs[0].GetName(); got != "ahsir_gateway_requests_total" {
		t.Fatalf("metric name = %q, want ahsir_gateway_requests_total", got)
	}
}
