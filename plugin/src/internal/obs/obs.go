// Package obs is the observability foundation for ahsir (issue #7, phase ①:
// metrics + correlation, tracing boundaries deferred).
//
// It owns three things every future metric must go through so the fleet's
// telemetry stays coherent and safe:
//
//   - The naming/label CONTRACT (§4): metrics are named
//     ahsir_<subsystem>_<name>_<unit>, counters end in _total, and only a
//     small bounded set of label keys is allowed.
//   - The high-cardinality RED LINE (§5): unbounded IDs (contextID,
//     messageID, invocationID, sessionID) must never become metric labels —
//     they belong in logs, span attributes, and histogram exemplars only.
//     The guarded constructors here PANIC at construction time if a caller
//     tries, so the mistake is caught by `go test`, never in production.
//   - Explicit registerer INJECTION (§4.3): everything registers through a
//     *Registry passed in by the constructor. prometheus.DefaultRegisterer is
//     never used — that keeps tests isolated and makes duplicate registration
//     impossible to trigger by accident.
//
// How to add a metric: see obs/metrics.go for a copy-paste template.
package obs

import (
	"fmt"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Namespace prefixes every ahsir metric name: ahsir_<subsystem>_<name>_<unit>.
const Namespace = "ahsir"

// Subsystem values are the only allowed <subsystem> segments (§4.1). Keeping
// this a closed set stops metric names from sprawling across ad-hoc prefixes.
const (
	SubsystemGateway  = "gateway"
	SubsystemTurn     = "turn"
	SubsystemSession  = "session"
	SubsystemProvider = "provider"
	SubsystemAgent    = "agent"
)

var allowedSubsystems = map[string]bool{
	SubsystemGateway:  true,
	SubsystemTurn:     true,
	SubsystemSession:  true,
	SubsystemProvider: true,
	SubsystemAgent:    true,
}

// Label keys — the ONLY keys any ahsir metric may carry (§4.2, §5). Every one
// is a bounded enumeration, so time-series cardinality stays finite.
const (
	LabelAgent    = "agent"
	LabelSource   = "source"
	LabelResult   = "result"
	LabelProvider = "provider"
	LabelOutcome  = "outcome"
	LabelReason   = "reason"
	// LabelTargetAgent is the callee side of an A2A call. It is still a bounded
	// enumeration (the set of agent names), so it is allowed alongside `agent`.
	LabelTargetAgent = "target_agent"
)

var allowedLabels = map[string]bool{
	LabelAgent:       true,
	LabelSource:      true,
	LabelResult:      true,
	LabelProvider:    true,
	LabelOutcome:     true,
	LabelReason:      true,
	LabelTargetAgent: true,
}

// highCardinalityLabels are the specific unbounded IDs §5 forbids as labels.
// They are called out by name (across the casing/underscore variants a coder
// might reach for) so the panic message names the red line that was crossed
// instead of a generic "unknown label".
var highCardinalityLabels = map[string]bool{
	"contextid": true,
	"messageid": true,
	"invocationid": true,
	"sessionid": true,
	"traceid": true,
	"spanid": true,
}

// baseUnitSuffixes are the base units §4.1 allows on a metric name. Counters
// are exempt (they end in _total); gauges that are plain counts (inflight,
// alive, depth) need no unit.
var baseUnitSuffixes = []string{"_seconds", "_bytes"}

// ValidateLabels reports the first label key that violates the contract, or nil
// if every key is an allowed bounded enumeration. Exported so a test can assert
// a metric's label set against the whitelist (§10 red-line check).
func ValidateLabels(names ...string) error {
	for _, name := range names {
		if allowedLabels[name] {
			continue
		}
		if highCardinalityLabels[strings.ToLower(strings.ReplaceAll(name, "_", ""))] {
			return fmt.Errorf("obs: label %q is a high-cardinality id and must NOT be a metric label (§5 red line); put it in a log field, span attribute, or histogram exemplar instead", name)
		}
		return fmt.Errorf("obs: label %q is not in the allowed bounded set %v (§4.2)", name, allowedLabelList())
	}
	return nil
}

func allowedLabelList() []string {
	out := make([]string, 0, len(allowedLabels))
	for k := range allowedLabels {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkContract enforces §4.1/§4.2 at construction time. A violation is a
// programmer error (a wrong metric definition), so it panics — surfacing in
// `go test` rather than silently shipping a malformed or unbounded metric.
func checkContract(subsystem, name string, requireTotal, requireUnit bool, labels []string) {
	if !allowedSubsystems[subsystem] {
		panic(fmt.Sprintf("obs: subsystem %q is not allowed; use one of gateway/turn/session/provider/agent (§4.1)", subsystem))
	}
	if requireTotal && !strings.HasSuffix(name, "_total") {
		panic(fmt.Sprintf("obs: counter %q must end in _total (§4.1)", name))
	}
	if requireUnit && !hasBaseUnit(name) {
		panic(fmt.Sprintf("obs: metric %q must end in a base unit %v (§4.1)", name, baseUnitSuffixes))
	}
	if err := ValidateLabels(labels...); err != nil {
		panic(err.Error())
	}
}

func hasBaseUnit(name string) bool {
	for _, suffix := range baseUnitSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// Registry is the single, explicitly-injected metric registerer for a process
// (§4.3). Construct one in main / the top-level component and thread it into
// every collector. Never fall back to prometheus.DefaultRegisterer.
type Registry struct {
	reg *prometheus.Registry
}

// NewRegistry returns a fresh, empty Registry. Each call is fully isolated,
// which is exactly what tests need to avoid the duplicate-registration panics
// a shared global would produce.
func NewRegistry() *Registry {
	return &Registry{reg: prometheus.NewRegistry()}
}

// Registerer exposes the underlying prometheus.Registerer for callers that use
// prometheus constructors directly. Prefer the guarded NewCounterVec/etc below.
func (r *Registry) Registerer() prometheus.Registerer { return r.reg }

// Gatherer exposes the underlying gatherer for the /metrics HTTP handler.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// mustRegister registers c on r, tolerating a re-registration of an identical
// collector: AlreadyRegisteredError returns the existing collector instead of
// panicking, so wiring the same metric twice (e.g. two components sharing one
// registry in a test) is safe (§4.3 "重复注册不 panic").
func mustRegister(r *Registry, c prometheus.Collector) {
	if r == nil {
		return
	}
	if err := r.reg.Register(c); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return
		}
		panic(err)
	}
}

// NewCounterVec builds a contract-checked counter and registers it on r.
// name is the <name>_<unit> tail (e.g. "requests_total"); the full metric name
// becomes ahsir_<subsystem>_<name>. Panics if the contract is violated.
func NewCounterVec(r *Registry, subsystem, name, help string, labels ...string) *prometheus.CounterVec {
	checkContract(subsystem, name, true, false, labels)
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Subsystem: subsystem,
		Name:      name,
		Help:      help,
	}, labels)
	mustRegister(r, c)
	return c
}

// NewGaugeVec builds a contract-checked gauge and registers it on r. Gauges are
// exempt from the _total and base-unit rules (they are plain counts/levels).
func NewGaugeVec(r *Registry, subsystem, name, help string, labels ...string) *prometheus.GaugeVec {
	checkContract(subsystem, name, false, false, labels)
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Subsystem: subsystem,
		Name:      name,
		Help:      help,
	}, labels)
	mustRegister(r, g)
	return g
}

// NewHistogramVec builds a contract-checked histogram and registers it on r.
// The name must end in a base unit (_seconds/_bytes). Pass nil buckets to use
// DefaultDurationBuckets.
func NewHistogramVec(r *Registry, subsystem, name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	checkContract(subsystem, name, false, true, labels)
	if buckets == nil {
		buckets = DefaultDurationBuckets
	}
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: Namespace,
		Subsystem: subsystem,
		Name:      name,
		Help:      help,
		Buckets:   buckets,
	}, labels)
	mustRegister(r, h)
	return h
}
