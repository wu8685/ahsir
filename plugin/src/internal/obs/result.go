package obs

// Result is the `result` label taxonomy (§7). It is a CLOSED enumeration: a
// turn / gateway request / A2A call settles into exactly one of these, and the
// label is chosen at the error-production site — never by parsing an error
// string after the fact (§6 implementation red line).
//
// The critical invariant is that busy is NOT an error: a full turn queue is
// normal backpressure (the request was rejected, never executed). Folding it
// into an *_error bucket would misreport healthy queueing as a fault (§7 note).
type Result string

const (
	// ResultDone is a normal completion.
	ResultDone Result = "done"
	// ResultBusy is a backpressure rejection: the turn queue was full so the
	// request was declined WITHOUT executing. Not a failure.
	ResultBusy Result = "busy"
	// ResultCancel is caller-context cancellation.
	ResultCancel Result = "cancel"
	// ResultTimeout is a runtime/chat timeout.
	ResultTimeout Result = "timeout"
	// ResultEvict is a capacity-policy interruption (unhealthy/LRU eviction).
	ResultEvict Result = "evict"
	// ResultProviderError is a provider subprocess error / mid-turn exit.
	ResultProviderError Result = "provider_error"
	// ResultUpstreamError is a gateway-only upstream 5xx / interrupted stream.
	ResultUpstreamError Result = "upstream_error"
	// ResultInternalError is the catch-all for other internal failures.
	ResultInternalError Result = "internal_error"
)

// AllResults is the full taxonomy, in the order documented in §7.
var AllResults = []Result{
	ResultDone,
	ResultBusy,
	ResultCancel,
	ResultTimeout,
	ResultEvict,
	ResultProviderError,
	ResultUpstreamError,
	ResultInternalError,
}

// Valid reports whether r is a known taxonomy value.
func (r Result) Valid() bool {
	for _, v := range AllResults {
		if r == v {
			return true
		}
	}
	return false
}

// IsError reports whether r denotes a genuine failure. Only the three *_error
// results count: done/busy/cancel/timeout/evict are normal outcomes. This is
// the programmatic guarantee behind §7's "busy must not mix into *_error".
func (r Result) IsError() bool {
	switch r {
	case ResultProviderError, ResultUpstreamError, ResultInternalError:
		return true
	default:
		return false
	}
}

// String makes Result printable and usable as a Prometheus label value.
func (r Result) String() string { return string(r) }
