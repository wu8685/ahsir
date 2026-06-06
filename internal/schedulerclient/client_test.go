package schedulerclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRefreshTimeoutAddsClientBufferForPositiveChatTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config/timeouts" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":"10m0s"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	got, err := c.RefreshTimeout()
	if err != nil {
		t.Fatal(err)
	}
	if got != 11*time.Minute {
		t.Fatalf("expected client timeout 11m, got %s", got)
	}
}

func TestRefreshTimeoutZeroChatTimeoutDisablesClientTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":"0s"}`))
	}))
	defer srv.Close()

	c := NewSchedulerHTTPClient(srv.URL)
	got, err := c.RefreshTimeout()
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("expected client timeout 0 for no-deadline scheduler chat, got %s", got)
	}
	if c.httpc.Timeout != 0 {
		t.Fatalf("expected http client timeout 0, got %s", c.httpc.Timeout)
	}
}
