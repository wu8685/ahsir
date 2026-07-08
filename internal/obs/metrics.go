package obs

import "github.com/prometheus/client_golang/prometheus"

// DefaultDurationBuckets is the shared latency bucket layout (seconds) for
// ahsir duration histograms. It spans sub-100ms turns through the 120s default
// chat timeout so both cold starts and slow turns land in a meaningful bucket.
var DefaultDurationBuckets = []float64{
	0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120,
}

// ExemplarKeyContextID / ExemplarKeyTraceID are the exemplar label keys used to
// hang a high-cardinality id off a histogram observation. These live ONLY in
// exemplars (never as metric labels), which is exactly what §5 sanctions for
// "find the specific slow turn by contextID".
const (
	ExemplarKeyContextID = "contextID"
	ExemplarKeyTraceID   = "traceID"
)

// ObserveWithExemplar records v on the observer and, when the observer supports
// exemplars, attaches the contextID/traceID as an exemplar. Empty ids are
// dropped so we never emit blank exemplar labels. If the observer does not
// implement prometheus.ExemplarObserver it falls back to a plain Observe.
//
// This is the ONLY place high-cardinality ids meet a metric — as an exemplar,
// not a label (§5).
func ObserveWithExemplar(o prometheus.Observer, v float64, contextID, traceID string) {
	ex, ok := o.(prometheus.ExemplarObserver)
	if !ok {
		o.Observe(v)
		return
	}
	labels := prometheus.Labels{}
	if contextID != "" {
		labels[ExemplarKeyContextID] = contextID
	}
	if traceID != "" {
		labels[ExemplarKeyTraceID] = traceID
	}
	if len(labels) == 0 {
		o.Observe(v)
		return
	}
	ex.ObserveWithExemplar(v, labels)
}

// NewTurnDurationSeconds is the §4.4 "how to add a metric" template. It is the
// canonical shape every future collector (including issue #6's scale-to-zero
// metrics) should copy:
//
//  1. declare the metric with a guarded constructor (labels are checked against
//     the bounded whitelist, the name against the naming convention);
//  2. register it on the injected *Registry — never DefaultRegisterer;
//  3. at the collection point, observe with an exemplar so a slow turn can be
//     traced by contextID without contextID ever becoming a label.
//
// Usage:
//
//	turnDur := obs.NewTurnDurationSeconds(reg)
//	obs.ObserveWithExemplar(
//	    turnDur.WithLabelValues(agent, provider, string(result)),
//	    elapsed.Seconds(), contextID, traceID)
//
// The labels {agent, provider, result} are all bounded enumerations; the
// contextID rides the exemplar. This is the exact pattern the Gateway A-group
// (see internal/scheduler/gateway_metrics.go) already follows.
func NewTurnDurationSeconds(reg *Registry) *prometheus.HistogramVec {
	return NewHistogramVec(reg, SubsystemTurn, "duration_seconds",
		"Duration of a full agent turn, labeled by agent/provider/result; contextID rides the exemplar.",
		DefaultDurationBuckets,
		LabelAgent, LabelProvider, LabelResult)
}
