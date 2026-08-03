package ahsir

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenStream_DoesNotRetryGenericBadGateway(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"proxy connection reset"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	if _, err := c.openStream(context.Background(), "cma-x-v1", []byte(`{}`), nil); err == nil {
		t.Fatal("expected 502 error")
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want exactly 1 at-most-once attempt", calls)
	}
}

type failingRoundTripper struct{ calls int }

func (rt *failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls++
	return nil, io.ErrUnexpectedEOF
}

func TestOpenStream_DoesNotRetryTransportError(t *testing.T) {
	rt := &failingRoundTripper{}
	c := New("http://scheduler.invalid", "")
	c.HTTP.Transport = rt
	if _, err := c.openStream(context.Background(), "cma-x-v1", []byte(`{}`), nil); err == nil {
		t.Fatal("expected transport error")
	}
	if rt.calls != 1 {
		t.Fatalf("requests = %d, want exactly 1 at-most-once attempt", rt.calls)
	}
}

func TestOpenStream_ClassifiesSchedulerAgentNotFoundBeforeStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(schedulerErrorCodeHeader, schedulerErrorAgentNotFound)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"agent not found"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.openStream(context.Background(), "cma-x-v4", []byte(`{}`), nil)
	var statusErr *PreStreamHTTPError
	if !errors.As(err, &statusErr) {
		t.Fatalf("err = %T %v, want *PreStreamHTTPError", err, err)
	}
	if statusErr.StatusCode != http.StatusNotFound || statusErr.Agent != "cma-x-v4" {
		t.Fatalf("typed error = %+v, want agent cma-x-v4 and 404", statusErr)
	}
	if !IsPreStreamRuntimeError(err) {
		t.Fatalf("scheduler agent-not-found must be eligible for reconcile: %v", err)
	}
}

func TestOpenStream_UpstreamAgentNotFoundBodyWithoutSchedulerMarkerIsNotRuntimeMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"agent not found"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.openStream(context.Background(), "cma-x-v4", []byte(`{}`), nil)
	if IsPreStreamRuntimeError(err) {
		t.Fatalf("unmarked upstream 404 must not be eligible for reconcile/replay: %v", err)
	}
}

func TestOpenStream_GenericNotFoundIsNotRuntimeMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"route not found"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	_, err := c.openStream(context.Background(), "cma-x-v4", []byte(`{}`), nil)
	if IsPreStreamRuntimeError(err) {
		t.Fatalf("generic 404 must not be eligible for reconcile/replay: %v", err)
	}
}

func TestPreStreamBadGatewayIsNotSafeToReplay(t *testing.T) {
	err := &PreStreamHTTPError{Agent: "cma-x-v4", StatusCode: http.StatusBadGateway, Detail: "proxy connection reset"}
	if IsPreStreamRuntimeError(err) {
		t.Fatal("ambiguous 502 must not be classified as safe to replay")
	}
}
