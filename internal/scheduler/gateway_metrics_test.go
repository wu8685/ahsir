package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/wu8685/ahsir/internal/obs"
)

// newMeteredLedger returns a ledger wired to a fresh, isolated registry plus
// the metrics it feeds, so each test asserts against its own series.
func newMeteredLedger() (*InvocationLedger, *GatewayMetrics, *obs.Registry) {
	reg := obs.NewRegistry()
	gm := NewGatewayMetrics(reg)
	l := NewInvocationLedger()
	l.SetGatewayMetrics(gm)
	return l, gm, reg
}

func requestsTotal(t *testing.T, gm *GatewayMetrics, agent, source string, result obs.Result) float64 {
	t.Helper()
	return testutil.ToFloat64(gm.requests.WithLabelValues(agent, source, result.String()))
}

func inflight(t *testing.T, gm *GatewayMetrics, agent, source string) float64 {
	t.Helper()
	return testutil.ToFloat64(gm.inflight.WithLabelValues(agent, source))
}

func TestGatewayMetrics_CompleteLifecycle(t *testing.T) {
	l, gm, _ := newMeteredLedger()
	inv := l.Begin(InvocationMetadata{Source: InvocationSourceChatGateway, AgentName: "teacher", ContextID: "ctx-1"})
	if got := inflight(t, gm, "teacher", "chat_gateway"); got != 1 {
		t.Fatalf("inflight after Begin = %v, want 1", got)
	}
	time.Sleep(2 * time.Millisecond) // guarantee a non-zero duration to observe
	l.Complete(inv.ID)

	if got := requestsTotal(t, gm, "teacher", "chat_gateway", obs.ResultDone); got != 1 {
		t.Fatalf("requests_total{done} = %v, want 1", got)
	}
	if got := inflight(t, gm, "teacher", "chat_gateway"); got != 0 {
		t.Fatalf("inflight after Complete = %v, want 0", got)
	}
	if got := testutil.CollectAndCount(gm.duration); got == 0 {
		t.Fatal("duration histogram recorded no samples")
	}
}

func TestGatewayMetrics_FailResultLabelsAtSite(t *testing.T) {
	// Each taxonomy value the gateway can settle to lands on its own result
	// label, and busy never lands in an *_error bucket (§7).
	cases := []obs.Result{obs.ResultUpstreamError, obs.ResultBusy, obs.ResultCancel, obs.ResultTimeout}
	for _, want := range cases {
		t.Run(want.String(), func(t *testing.T) {
			l, gm, _ := newMeteredLedger()
			inv := l.Begin(InvocationMetadata{Source: InvocationSourceA2AProxy, AgentName: "coder"})
			l.FailResult(inv.ID, want, errors.New("boom"))
			if got := requestsTotal(t, gm, "coder", "a2a_proxy", want); got != 1 {
				t.Fatalf("requests_total{%s} = %v, want 1", want, got)
			}
			if want == obs.ResultBusy && want.IsError() {
				t.Fatal("busy must not be an error result")
			}
			if got := inflight(t, gm, "coder", "a2a_proxy"); got != 0 {
				t.Fatalf("inflight not settled: %v", got)
			}
		})
	}
}

func TestGatewayMetrics_FailMessageResult(t *testing.T) {
	// The A2A proxy path settles upstream 5xx / interrupted streams via
	// FailMessageResult with an explicit upstream_error label.
	l, gm, _ := newMeteredLedger()
	inv := l.Begin(InvocationMetadata{Source: InvocationSourceA2AProxy, AgentName: "coder"})
	l.FailMessageResult(inv.ID, obs.ResultUpstreamError, "upstream status 502")
	if got := requestsTotal(t, gm, "coder", "a2a_proxy", obs.ResultUpstreamError); got != 1 {
		t.Fatalf("requests_total{upstream_error} = %v, want 1", got)
	}
	if got := inflight(t, gm, "coder", "a2a_proxy"); got != 0 {
		t.Fatalf("inflight = %v, want 0", got)
	}
}

func TestGatewayMetrics_AsyncQueuedSingleSettle(t *testing.T) {
	l, gm, _ := newMeteredLedger()
	inv := l.Begin(InvocationMetadata{Source: InvocationSourceChatGateway, AgentName: "teacher"})
	l.Queued(inv.ID)
	// Queued is non-terminal: still in-flight, not yet counted.
	if got := inflight(t, gm, "teacher", "chat_gateway"); got != 1 {
		t.Fatalf("inflight after Queued = %v, want 1 (queued counts as in-flight)", got)
	}
	if got := requestsTotal(t, gm, "teacher", "chat_gateway", obs.ResultDone); got != 0 {
		t.Fatalf("requests_total counted a queued (non-terminal) record: %v", got)
	}
	l.Complete(inv.ID) // background poll settles it
	if got := requestsTotal(t, gm, "teacher", "chat_gateway", obs.ResultDone); got != 1 {
		t.Fatalf("requests_total{done} after settle = %v, want 1", got)
	}
	if got := inflight(t, gm, "teacher", "chat_gateway"); got != 0 {
		t.Fatalf("inflight after settle = %v, want 0", got)
	}
}

func TestGatewayMetrics_RecoveryNoDoubleCount(t *testing.T) {
	l, gm, _ := newMeteredLedger()
	inv := l.Begin(InvocationMetadata{Source: InvocationSourceChatGateway, AgentName: "teacher"})
	l.Fail(inv.ID, errors.New("crash")) // settle #1 (the only one)
	l.Recovering(inv.ID)
	l.Recovered(inv.ID)

	if got := requestsTotal(t, gm, "teacher", "chat_gateway", obs.ResultInternalError); got != 1 {
		t.Fatalf("requests_total{internal_error} = %v, want 1 (recovery must not re-count)", got)
	}
	// Balanced in-flight: one begin, exactly one settle -> back to 0, not -1.
	if got := inflight(t, gm, "teacher", "chat_gateway"); got != 0 {
		t.Fatalf("inflight = %v, want 0 (recovery decremented twice?)", got)
	}
}

func TestGatewayMetrics_NilSafe(t *testing.T) {
	// A ledger without a metrics sink must behave exactly as before.
	l := NewInvocationLedger()
	inv := l.Begin(InvocationMetadata{Source: InvocationSourceChatGateway, AgentName: "x"})
	l.Complete(inv.ID) // must not panic
	var gm *GatewayMetrics
	gm.begin("a", "b") // nil receiver, no panic
	gm.settle("a", "b", obs.ResultDone, time.Second, "ctx", "")
}

func TestGatewayMetrics_NoHighCardinalityLabels(t *testing.T) {
	// §5/§10 red-line assertion: after real traffic, no metric may carry a
	// contextID/messageID/invocationID/sessionID/traceID label KEY.
	l, gm, reg := newMeteredLedger()
	inv := l.Begin(InvocationMetadata{Source: InvocationSourceChatGateway, AgentName: "teacher", ContextID: "ctx-secret", MessageID: "msg-secret"})
	l.Complete(inv.ID)
	_ = gm

	forbidden := map[string]bool{
		"contextid": true, "contextId": true, "context_id": true,
		"messageid": true, "invocationid": true, "sessionid": true, "traceid": true,
	}
	mfs, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(mfs) == 0 {
		t.Fatal("no metrics gathered")
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				key := strings.ToLower(strings.ReplaceAll(lp.GetName(), "_", ""))
				if forbidden[key] {
					t.Fatalf("metric %s carries forbidden high-cardinality label %q", mf.GetName(), lp.GetName())
				}
			}
		}
	}
}

func TestClassifyChatError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want obs.Result
	}{
		{"nil", nil, obs.ResultDone},
		{"canceled", context.Canceled, obs.ResultCancel},
		{"deadline", context.DeadlineExceeded, obs.ResultTimeout},
		{"busy-wire-marker", errors.New("agent busy: a turn is already running"), obs.ResultBusy},
		{"wrapped-cancel", fmt.Errorf("chat: %w", context.Canceled), obs.ResultCancel},
		{"other", errors.New("connection refused"), obs.ResultUpstreamError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyChatError(c.err); got != c.want {
				t.Fatalf("classifyChatError(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

func TestClassifyProxyError(t *testing.T) {
	if got := classifyProxyError(context.Canceled); got != obs.ResultCancel {
		t.Fatalf("canceled -> %q, want cancel", got)
	}
	if got := classifyProxyError(context.DeadlineExceeded); got != obs.ResultTimeout {
		t.Fatalf("deadline -> %q, want timeout", got)
	}
	if got := classifyProxyError(errors.New("dial tcp: refused")); got != obs.ResultUpstreamError {
		t.Fatalf("dial error -> %q, want upstream_error", got)
	}
}

// TestGatewayMetricsEndpoint_EndToEnd is the §10 acceptance check: start a
// scheduler + a real agent, send a chat, then scrape /metrics and confirm the
// gateway request counter and duration histogram are present and non-empty.
func TestGatewayMetricsEndpoint_EndToEnd(t *testing.T) {
	sch, gwURL := newTestScheduler(t)
	agentURL := realAgent(t, "teacher", "hello from the teacher", 0)
	sch.Registry().Register(&a2a.AgentCard{
		Name:               "teacher",
		Version:            "1.0.0",
		URL:                agentURL,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	})

	for i := 0; i < 3; i++ {
		if status, body := postChat(t, gwURL, "teacher", "ping"); status != http.StatusOK {
			t.Fatalf("chat %d failed: %d %s", i, status, body)
		}
	}

	resp, err := http.Get(gwURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	for _, want := range []string{
		`ahsir_gateway_requests_total{agent="teacher",result="done",source="chat_gateway"} 3`,
		"ahsir_gateway_request_duration_seconds_count",
		"ahsir_gateway_requests_inflight",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("/metrics missing %q\n---\n%s", want, page)
		}
	}
}
