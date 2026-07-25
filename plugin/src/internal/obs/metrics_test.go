package obs

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func TestObserveWithExemplar_AttachesContextAndTrace(t *testing.T) {
	reg := NewRegistry()
	h := NewHistogramVec(reg, SubsystemTurn, "duration_seconds", "help", nil, LabelAgent)
	ObserveWithExemplar(h.WithLabelValues("teacher"), 1.5, "ctx-123", "trace-abc")

	ex := scrapeExemplar(t, reg, "ahsir_turn_duration_seconds")
	got := map[string]string{}
	for _, lp := range ex.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	if got[ExemplarKeyContextID] != "ctx-123" {
		t.Fatalf("exemplar contextID = %q, want ctx-123", got[ExemplarKeyContextID])
	}
	if got[ExemplarKeyTraceID] != "trace-abc" {
		t.Fatalf("exemplar traceID = %q, want trace-abc", got[ExemplarKeyTraceID])
	}
}

func TestObserveWithExemplar_EmptyIDsNoExemplar(t *testing.T) {
	reg := NewRegistry()
	h := NewHistogramVec(reg, SubsystemTurn, "duration_seconds", "help", nil, LabelAgent)
	ObserveWithExemplar(h.WithLabelValues("teacher"), 0.5, "", "")

	// Observation still recorded, but no exemplar attached (blank labels would
	// be meaningless and could even be rejected by the exposition format).
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "ahsir_turn_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if m.GetHistogram().GetSampleCount() != 1 {
				t.Fatalf("sample count = %d, want 1", m.GetHistogram().GetSampleCount())
			}
			for _, b := range m.GetHistogram().GetBucket() {
				if b.Exemplar != nil {
					t.Fatalf("unexpected exemplar on empty-id observation: %v", b.Exemplar)
				}
			}
		}
	}
}

func TestNewTurnDurationSeconds_ContractLabels(t *testing.T) {
	// The §4.4 template must itself honor the contract (guarded constructor).
	reg := NewRegistry()
	h := NewTurnDurationSeconds(reg)
	// {agent, provider, result} — all bounded — must be accepted.
	h.WithLabelValues("teacher", "claude", string(ResultDone)).Observe(0.2)
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(mfs) != 1 || mfs[0].GetName() != "ahsir_turn_duration_seconds" {
		t.Fatalf("unexpected metric families: %v", mfs)
	}
}

// scrapeExemplar returns the first exemplar attached to any bucket of the named
// histogram in reg.
func scrapeExemplar(t *testing.T, reg *Registry, name string) *dto.Exemplar {
	t.Helper()
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, b := range m.GetHistogram().GetBucket() {
				if b.Exemplar != nil {
					return b.Exemplar
				}
			}
		}
	}
	t.Fatalf("no exemplar found on %s", name)
	return nil
}
