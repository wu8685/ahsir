package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestHTTPClientSendRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		var req a2a.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Method != "message/send" {
			t.Errorf("expected message/send, got %s", req.Method)
		}
		resp := a2a.NewJSONRPCResponse(req.ID, json.RawMessage(`{"ok":true}`))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 5*time.Second)
	req := a2a.NewJSONRPCRequest("message/send", json.RawMessage(`{"msg":"hello"}`))
	resp, err := client.Send(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
}

func TestHTTPClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 10*time.Millisecond)
	req := a2a.NewJSONRPCRequest("test", nil)
	_, err := client.Send(context.Background(), req)
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestHTTPClientContextCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := NewHTTPClient(server.URL, 5*time.Second)
	cancel() // cancel immediately
	_, err := client.Send(ctx, a2a.NewJSONRPCRequest("test", nil))
	if err == nil {
		t.Error("expected context canceled error")
	}
}

func TestHTTPClientErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := a2a.NewJSONRPCError("req-1", -32600, "Invalid Request", nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 5*time.Second)
	req := a2a.NewJSONRPCRequest("test", nil)
	resp, err := client.Send(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Error("expected error in response")
	}
}
