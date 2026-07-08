package scheduler

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wu8685/ahsir/internal/obs"
)

// GatewayMetrics is the §6 "A. Gateway" group: three metrics derived entirely
// from the invocation ledger's single begin/finish sink — no second set of
// probes. Every method is nil-safe so a ledger constructed without a registry
// (tests, zero-config) simply records nothing.
//
// Red line (§5): the labels here are ONLY the bounded enumerations
// {agent, source, result}. The high-cardinality contextID never becomes a
// label — it rides the duration histogram's exemplar instead.
type GatewayMetrics struct {
	requests *prometheus.CounterVec   // agent, source, result
	duration *prometheus.HistogramVec // agent, source
	inflight *prometheus.GaugeVec     // agent, source
}

// NewGatewayMetrics constructs and registers the Gateway A-group on reg. The
// guarded obs constructors enforce the naming/label contract at build time.
func NewGatewayMetrics(reg *obs.Registry) *GatewayMetrics {
	return &GatewayMetrics{
		requests: obs.NewCounterVec(reg, obs.SubsystemGateway, "requests_total",
			"Total gateway-mediated invocations, by agent/source/result.",
			obs.LabelAgent, obs.LabelSource, obs.LabelResult),
		duration: obs.NewHistogramVec(reg, obs.SubsystemGateway, "request_duration_seconds",
			"Gateway invocation wall-clock duration (FinishedAt-StartedAt); contextID rides the exemplar.",
			obs.DefaultDurationBuckets,
			obs.LabelAgent, obs.LabelSource),
		inflight: obs.NewGaugeVec(reg, obs.SubsystemGateway, "requests_inflight",
			"Gateway invocations currently in flight (queued counts as in-flight until settle).",
			obs.LabelAgent, obs.LabelSource),
	}
}

// begin marks an invocation as in-flight. Paired with a single settle().
func (m *GatewayMetrics) begin(agent, source string) {
	if m == nil {
		return
	}
	m.inflight.WithLabelValues(agent, source).Inc()
}

// settle records the terminal transition of an invocation exactly once: it
// increments requests_total{result}, observes the duration with a contextID
// exemplar, and decrements the in-flight gauge.
func (m *GatewayMetrics) settle(agent, source string, result obs.Result, dur time.Duration, contextID, traceID string) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(agent, source, result.String()).Inc()
	if dur > 0 {
		obs.ObserveWithExemplar(
			m.duration.WithLabelValues(agent, source),
			dur.Seconds(), contextID, traceID)
	}
	m.inflight.WithLabelValues(agent, source).Dec()
}
