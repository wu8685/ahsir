package a2a

import (
	"encoding/json"
	"testing"
)

func TestNewJSONRPCRequest(t *testing.T) {
	params := json.RawMessage(`{"message":"hello"}`)
	req := NewJSONRPCRequest("message/send", params)
	if req.JSONRPC != "2.0" {
		t.Errorf("expected 2.0, got %s", req.JSONRPC)
	}
	if req.Method != "message/send" {
		t.Errorf("expected message/send, got %s", req.Method)
	}
	if req.ID == "" {
		t.Error("expected non-empty ID")
	}
}

func TestJSONRPCRequestMarshal(t *testing.T) {
	req := NewJSONRPCRequest("tasks/get", json.RawMessage(`{"id":"task-1"}`))
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var result JSONRPCRequest
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Method != "tasks/get" || result.JSONRPC != "2.0" {
		t.Errorf("unexpected request: %+v", result)
	}
}

func TestJSONRPCResponseSuccess(t *testing.T) {
	result := json.RawMessage(`{"status":"ok"}`)
	resp := NewJSONRPCResponse("req-1", result)
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var parsed JSONRPCResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ID != "req-1" || parsed.Error != nil {
		t.Errorf("unexpected response: %+v", parsed)
	}
}

func TestJSONRPCResponseError(t *testing.T) {
	resp := NewJSONRPCError("req-2", -32600, "Invalid Request", nil)
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var parsed JSONRPCResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ID != "req-2" || parsed.Error == nil {
		t.Errorf("expected error in response: %+v", parsed)
	}
	if parsed.Error.Code != -32600 || parsed.Error.Message != "Invalid Request" {
		t.Errorf("unexpected error: %+v", parsed.Error)
	}
}

func TestJSONRPCStandardErrors(t *testing.T) {
	tests := []struct {
		err  *JSONRPCError
		code int
	}{
		{ErrParseError(), -32700},
		{ErrInvalidRequest(), -32600},
		{ErrMethodNotFound(), -32601},
		{ErrInvalidParams(), -32602},
		{ErrInternalError(), -32603},
	}
	for _, tt := range tests {
		if tt.err.Code != tt.code {
			t.Errorf("expected code %d, got %d", tt.code, tt.err.Code)
		}
	}
}

func TestJSONRPCNotification(t *testing.T) {
	params := json.RawMessage(`{"event":"task_update"}`)
	notif := NewJSONRPCNotification("tasks/update", params)
	data, err := json.Marshal(notif)
	if err != nil {
		t.Fatal(err)
	}
	var parsed JSONRPCRequest
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ID != "" {
		t.Errorf("notification should have empty ID, got %s", parsed.ID)
	}
	if parsed.Method != "tasks/update" {
		t.Errorf("expected tasks/update, got %s", parsed.Method)
	}
}
