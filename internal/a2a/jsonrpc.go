package a2a

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// JSONRPCRequest represents a JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      string          `json:"id,omitempty"`
}

// NewJSONRPCRequest creates a new JSON-RPC request with a generated ID.
func NewJSONRPCRequest(method string, params json.RawMessage) *JSONRPCRequest {
	return &JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
		ID:      uuid.New().String(),
	}
}

// NewJSONRPCNotification creates a JSON-RPC notification (no ID).
func NewJSONRPCNotification(method string, params json.RawMessage) *JSONRPCRequest {
	return &JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
}

// JSONRPCResponse represents a JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
	ID      string          `json:"id"`
}

// JSONRPCError represents a JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *JSONRPCError) Error() string {
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

// NewJSONRPCResponse creates a successful JSON-RPC response.
func NewJSONRPCResponse(id string, result json.RawMessage) *JSONRPCResponse {
	return &JSONRPCResponse{
		JSONRPC: "2.0",
		Result:  result,
		ID:      id,
	}
}

// NewJSONRPCError creates a JSON-RPC error response.
func NewJSONRPCError(id string, code int, message string, data json.RawMessage) *JSONRPCResponse {
	return &JSONRPCResponse{
		JSONRPC: "2.0",
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
		ID: id,
	}
}

// Standard JSON-RPC 2.0 error constructors.

// ErrParseError returns -32700 Parse error.
func ErrParseError() *JSONRPCError {
	return &JSONRPCError{Code: -32700, Message: "Parse error"}
}

// ErrInvalidRequest returns -32600 Invalid Request.
func ErrInvalidRequest() *JSONRPCError {
	return &JSONRPCError{Code: -32600, Message: "Invalid Request"}
}

// ErrMethodNotFound returns -32601 Method not found.
func ErrMethodNotFound() *JSONRPCError {
	return &JSONRPCError{Code: -32601, Message: "Method not found"}
}

// ErrInvalidParams returns -32602 Invalid params.
func ErrInvalidParams() *JSONRPCError {
	return &JSONRPCError{Code: -32602, Message: "Invalid params"}
}

// ErrInternalError returns -32603 Internal error.
func ErrInternalError() *JSONRPCError {
	return &JSONRPCError{Code: -32603, Message: "Internal error"}
}
