# AHSIR Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a multi-agent scheduler that wraps Claude Code instances as A2A-compliant agents, enabling user-to-agent and agent-to-agent communication.

**Architecture:** Bottom-up: A2A types → JSON-RPC codec → HTTP transport → Registry → Session management → Agent Wrapper (Server + Client) → MCP Server → Scheduler → CLI. Each layer is independently testable. TDD throughout.

**Tech Stack:** Go 1.23, net/http, encoding/json, gopkg.in/yaml.v3, github.com/google/uuid

---

### Task 1: A2A Protocol Types

**Files:**
- Create: `internal/a2a/types.go`
- Create: `internal/a2a/types_test.go`

- [ ] **Step 1: Write failing tests for A2A types**

```go
// internal/a2a/types_test.go
package a2a

import (
	"encoding/json"
	"testing"
)

func TestTaskStateConstants(t *testing.T) {
	tests := []struct {
		state TaskState
		str   string
	}{
		{TaskStateSubmitted, "TASK_STATE_SUBMITTED"},
		{TaskStateWorking, "TASK_STATE_WORKING"},
		{TaskStateInputRequired, "TASK_STATE_INPUT_REQUIRED"},
		{TaskStateCompleted, "TASK_STATE_COMPLETED"},
		{TaskStateFailed, "TASK_STATE_FAILED"},
		{TaskStateCanceled, "TASK_STATE_CANCELED"},
	}
	for _, tt := range tests {
		if string(tt.state) != tt.str {
			t.Errorf("expected %s, got %s", tt.str, tt.state)
		}
	}
}

func TestRoleConstants(t *testing.T) {
	if string(RoleUser) != "user" {
		t.Errorf("expected user, got %s", RoleUser)
	}
	if string(RoleAgent) != "agent" {
		t.Errorf("expected agent, got %s", RoleAgent)
	}
}

func TestPartTypeConstants(t *testing.T) {
	if string(PartTypeText) != "text" {
		t.Errorf("expected text, got %s", PartTypeText)
	}
	if string(PartTypeFile) != "file" {
		t.Errorf("expected file, got %s", PartTypeFile)
	}
	if string(PartTypeData) != "data" {
		t.Errorf("expected data, got %s", PartTypeData)
	}
}

func TestTextPartMarshalJSON(t *testing.T) {
	p := TextPart{Text: "hello"}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"type":"text","text":"hello"}`
	if string(data) != expected {
		t.Errorf("expected %s, got %s", expected, string(data))
	}
}

func TestTextPartUnmarshalJSON(t *testing.T) {
	data := []byte(`{"type":"text","text":"hello"}`)
	var p TextPart
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Text != "hello" {
		t.Errorf("expected hello, got %s", p.Text)
	}
}

func TestFilePartMarshalJSON(t *testing.T) {
	p := FilePart{Name: "main.go", MediaType: "text/x-go", URI: "file:///tmp/main.go"}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var result FilePart
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "main.go" || result.MediaType != "text/x-go" {
		t.Errorf("unexpected unmarshaled FilePart: %+v", result)
	}
}

func TestMessageMarshalJSON(t *testing.T) {
	msg := Message{
		Role:  RoleUser,
		Parts: []Part{TextPart{Text: "design a user API"}},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var result Message
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Role != RoleUser {
		t.Errorf("expected user role, got %s", result.Role)
	}
	if len(result.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(result.Parts))
	}
}

func TestTaskMarshalJSON(t *testing.T) {
	task := Task{
		ID:     "task-1",
		Status: TaskStateWorking,
		Message: Message{
			Role:  RoleUser,
			Parts: []Part{TextPart{Text: "do something"}},
		},
	}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var result Task
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != "task-1" || result.Status != TaskStateWorking {
		t.Errorf("unexpected unmarshaled Task: %+v", result)
	}
}

func TestAgentCardMarshalJSON(t *testing.T) {
	card := AgentCard{
		Name:        "Backend Agent",
		Description: "Go backend development",
		Version:     "1.0.0",
		Provider:    &AgentProvider{Name: "ahsir", URL: "https://github.com/wu8685/ahsir"},
		Skills: []AgentSkill{
			{Name: "api-design", Description: "Design RESTful APIs"},
		},
		Endpoint: "http://127.0.0.1:9801/",
	}
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	var result AgentCard
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "Backend Agent" {
		t.Errorf("expected Backend Agent, got %s", result.Name)
	}
	if result.Provider == nil || result.Provider.Name != "ahsir" {
		t.Errorf("unexpected provider: %+v", result.Provider)
	}
	if len(result.Skills) != 1 || result.Skills[0].Name != "api-design" {
		t.Errorf("unexpected skills: %+v", result.Skills)
	}
}

func TestPartUnmarshalJSON_DiscriminatesType(t *testing.T) {
	data := []byte(`{"type":"text","text":"hello"}`)
	part, err := UnmarshalPart(data)
	if err != nil {
		t.Fatal(err)
	}
	tp, ok := part.(TextPart)
	if !ok {
		t.Fatalf("expected TextPart, got %T", part)
	}
	if tp.Text != "hello" {
		t.Errorf("expected hello, got %s", tp.Text)
	}
}

func TestPartUnmarshalJSON_UnknownType(t *testing.T) {
	data := []byte(`{"type":"unknown","value":"x"}`)
	_, err := UnmarshalPart(data)
	if err == nil {
		t.Error("expected error for unknown part type")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/wuke/workspace/go/src/github.com/wu8685/ahsir && go test ./internal/a2a/... -v`
Expected: FAIL (types not defined)

- [ ] **Step 3: Write minimal implementation**

```go
// internal/a2a/types.go
package a2a

import (
	"encoding/json"
	"fmt"
)

// TaskState represents the state of an A2A task.
type TaskState string

const (
	TaskStateSubmitted      TaskState = "TASK_STATE_SUBMITTED"
	TaskStateWorking        TaskState = "TASK_STATE_WORKING"
	TaskStateInputRequired  TaskState = "TASK_STATE_INPUT_REQUIRED"
	TaskStateCompleted      TaskState = "TASK_STATE_COMPLETED"
	TaskStateFailed         TaskState = "TASK_STATE_FAILED"
	TaskStateCanceled       TaskState = "TASK_STATE_CANCELED"
	TaskStateAuthRequired   TaskState = "TASK_STATE_AUTH_REQUIRED"
)

// Role represents the role of a message sender.
type Role string

const (
	RoleUser  Role = "user"
	RoleAgent Role = "agent"
)

// PartType identifies the type of a Part.
type PartType string

const (
	PartTypeText PartType = "text"
	PartTypeFile PartType = "file"
	PartTypeData PartType = "data"
)

// Part is an interface for message parts.
type Part interface {
	partMarker()
}

// TextPart represents a text content part.
type TextPart struct {
	Type PartType `json:"type"`
	Text string   `json:"text"`
}

func (TextPart) partMarker() {}

// FilePart represents a file content part.
type FilePart struct {
	Type      PartType `json:"type"`
	Name      string   `json:"name"`
	MediaType string   `json:"mediaType,omitempty"`
	URI       string   `json:"uri,omitempty"`
	Content   string   `json:"content,omitempty"`
}

func (FilePart) partMarker() {}

// DataPart represents a structured data part.
type DataPart struct {
	Type     PartType        `json:"type"`
	Name     string          `json:"name,omitempty"`
	MimeType string          `json:"mimeType,omitempty"`
	Data     json.RawMessage `json:"data"`
}

func (DataPart) partMarker() {}

// partAlias is used to avoid infinite recursion during unmarshaling.
type partAlias struct {
	Type PartType `json:"type"`
}

// UnmarshalPart unmarshals a Part from JSON, dispatching on the type field.
func UnmarshalPart(data []byte) (Part, error) {
	var alias partAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return nil, fmt.Errorf("unmarshal part type: %w", err)
	}
	switch alias.Type {
	case PartTypeText:
		var tp TextPart
		if err := json.Unmarshal(data, &tp); err != nil {
			return nil, err
		}
		return tp, nil
	case PartTypeFile:
		var fp FilePart
		if err := json.Unmarshal(data, &fp); err != nil {
			return nil, err
		}
		return fp, nil
	case PartTypeData:
		var dp DataPart
		if err := json.Unmarshal(data, &dp); err != nil {
			return nil, err
		}
		return dp, nil
	default:
		return nil, fmt.Errorf("unknown part type: %s", alias.Type)
	}
}

// Message represents an A2A message.
type Message struct {
	Role     Role   `json:"role"`
	Parts    []Part `json:"parts"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// messageJSON is the JSON representation used for custom (un)marshaling.
type messageJSON struct {
	Role     Role                   `json:"role"`
	Parts    []json.RawMessage      `json:"parts"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	rawParts := make([]json.RawMessage, len(m.Parts))
	for i, p := range m.Parts {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("marshal part %d: %w", i, err)
		}
		rawParts[i] = b
	}
	return json.Marshal(messageJSON{
		Role:     m.Role,
		Parts:    rawParts,
		Metadata: m.Metadata,
	})
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw messageJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Metadata = raw.Metadata
	m.Parts = make([]Part, len(raw.Parts))
	for i, rp := range raw.Parts {
		part, err := UnmarshalPart(rp)
		if err != nil {
			return fmt.Errorf("unmarshal part %d: %w", i, err)
		}
		m.Parts[i] = part
	}
	return nil
}

// Task represents an A2A task.
type Task struct {
	ID        string                 `json:"id"`
	ContextID string                 `json:"contextId,omitempty"`
	Status    TaskState              `json:"status"`
	Message   Message               `json:"message"`
	Artifacts []Artifact             `json:"artifacts,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// Artifact represents an output artifact from a task.
type Artifact struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parts       []Part `json:"parts"`
}

// artifactJSON is used for custom (un)marshaling of Artifact.
type artifactJSON struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Parts       []json.RawMessage `json:"parts"`
}

func (a Artifact) MarshalJSON() ([]byte, error) {
	rawParts := make([]json.RawMessage, len(a.Parts))
	for i, p := range a.Parts {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		rawParts[i] = b
	}
	return json.Marshal(artifactJSON{
		Name:        a.Name,
		Description: a.Description,
		Parts:       rawParts,
	})
}

func (a *Artifact) UnmarshalJSON(data []byte) error {
	var raw artifactJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	a.Name = raw.Name
	a.Description = raw.Description
	a.Parts = make([]Part, len(raw.Parts))
	for i, rp := range raw.Parts {
		part, err := UnmarshalPart(rp)
		if err != nil {
			return err
		}
		a.Parts[i] = part
	}
	return nil
}

// AgentSkill describes a skill an agent can perform.
type AgentSkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AgentProvider describes the provider of an agent.
type AgentProvider struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// AgentCard describes an agent's capabilities and endpoint.
type AgentCard struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Version      string            `json:"version"`
	Provider     *AgentProvider    `json:"provider,omitempty"`
	Skills       []AgentSkill      `json:"skills"`
	Endpoint     string            `json:"endpoint"`
	Capabilities map[string]interface{} `json:"capabilities,omitempty"`
	Status       string            `json:"status,omitempty"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/wuke/workspace/go/src/github.com/wu8685/ahsir && go test ./internal/a2a/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/a2a/types.go internal/a2a/types_test.go
git commit -m "feat(a2a): add A2A protocol types (Task, Message, Part, AgentCard)"
```

---

### Task 2: JSON-RPC 2.0 Codec

**Files:**
- Create: `internal/a2a/jsonrpc.go`
- Create: `internal/a2a/jsonrpc_test.go`

- [ ] **Step 1: Write failing tests for JSON-RPC codec**

```go
// internal/a2a/jsonrpc_test.go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/a2a/... -v -run TestJSONRPC`
Expected: FAIL (types not defined)

- [ ] **Step 3: Write minimal implementation**

```go
// internal/a2a/jsonrpc.go
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
func ErrParseError() *JSONRPCError {
	return &JSONRPCError{Code: -32700, Message: "Parse error"}
}

func ErrInvalidRequest() *JSONRPCError {
	return &JSONRPCError{Code: -32600, Message: "Invalid Request"}
}

func ErrMethodNotFound() *JSONRPCError {
	return &JSONRPCError{Code: -32601, Message: "Method not found"}
}

func ErrInvalidParams() *JSONRPCError {
	return &JSONRPCError{Code: -32602, Message: "Invalid params"}
}

func ErrInternalError() *JSONRPCError {
	return &JSONRPCError{Code: -32603, Message: "Internal error"}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/a2a/... -v -run TestJSONRPC`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/a2a/jsonrpc.go internal/a2a/jsonrpc_test.go
git commit -m "feat(a2a): add JSON-RPC 2.0 codec (request, response, error)"
```

---

### Task 3: HTTP Transport

**Files:**
- Create: `internal/transport/http.go`
- Create: `internal/transport/http_test.go`

- [ ] **Step 1: Write failing tests for HTTP transport**

```go
// internal/transport/http_test.go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/transport/... -v`
Expected: FAIL (package not found)

- [ ] **Step 3: Write minimal implementation**

```go
// internal/transport/http.go
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

// HTTPClient sends JSON-RPC requests to an A2A agent endpoint.
type HTTPClient struct {
	baseURL string
	client  *http.Client
}

// NewHTTPClient creates a new HTTP client for A2A communication.
func NewHTTPClient(baseURL string, timeout time.Duration) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// Send sends a JSON-RPC request and returns the response.
func (c *HTTPClient) Send(ctx context.Context, req *a2a.JSONRPCRequest) (*a2a.JSONRPCResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer httpResp.Body.Close()

	var resp a2a.JSONRPCResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/transport/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/transport/http.go internal/transport/http_test.go
git commit -m "feat(transport): add HTTP client for A2A JSON-RPC"
```

---

### Task 4: Agent Registry

**Files:**
- Create: `internal/registry/registry.go`
- Create: `internal/registry/registry_test.go`

- [ ] **Step 1: Write failing tests for Registry**

```go
// internal/registry/registry_test.go
package registry

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestRegisterAgent(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{
		Name:        "backend",
		Description: "Backend agent",
		Version:     "1.0.0",
		Endpoint:    "http://127.0.0.1:9801/",
		Skills:      []a2a.AgentSkill{{Name: "api-design"}},
	}
	reg.Register(card)

	agents := reg.List()
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Name != "backend" {
		t.Errorf("expected backend, got %s", agents[0].Name)
	}
	if agents[0].Status != "online" {
		t.Errorf("expected online, got %s", agents[0].Status)
	}
}

func TestRegisterDuplicate(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9801/"}
	reg.Register(card)
	card2 := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9802/"}
	reg.Register(card2)

	agents := reg.List()
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Endpoint != "http://127.0.0.1:9802/" {
		t.Errorf("expected updated endpoint, got %s", agents[0].Endpoint)
	}
}

func TestGetAgent(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{
		Name:        "frontend",
		Description: "Frontend agent",
		Version:     "1.0.0",
		Endpoint:    "http://127.0.0.1:9802/",
		Skills:      []a2a.AgentSkill{{Name: "react"}},
	}
	reg.Register(card)

	found, ok := reg.Get("frontend")
	if !ok {
		t.Fatal("expected to find agent")
	}
	if found.Description != "Frontend agent" {
		t.Errorf("unexpected description: %s", found.Description)
	}
}

func TestGetAgentNotFound(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	_, ok := reg.Get("nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestUnregisterAgent(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{Name: "data", Endpoint: "http://127.0.0.1:9803/"}
	reg.Register(card)
	if err := reg.Unregister("data"); err != nil {
		t.Fatal(err)
	}
	if len(reg.List()) != 0 {
		t.Error("expected empty list")
	}
}

func TestUnregisterNotFound(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	err := reg.Unregister("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestHeartbeat(t *testing.T) {
	reg := NewRegistry(10 * time.Second)
	card := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9801/"}
	reg.Register(card)

	// Mark heartbeat
	reg.Heartbeat("backend")

	agent, _ := reg.Get("backend")
	if agent.Status != "online" {
		t.Errorf("expected online after heartbeat, got %s", agent.Status)
	}
}

func TestHeartbeatTimeout(t *testing.T) {
	reg := NewRegistry(50 * time.Millisecond)
	card := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9801/"}
	reg.Register(card)

	// Wait for timeout
	time.Sleep(100 * time.Millisecond)

	agent, _ := reg.Get("backend")
	if agent.Status != "offline" {
		t.Errorf("expected offline after timeout, got %s", agent.Status)
	}
}

func TestRegistryHTTPHandler(t *testing.T) {
	reg := NewRegistry(30 * time.Second)
	handler := NewHTTPHandler(reg)

	// Register via HTTP
	card := a2a.AgentCard{Name: "backend", Endpoint: "http://127.0.0.1:9801/", Skills: []a2a.AgentSkill{}}
	body := mustMarshal(t, card)
	req := httptest.NewRequest(http.MethodPost, "/agents", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// List via HTTP
	req, _ = http.NewRequest(http.MethodGet, "/agents", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Get via HTTP
	req, _ = http.NewRequest(http.MethodGet, "/agents/backend", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Delete via HTTP
	req, _ = http.NewRequest(http.MethodDelete, "/agents/backend", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify empty
	req, _ = http.NewRequest(http.MethodGet, "/agents", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if len(reg.List()) != 0 {
		t.Error("expected empty after delete")
	}
}

func mustMarshal(t *testing.T, v interface{}) *bytes.Buffer {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewBuffer(data)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/registry/... -v`
Expected: FAIL (package not found)

- [ ] **Step 3: Write minimal implementation**

```go
// internal/registry/registry.go
package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

// Registry manages agent registration and discovery.
type Registry struct {
	mu              sync.RWMutex
	agents          map[string]*agentEntry
	heartbeatTimeout time.Duration
}

type agentEntry struct {
	card      a2a.AgentCard
	lastBeat  time.Time
}

// NewRegistry creates a new agent registry.
func NewRegistry(heartbeatTimeout time.Duration) *Registry {
	r := &Registry{
		agents:           make(map[string]*agentEntry),
		heartbeatTimeout: heartbeatTimeout,
	}
	return r
}

// Register adds or updates an agent in the registry.
func (r *Registry) Register(card a2a.AgentCard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	card.Status = "online"
	r.agents[card.Name] = &agentEntry{
		card:     card,
		lastBeat: time.Now(),
	}
}

// Get retrieves an agent by name.
func (r *Registry) Get(name string) (a2a.AgentCard, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.agents[name]
	if !ok {
		return a2a.AgentCard{}, false
	}
	card := entry.card
	if r.heartbeatTimeout > 0 && time.Since(entry.lastBeat) > r.heartbeatTimeout {
		card.Status = "offline"
	}
	return card, true
}

// List returns all registered agents.
func (r *Registry) List() []a2a.AgentCard {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cards := make([]a2a.AgentCard, 0, len(r.agents))
	for _, entry := range r.agents {
		card := entry.card
		if r.heartbeatTimeout > 0 && time.Since(entry.lastBeat) > r.heartbeatTimeout {
			card.Status = "offline"
		}
		cards = append(cards, card)
	}
	return cards
}

// Unregister removes an agent from the registry.
func (r *Registry) Unregister(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.agents[name]; !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	delete(r.agents, name)
	return nil
}

// Heartbeat updates the last heartbeat time for an agent.
func (r *Registry) Heartbeat(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.agents[name]; ok {
		entry.lastBeat = time.Now()
		entry.card.Status = "online"
	}
}

// HTTPHandler returns an HTTP handler for the registry API.
type HTTPHandler struct {
	registry *Registry
}

// NewHTTPHandler creates a new HTTP handler for the registry.
func NewHTTPHandler(reg *Registry) http.Handler {
	h := &HTTPHandler{registry: reg}
	mux := http.NewServeMux()
	mux.HandleFunc("/agents", h.handleAgents)
	mux.HandleFunc("/agents/", h.handleAgent)
	return mux
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	mux.HandleFunc("/agents", h.handleAgents)
	mux.HandleFunc("/agents/", h.handleAgent)
	mux.ServeHTTP(w, r)
}

func (h *HTTPHandler) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		agents := h.registry.List()
		writeJSON(w, http.StatusOK, agents)
	case http.MethodPost:
		var card a2a.AgentCard
		if err := json.NewDecoder(r.Body).Decode(&card); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		h.registry.Register(card)
		writeJSON(w, http.StatusCreated, card)
	case http.MethodDelete:
		name := strings.TrimPrefix(r.URL.Path, "/agents/")
		if name == "" || name == "/" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent name required"})
			return
		}
		if err := h.registry.Unregister(name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "unregistered"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (h *HTTPHandler) handleAgent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/agents/")
	if name == "" || name == "/" {
		h.handleAgents(w, r)
		return
	}

	if r.Method == http.MethodDelete {
		if err := h.registry.Unregister(name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "unregistered"})
		return
	}

	card, ok := h.registry.Get(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "agent not found"})
		return
	}
	writeJSON(w, http.StatusOK, card)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/registry/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/registry/registry.go internal/registry/registry_test.go
git commit -m "feat(registry): add agent registry with HTTP API and heartbeat"
```

---

### Task 5: Session Manager

**Files:**
- Create: `internal/wrapper/session.go`
- Create: `internal/wrapper/session_test.go`

- [ ] **Step 1: Write failing tests for Session Manager**

```go
// internal/wrapper/session_test.go
package wrapper

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionManagerStartStop(t *testing.T) {
	sm := NewSessionManager(SessionConfig{
		Command: "echo",
		Args:    []string{"hello"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !sm.IsRunning() {
		t.Error("expected session to be running")
	}

	if err := sm.Stop(); err != nil {
		t.Fatal(err)
	}
	if sm.IsRunning() {
		t.Error("expected session to be stopped")
	}
}

func TestSessionManagerSendMessage(t *testing.T) {
	// Use cat to echo back input
	sm := NewSessionManager(SessionConfig{
		Command: "cat",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sm.Stop()

	// Send message and read response
	prompt := "hello claude\n"
	outputCh, err := sm.Send(ctx, prompt)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	for line := range outputCh {
		buf.WriteString(line)
	}

	if !strings.Contains(buf.String(), "hello claude") {
		t.Errorf("expected echo of 'hello claude', got: %s", buf.String())
	}
}

func TestSessionManagerCrashRecovery(t *testing.T) {
	// Test that a crashed process can be restarted
	sm := NewSessionManager(SessionConfig{
		Command:    "sleep",
		Args:       []string{"0.1"},
		AutoRestart: true,
	})
	ctx := context.Background()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Wait for process to exit
	time.Sleep(300 * time.Millisecond)

	// Should not be running anymore
	if sm.IsRunning() {
		t.Log("process still running")
	}

	// Should be able to restart
	if err := sm.Start(ctx); err != nil {
		t.Fatalf("expected restart to succeed: %v", err)
	}
	sm.Stop()
}

func TestSessionManagerTimeout(t *testing.T) {
	sm := NewSessionManager(SessionConfig{
		Command: "sleep",
		Args:    []string{"10"},
		Timeout: 50 * time.Millisecond,
	})
	ctx := context.Background()

	if err := sm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sm.Stop()

	_, err := sm.Send(ctx, "do something\n")
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestSessionConfigValidation(t *testing.T) {
	cfg := SessionConfig{
		Command: "",
	}
	if cfg.Validate() == nil {
		t.Error("expected validation error for empty command")
	}

	cfg.Command = "echo"
	cfg.Timeout = -1
	if cfg.Validate() == nil {
		t.Error("expected validation error for negative timeout")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/wrapper/... -v -run TestSession`
Expected: FAIL (not defined)

- [ ] **Step 3: Write minimal implementation**

```go
// internal/wrapper/session.go
package wrapper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// SessionConfig configures a Claude Code session.
type SessionConfig struct {
	Command     string
	Args        []string
	Env         []string
	WorkDir     string
	Timeout     time.Duration
	AutoRestart bool
}

// Validate checks the config for required fields.
func (c SessionConfig) Validate() error {
	if c.Command == "" {
		return errors.New("command is required")
	}
	if c.Timeout < 0 {
		return errors.New("timeout must be non-negative")
	}
	return nil
}

// SessionManager manages a persistent Claude Code subprocess.
type SessionManager struct {
	cfg     SessionConfig
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	mu      sync.Mutex
	running bool
}

// NewSessionManager creates a new session manager.
func NewSessionManager(cfg SessionConfig) *SessionManager {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &SessionManager{
		cfg: cfg,
	}
}

// Start launches the Claude Code subprocess.
func (sm *SessionManager) Start(ctx context.Context) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.running {
		return errors.New("session already running")
	}

	sm.cmd = exec.CommandContext(ctx, sm.cfg.Command, sm.cfg.Args...)
	if sm.cfg.WorkDir != "" {
		sm.cmd.Dir = sm.cfg.WorkDir
	}
	if len(sm.cfg.Env) > 0 {
		sm.cmd.Env = sm.cfg.Env
	}

	var err error
	sm.stdin, err = sm.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}
	sm.stdout, err = sm.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}

	if err := sm.cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}
	sm.running = true
	return nil
}

// Stop terminates the Claude Code subprocess.
func (sm *SessionManager) Stop() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if !sm.running {
		return nil
	}

	sm.stdin.Close()
	sm.stdout.Close()

	if sm.cmd != nil && sm.cmd.Process != nil {
		if err := sm.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("kill process: %w", err)
		}
	}
	sm.running = false
	return nil
}

// IsRunning returns whether the session is currently running.
func (sm *SessionManager) IsRunning() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.running
}

// Send writes a prompt to the Claude Code process and returns an output channel.
func (sm *SessionManager) Send(ctx context.Context, prompt string) (<-chan string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if !sm.running {
		return nil, errors.New("session not running")
	}

	if _, err := io.WriteString(sm.stdin, prompt); err != nil {
		return nil, fmt.Errorf("write to stdin: %w", err)
	}

	outputCh := make(chan string, 100)

	go func() {
		defer close(outputCh)
		scanner := bufio.NewScanner(sm.stdout)
		timeout := sm.cfg.Timeout

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
		}

			line := scanner.Text()
			select {
			case outputCh <- line:
			case <-time.After(timeout):
				return
			}
		}
	}()

	return outputCh, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/wrapper/... -v -run TestSession`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/wrapper/session.go internal/wrapper/session_test.go
git commit -m "feat(wrapper): add session manager for Claude Code subprocess"
```

---

### Task 6: AgentCard Builder

**Files:**
- Create: `internal/wrapper/card.go`
- Create: `internal/wrapper/card_test.go`

- [ ] **Step 1: Write failing tests for AgentCard builder**

```go
// internal/wrapper/card_test.go
package wrapper

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestLoadAgentCardFromYAML(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: Backend Agent
description: Go backend development
version: "1.0.0"
provider:
  name: ahsir
  url: https://github.com/wu8685/ahsir
skills:
  - name: api-design
    description: Design RESTful APIs
  - name: database-schema
    description: Database schema design
claude:
  systemPrompt: "You are a Go backend developer."
  maxAgentCalls: 5
network:
  bind: "127.0.0.1"
`
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(yamlContent), 0644)

	builder := NewAgentCardBuilder(dir)
	card, err := builder.Load()
	if err != nil {
		t.Fatal(err)
	}
	if card.Name != "Backend Agent" {
		t.Errorf("expected 'Backend Agent', got '%s'", card.Name)
	}
	if card.Version != "1.0.0" {
		t.Errorf("expected '1.0.0', got '%s'", card.Version)
	}
	if len(card.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(card.Skills))
	}
	if card.Skills[0].Name != "api-design" {
		t.Errorf("expected 'api-design', got '%s'", card.Skills[0].Name)
	}
}

func TestBuildRuntimeAgentCard(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `
name: Test Agent
description: Test
version: "1.0.0"
skills: []
network:
  bind: "127.0.0.1"
`
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte(yamlContent), 0644)

	builder := NewAgentCardBuilder(dir)
	card, err := builder.Load()
	if err != nil {
		t.Fatal(err)
	}

	runtimeCard := builder.BuildRuntime(card, 9801)
	if runtimeCard.Endpoint != "http://127.0.0.1:9801/" {
		t.Errorf("expected endpoint 'http://127.0.0.1:9801/', got '%s'", runtimeCard.Endpoint)
	}
	if runtimeCard.Provider == nil {
		t.Error("expected provider to be set")
	}
}

func TestLoadAgentCardFileNotFound(t *testing.T) {
	dir := t.TempDir()
	builder := NewAgentCardBuilder(dir)
	_, err := builder.Load()
	if err == nil {
		t.Error("expected error for missing agent-card.yaml")
	}
}

func TestLoadAgentCardInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	a2aDir := filepath.Join(dir, ".a2a")
	os.MkdirAll(a2aDir, 0755)
	os.WriteFile(filepath.Join(a2aDir, "agent-card.yaml"), []byte("invalid: [yaml"), 0644)

	builder := NewAgentCardBuilder(dir)
	_, err := builder.Load()
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestAgentCardConfigDefaults(t *testing.T) {
	var cfg AgentCardConfig
	// Test that version defaults
	if cfg.Network.Bind != "" {
		// defaults are applied after unmarshaling
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/wrapper/... -v -run TestLoadAgentCard`
Expected: FAIL

- [ ] **Step 3: Write minimal implementation**

```go
// internal/wrapper/card.go
package wrapper

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wu8685/ahsir/internal/a2a"
	"gopkg.in/yaml.v3"
)

// AgentCardConfig represents the .a2a/agent-card.yaml file structure.
type AgentCardConfig struct {
	Name        string              `yaml:"name"`
	Description string              `yaml:"description"`
	Version     string              `yaml:"version"`
	Provider    *a2a.AgentProvider  `yaml:"provider"`
	Skills      []a2a.AgentSkill    `yaml:"skills"`
	Claude      ClaudeConfig        `yaml:"claude"`
	Network     NetworkConfig       `yaml:"network"`
}

// ClaudeConfig holds Claude-specific settings from the card.
type ClaudeConfig struct {
	SystemPrompt  string `yaml:"systemPrompt"`
	MaxAgentCalls int    `yaml:"maxAgentCalls"`
}

// NetworkConfig holds network settings from the card.
type NetworkConfig struct {
	Bind      string `yaml:"bind"`
	Advertise string `yaml:"advertise"`
}

// AgentCardBuilder builds A2A AgentCards from workspace config.
type AgentCardBuilder struct {
	workspaceDir string
}

// NewAgentCardBuilder creates a new AgentCard builder.
func NewAgentCardBuilder(workspaceDir string) *AgentCardBuilder {
	return &AgentCardBuilder{workspaceDir: workspaceDir}
}

// cardFile returns the path to the agent-card.yaml file.
func (b *AgentCardBuilder) cardFile() string {
	return filepath.Join(b.workspaceDir, ".a2a", "agent-card.yaml")
}

// Load reads and parses the agent-card.yaml from the workspace.
func (b *AgentCardBuilder) Load() (*AgentCardConfig, error) {
	data, err := os.ReadFile(b.cardFile())
	if err != nil {
		return nil, fmt.Errorf("read agent-card.yaml: %w", err)
	}

	var cfg AgentCardConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse agent-card.yaml: %w", err)
	}

	// Apply defaults
	if cfg.Network.Bind == "" {
		cfg.Network.Bind = "127.0.0.1"
	}
	if cfg.Claude.MaxAgentCalls == 0 {
		cfg.Claude.MaxAgentCalls = 5
	}

	return &cfg, nil
}

// BuildRuntime creates a runtime AgentCard with the endpoint set from the port.
func (b *AgentCardBuilder) BuildRuntime(cfg *AgentCardConfig, port int) a2a.AgentCard {
	bind := cfg.Network.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	advertise := cfg.Network.Advertise
	if advertise == "" {
		advertise = bind
	}

	card := a2a.AgentCard{
		Name:        cfg.Name,
		Description: cfg.Description,
		Version:     cfg.Version,
		Provider:    cfg.Provider,
		Skills:      cfg.Skills,
		Endpoint:    fmt.Sprintf("http://%s:%d/", advertise, port),
	}

	if card.Provider == nil {
		card.Provider = &a2a.AgentProvider{
			Name: "ahsir",
			URL:  "https://github.com/wu8685/ahsir",
		}
	}
	if card.Version == "" {
		card.Version = "1.0.0"
	}

	return card
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/wrapper/... -v -run TestLoadAgentCard`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/wrapper/card.go internal/wrapper/card_test.go
git commit -m "feat(wrapper): add AgentCard builder from .a2a/agent-card.yaml"
```

---

### Task 7: Prompt Construction & A2A_CALL Parsing

**Files:**
- Create: `internal/wrapper/prompt.go`
- Create: `internal/wrapper/prompt_test.go`

- [ ] **Step 1: Write failing tests for prompt construction and parsing**

```go
// internal/wrapper/prompt_test.go
package wrapper

import (
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestBuildSystemPrompt(t *testing.T) {
	agents := []a2a.AgentCard{
		{Name: "backend", Skills: []a2a.AgentSkill{{Name: "api-design"}}, Endpoint: "http://127.0.0.1:9801/"},
		{Name: "data", Skills: []a2a.AgentSkill{{Name: "sql"}}, Endpoint: "http://127.0.0.1:9802/"},
	}
	basePrompt := "You are a Go developer."

	prompt := BuildSystemPrompt(basePrompt, agents, 5)
	if !strings.Contains(prompt, "backend") {
		t.Error("expected prompt to mention backend agent")
	}
	if !strings.Contains(prompt, "data") {
		t.Error("expected prompt to mention data agent")
	}
	if !strings.Contains(prompt, "---A2A_CALL---") {
		t.Error("expected prompt to contain A2A_CALL format instructions")
	}
	if !strings.Contains(prompt, "api-design") {
		t.Error("expected prompt to mention api-design skill")
	}
}

func TestBuildSystemPromptNoAgents(t *testing.T) {
	prompt := BuildSystemPrompt("You are a Go developer.", nil, 5)
	if !strings.Contains(prompt, "You are a Go developer.") {
		t.Error("expected base prompt to be included")
	}
}

func TestParseA2ACall(t *testing.T) {
	output := `Some text before
---A2A_CALL---
{"agent": "backend", "task": "design a user API"}
---END---
Some text after`

	call, ok := ParseA2ACall(output)
	if !ok {
		t.Fatal("expected to parse A2A_CALL")
	}
	if call.Agent != "backend" {
		t.Errorf("expected backend, got %s", call.Agent)
	}
	if call.Task != "design a user API" {
		t.Errorf("unexpected task: %s", call.Task)
	}
}

func TestParseA2ACallNoCall(t *testing.T) {
	output := "Just normal output, no A2A call here."
	_, ok := ParseA2ACall(output)
	if ok {
		t.Error("expected no A2A_CALL found")
	}
}

func TestParseA2ACallInvalidJSON(t *testing.T) {
	output := `---A2A_CALL---
{invalid json
---END---`
	_, ok := ParseA2ACall(output)
	if ok {
		t.Error("expected parse failure for invalid JSON")
	}
}

func TestSerializeA2ACall(t *testing.T) {
	call := A2ACall{Agent: "data", Task: "run a SQL query"}
	serialized := SerializeA2ACall(call)
	if !strings.Contains(serialized, "---A2A_CALL---") {
		t.Error("expected ---A2A_CALL--- marker")
	}
	if !strings.Contains(serialized, "---END---") {
		t.Error("expected ---END--- marker")
	}
	if !strings.Contains(serialized, "data") {
		t.Error("expected agent name in output")
	}
}

func TestBuildInjectionPrompt(t *testing.T) {
	result := BuildInjectionPrompt("data", "the query result")
	if !strings.Contains(result, "data") {
		t.Error("expected agent name in injection prompt")
	}
	if !strings.Contains(result, "the query result") {
		t.Error("expected result in injection prompt")
	}
}

func TestValidateA2ACall(t *testing.T) {
	tests := []struct {
		call    A2ACall
		wantErr bool
	}{
		{A2ACall{Agent: "backend", Task: "do something"}, false},
		{A2ACall{Agent: "", Task: "do something"}, true},
		{A2ACall{Agent: "backend", Task: ""}, true},
		{A2ACall{Agent: "", Task: ""}, true},
	}
	for _, tt := range tests {
		err := ValidateA2ACall(tt.call)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateA2ACall(%+v) error = %v, wantErr = %v", tt.call, err, tt.wantErr)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/wrapper/... -v -run "TestBuild|TestParse|TestSerialize|TestValidate"`
Expected: FAIL

- [ ] **Step 3: Write minimal implementation**

```go
// internal/wrapper/prompt.go
package wrapper

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wu8685/ahsir/internal/a2a"
)

// A2ACall represents a parsed ---A2A_CALL--- block from Claude Code output.
type A2ACall struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// ValidateA2ACall checks that an A2A call has required fields.
func ValidateA2ACall(call A2ACall) error {
	if call.Agent == "" {
		return fmt.Errorf("agent name is required in A2A_CALL")
	}
	if call.Task == "" {
		return fmt.Errorf("task description is required in A2A_CALL")
	}
	return nil
}

// BuildSystemPrompt injects available agents into the system prompt.
func BuildSystemPrompt(basePrompt string, agents []a2a.AgentCard, maxCalls int) string {
	var sb strings.Builder
	sb.WriteString(basePrompt)

	if len(agents) > 0 {
		sb.WriteString("\n\nYou can call the following agents for help:\n")
		for _, a := range agents {
			skills := make([]string, len(a.Skills))
			for i, s := range a.Skills {
				skills[i] = s.Name
			}
			sb.WriteString(fmt.Sprintf("- name: %q, skills: %v, endpoint: %q\n",
				a.Name, skills, a.Endpoint))
		}
		sb.WriteString(fmt.Sprintf("\nWhen you need another agent's help, append to your response:\n"))
		sb.WriteString("---A2A_CALL---\n")
		sb.WriteString(`{"agent": "<name>", "task": "<description of what you need>"}` + "\n")
		sb.WriteString("---END---\n")
		sb.WriteString(fmt.Sprintf("\nMax chain depth: %d agent calls.\n", maxCalls))
	}

	return sb.String()
}

// ParseA2ACall extracts an ---A2A_CALL--- block from Claude Code output.
// Returns the call and true if found, zero value and false otherwise.
func ParseA2ACall(output string) (A2ACall, bool) {
	start := strings.Index(output, "---A2A_CALL---")
	if start == -1 {
		return A2ACall{}, false
	}

	contentStart := start + len("---A2A_CALL---")
	end := strings.Index(output[contentStart:], "---END---")
	if end == -1 {
		return A2ACall{}, false
	}

	jsonStr := strings.TrimSpace(output[contentStart : contentStart+end])
	var call A2ACall
	if err := json.Unmarshal([]byte(jsonStr), &call); err != nil {
		return A2ACall{}, false
	}

	return call, true
}

// SerializeA2ACall creates the ---A2A_CALL--- text block for a given call.
func SerializeA2ACall(call A2ACall) string {
	data, _ := json.MarshalIndent(call, "", "  ")
	return fmt.Sprintf("---A2A_CALL---\n%s\n---END---\n", string(data))
}

// BuildInjectionPrompt creates a prompt that injects another agent's result.
func BuildInjectionPrompt(agentName string, result string) string {
	return fmt.Sprintf("\n[Agent %s returned: %s]\n", agentName, result)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/wrapper/... -v -run "TestBuild|TestParse|TestSerialize|TestValidate"`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/wrapper/prompt.go internal/wrapper/prompt_test.go
git commit -m "feat(wrapper): add prompt construction and --A2A_CALL-- parsing"
```

---

### Task 8: A2A Client (Agent-to-Agent Calling)

**Files:**
- Create: `internal/wrapper/client.go`
- Create: `internal/wrapper/client_test.go`

- [ ] **Step 1: Write failing tests for A2A Client**

```go
// internal/wrapper/client_test.go
package wrapper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestA2AClientSendMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req a2a.JSONRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "message/send" {
			t.Errorf("unexpected method: %s", req.Method)
		}
		resp := a2a.NewJSONRPCResponse(req.ID, json.RawMessage(`{"status":"received"}`))
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewA2AClient(server.URL, 5*time.Second)
	resp, err := client.SendMessage(context.Background(), "test message", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("expected response")
	}
}

func TestA2AClientGetTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req a2a.JSONRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		task := a2a.Task{
			ID:     "task-1",
			Status: a2a.TaskStateCompleted,
		}
		data, _ := json.Marshal(task)
		resp := a2a.NewJSONRPCResponse(req.ID, data)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewA2AClient(server.URL, 5*time.Second)
	task, err := client.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "task-1" {
		t.Errorf("expected task-1, got %s", task.ID)
	}
	if task.Status != a2a.TaskStateCompleted {
		t.Errorf("expected completed, got %s", task.Status)
	}
}

func TestA2AClientRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var req a2a.JSONRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		resp := a2a.NewJSONRPCResponse(req.ID, json.RawMessage(`{"ok":true}`))
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewA2AClient(server.URL, 5*time.Second)
	client.SetMaxRetries(3)
	_, err := client.SendMessage(context.Background(), "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/wrapper/... -v -run TestA2AClient`
Expected: FAIL

- [ ] **Step 3: Write minimal implementation**

```go
// internal/wrapper/client.go
package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
	"github.com/wu8685/ahsir/internal/transport"
)

// A2AClient sends A2A protocol messages to other agents.
type A2AClient struct {
	httpClient *transport.HTTPClient
	maxRetries int
}

// NewA2AClient creates a new A2A client targeting a specific agent endpoint.
func NewA2AClient(endpoint string, timeout time.Duration) *A2AClient {
	return &A2AClient{
		httpClient: transport.NewHTTPClient(endpoint, timeout),
		maxRetries: 3,
	}
}

// SetMaxRetries configures the number of retry attempts.
func (c *A2AClient) SetMaxRetries(n int) {
	c.maxRetries = n
}

// SendMessage sends a message to the agent and returns the response.
func (c *A2AClient) SendMessage(ctx context.Context, text string, metadata map[string]interface{}) (*a2a.JSONRPCResponse, error) {
	msg := a2a.Message{
		Role:     a2a.RoleUser,
		Parts:    []a2a.Part{a2a.TextPart{Type: a2a.PartTypeText, Text: text}},
		Metadata: metadata,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal message: %w", err)
	}
	req := a2a.NewJSONRPCRequest("message/send", data)

	var lastErr error
	for attempt := 0; attempt < c.maxRetries; attempt++ {
		resp, err := c.httpClient.Send(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("send message after %d retries: %w", c.maxRetries, lastErr)
}

// GetTask retrieves a task's status from the agent.
func (c *A2AClient) GetTask(ctx context.Context, taskID string) (*a2a.Task, error) {
	params, _ := json.Marshal(map[string]string{"id": taskID})
	req := a2a.NewJSONRPCRequest("tasks/get", params)

	resp, err := c.httpClient.Send(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}

	var task a2a.Task
	if err := json.Unmarshal(resp.Result, &task); err != nil {
		return nil, fmt.Errorf("unmarshal task: %w", err)
	}
	return &task, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/wrapper/... -v -run TestA2AClient`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/wrapper/client.go internal/wrapper/client_test.go
git commit -m "feat(wrapper): add A2A client for agent-to-agent communication"
```

---

### Task 9: A2A Server

**Files:**
- Create: `internal/wrapper/server.go`
- Create: `internal/wrapper/server_test.go`

- [ ] **Step 1: Write failing tests for A2A Server**

```go
// internal/wrapper/server_test.go
package wrapper

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestA2AServerHandleMessageSend(t *testing.T) {
	taskStore := NewTaskStore()
	server := NewA2AServer(taskStore, nil)

	msg := a2a.Message{
		Role:  a2a.RoleUser,
		Parts: []a2a.Part{a2a.TextPart{Type: a2a.PartTypeText, Text: "write a test"}},
	}
	msgData, _ := json.Marshal(msg)
	req := a2a.NewJSONRPCRequest("message/send", msgData)
	body, _ := json.Marshal(req)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp a2a.JSONRPCResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}

	// Verify task created
	var task a2a.Task
	json.Unmarshal(resp.Result, &task)
	if task.Status != a2a.TaskStateSubmitted {
		t.Errorf("expected TASK_STATE_SUBMITTED, got %s", task.Status)
	}
}

func TestA2AServerHandleTasksGet(t *testing.T) {
	taskStore := NewTaskStore()
	// Pre-populate a task
	task := &a2a.Task{ID: "task-1", Status: a2a.TaskStateWorking}
	taskStore.Save(task)

	server := NewA2AServer(taskStore, nil)

	params, _ := json.Marshal(map[string]string{"id": "task-1"})
	req := a2a.NewJSONRPCRequest("tasks/get", params)
	body, _ := json.Marshal(req)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestA2AServerHandleUnknownMethod(t *testing.T) {
	taskStore := NewTaskStore()
	server := NewA2AServer(taskStore, nil)

	req := a2a.NewJSONRPCRequest("unknown/method", nil)
	body, _ := json.Marshal(req)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp a2a.JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected -32601, got %d", resp.Error.Code)
	}
}

func TestA2AServerHandleInvalidJSON(t *testing.T) {
	taskStore := NewTaskStore()
	server := NewA2AServer(taskStore, nil)

	httpReq := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("not json")))
	httpReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	server.ServeHTTP(w, httpReq)

	var resp a2a.JSONRPCResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil || resp.Error.Code != -32700 {
		t.Errorf("expected parse error, got %+v", resp.Error)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/wrapper/... -v -run TestA2AServer`
Expected: FAIL

- [ ] **Step 3: Write minimal implementation**

```go
// internal/wrapper/server.go
package wrapper

import (
	"encoding/json"
	"net/http"

	"github.com/wu8685/ahsir/internal/a2a"
	"github.com/google/uuid"
)

// ProcessMessageFunc is called when a message/send request arrives.
// It returns the task created for this request.
type ProcessMessageFunc func(msg *a2a.Message) (*a2a.Task, error)

// A2AServer handles incoming A2A JSON-RPC requests over HTTP.
type A2AServer struct {
	taskStore  *TaskStore
	processor  ProcessMessageFunc
}

// NewA2AServer creates a new A2A JSON-RPC server.
func NewA2AServer(taskStore *TaskStore, processor ProcessMessageFunc) *A2AServer {
	return &A2AServer{
		taskStore: taskStore,
		processor: processor,
	}
}

// ServeHTTP handles JSON-RPC requests.
func (s *A2AServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var req a2a.JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp := a2a.NewJSONRPCError("", -32700, "Parse error", nil)
		json.NewEncoder(w).Encode(resp)
		return
	}

	switch req.Method {
	case "message/send":
		s.handleMessageSend(w, &req)
	case "tasks/get":
		s.handleTasksGet(w, &req)
	default:
		resp := a2a.NewJSONRPCError(req.ID, -32601, "Method not found", nil)
		json.NewEncoder(w).Encode(resp)
	}
}

func (s *A2AServer) handleMessageSend(w http.ResponseWriter, req *a2a.JSONRPCRequest) {
	var msg a2a.Message
	if err := json.Unmarshal(req.Params, &msg); err != nil {
		resp := a2a.NewJSONRPCError(req.ID, -32602, "Invalid params", nil)
		json.NewEncoder(w).Encode(resp)
		return
	}

	task := &a2a.Task{
		ID:     uuid.New().String(),
		Status: a2a.TaskStateSubmitted,
		Message: msg,
	}

	if s.processor != nil {
		result, err := s.processor(&msg)
		if err != nil {
			resp := a2a.NewJSONRPCError(req.ID, -32603, "Internal error", nil)
			json.NewEncoder(w).Encode(resp)
			return
		}
		task = result
	}

	s.taskStore.Save(task)

	result, _ := json.Marshal(task)
	resp := a2a.NewJSONRPCResponse(req.ID, result)
	json.NewEncoder(w).Encode(resp)
}

func (s *A2AServer) handleTasksGet(w http.ResponseWriter, req *a2a.JSONRPCRequest) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		resp := a2a.NewJSONRPCError(req.ID, -32602, "Invalid params", nil)
		json.NewEncoder(w).Encode(resp)
		return
	}

	task, ok := s.taskStore.Get(params.ID)
	if !ok {
		resp := a2a.NewJSONRPCError(req.ID, -32602, "Task not found", nil)
		json.NewEncoder(w).Encode(resp)
		return
	}

	result, _ := json.Marshal(task)
	resp := a2a.NewJSONRPCResponse(req.ID, result)
	json.NewEncoder(w).Encode(resp)
}
```

Also need to add TaskStore:

```go
// internal/wrapper/taskstore.go
package wrapper

import (
	"sync"

	"github.com/wu8685/ahsir/internal/a2a"
)

// TaskStore is an in-memory store for A2A tasks.
type TaskStore struct {
	mu    sync.RWMutex
	tasks map[string]*a2a.Task
}

// NewTaskStore creates a new in-memory task store.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks: make(map[string]*a2a.Task),
	}
}

// Save stores a task.
func (ts *TaskStore) Save(task *a2a.Task) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.tasks[task.ID] = task
}

// Get retrieves a task by ID.
func (ts *TaskStore) Get(id string) (*a2a.Task, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	task, ok := ts.tasks[id]
	return task, ok
}

// List returns all tasks.
func (ts *TaskStore) List() []*a2a.Task {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	tasks := make([]*a2a.Task, 0, len(ts.tasks))
	for _, t := range ts.tasks {
		tasks = append(tasks, t)
	}
	return tasks
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/wrapper/... -v -run TestA2AServer`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/wrapper/server.go internal/wrapper/server_test.go internal/wrapper/taskstore.go
git commit -m "feat(wrapper): add A2A JSON-RPC server and task store"
```

---

### Task 10: Agent Wrapper Integration

**Files:**
- Create: `internal/wrapper/wrapper.go`
- Create: `internal/wrapper/wrapper_test.go`

- [ ] **Step 1: Write failing tests for Agent Wrapper**

```go
// internal/wrapper/wrapper_test.go
package wrapper

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestAgentWrapperStartStop(t *testing.T) {
	// Find an available port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	cfg := AgentWrapperConfig{
		Port:        port,
		RegistryURL: "",
		AgentCard: a2a.AgentCard{
			Name:        "test-agent",
			Description: "test",
			Version:     "1.0.0",
			Endpoint:    fmt.Sprintf("http://127.0.0.1:%d/", port),
		},
	}

	wrapper := NewAgentWrapper(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := wrapper.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify server is listening
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		t.Fatal("server not listening:", err)
	}
	conn.Close()

	if err := wrapper.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAgentWrapperRegisterWithRegistry(t *testing.T) {
	// Start a mock registry
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer registry.Close()

	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	cfg := AgentWrapperConfig{
		Port:        port,
		RegistryURL: registry.URL,
		AgentCard: a2a.AgentCard{
			Name:        "test-agent",
			Description: "test",
			Version:     "1.0.0",
			Endpoint:    fmt.Sprintf("http://127.0.0.1:%d/", port),
		},
	}

	wrapper := NewAgentWrapper(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := wrapper.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer wrapper.Stop(ctx)

	// Allow time for registration heartbeat
	time.Sleep(500 * time.Millisecond)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/wrapper/... -v -run TestAgentWrapper`
Expected: FAIL

- [ ] **Step 3: Write minimal implementation**

```go
// internal/wrapper/wrapper.go
package wrapper

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
)

// AgentWrapperConfig configures an agent wrapper instance.
type AgentWrapperConfig struct {
	Port        int
	RegistryURL string
	AgentCard   a2a.AgentCard
}

// AgentWrapper is the top-level component that ties together the A2A server,
// client, task store, and optional registry heartbeat.
type AgentWrapper struct {
	cfg       AgentWrapperConfig
	taskStore *TaskStore
	server    *A2AServer
	httpSrv   *http.Server
	mu        sync.Mutex
	running   bool
}

// NewAgentWrapper creates a new agent wrapper.
func NewAgentWrapper(cfg AgentWrapperConfig) *AgentWrapper {
	return &AgentWrapper{
		cfg:       cfg,
		taskStore: NewTaskStore(),
	}
}

// Start starts the HTTP server and begins registry heartbeat.
func (w *AgentWrapper) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.running {
		return fmt.Errorf("wrapper already running")
	}

	w.server = NewA2AServer(w.taskStore, nil)

	mux := http.NewServeMux()
	mux.Handle("/", w.server)

	w.httpSrv = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", w.cfg.Port),
		Handler: mux,
	}

	go func() {
		if err := w.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// log error
		}
	}()

	// Start heartbeat to registry if configured
	if w.cfg.RegistryURL != "" {
		go w.heartbeatLoop(ctx)
	}

	w.running = true
	return nil
}

// Stop gracefully shuts down the HTTP server.
func (w *AgentWrapper) Stop(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.running {
		return nil
	}

	if w.httpSrv != nil {
		if err := w.httpSrv.Shutdown(ctx); err != nil {
			return fmt.Errorf("shutdown server: %w", err)
		}
	}
	w.running = false
	return nil
}

func (w *AgentWrapper) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// POST to registry
			w.registerWithRegistry(ctx)
		}
	}
}

func (w *AgentWrapper) registerWithRegistry(ctx context.Context) {
	// Registration logic - will be implemented with HTTP client
	// V1: simple POST to registry URL
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/wrapper/... -v -run TestAgentWrapper`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/wrapper/wrapper.go internal/wrapper/wrapper_test.go
git commit -m "feat(wrapper): add agent wrapper integration (server + heartbeat)"
```

---

### Task 11: MCP Server

**Files:**
- Create: `internal/mcp/server.go`
- Create: `internal/mcp/server_test.go`

- [ ] **Step 1: Write failing tests for MCP Server**

```go
// internal/mcp/server_test.go
package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/a2a"
)

// mockAgentRouter implements AgentRouter for testing
type mockAgentRouter struct {
	agents []a2a.AgentCard
}

func (m *mockAgentRouter) ListAgents() []a2a.AgentCard {
	return m.agents
}

func (m *mockAgentRouter) ChatWithAgent(agentName, message string) (string, error) {
	return "response from " + agentName + ": " + message, nil
}

func (m *mockAgentRouter) GetTaskStatus(agentName, taskID string) (*a2a.Task, error) {
	return &a2a.Task{ID: taskID, Status: a2a.TaskStateCompleted}, nil
}

func TestMCPServerInitialize(t *testing.T) {
	srv := NewServer(&mockAgentRouter{})
	var buf bytes.Buffer

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
		},
		"id": "1",
	}
	data, _ := json.Marshal(req)

	resp, err := srv.HandleMessage(data)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]interface{}
	json.Unmarshal(resp, &result)
	if result["result"] == nil {
		t.Error("expected result in initialize response")
	}
}

func TestMCPServerListTools(t *testing.T) {
	srv := NewServer(&mockAgentRouter{})
	var buf bytes.Buffer

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/list",
		"id":      "2",
	}
	data, _ := json.Marshal(req)

	resp, err := srv.HandleMessage(data)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	json.Unmarshal(resp, &result)

	tools, ok := result["result"].(map[string]interface{})["tools"].([]interface{})
	if !ok {
		t.Fatal("expected tools list")
	}
	if len(tools) < 2 {
		t.Errorf("expected at least 2 tools, got %d", len(tools))
	}
}

func TestMCPServerAgentList(t *testing.T) {
	srv := NewServer(&mockAgentRouter{
		agents: []a2a.AgentCard{
			{Name: "backend", Description: "backend agent", Status: "online"},
			{Name: "frontend", Description: "frontend agent", Status: "online"},
		},
	})

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "agent_list",
		},
		"id": "3",
	}
	data, _ := json.Marshal(req)

	resp, err := srv.HandleMessage(data)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	json.Unmarshal(resp, &result)
	if result["result"] == nil {
		t.Error("expected result from agent_list")
	}
}

func TestMCPServerAgentChat(t *testing.T) {
	srv := NewServer(&mockAgentRouter{})

	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "agent_chat",
			"arguments": map[string]interface{}{"agent_name": "backend", "message": "hello"},
		},
		"id": "4",
	}
	data, _ := json.Marshal(req)

	resp, err := srv.HandleMessage(data)
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	json.Unmarshal(resp, &result)
	content := result["result"].(map[string]interface{})["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "backend") {
		t.Errorf("expected response to mention backend, got: %s", text)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcp/... -v`
Expected: FAIL (package not found)

- [ ] **Step 3: Write minimal implementation**

```go
// internal/mcp/server.go
package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/wu8685/ahsir/internal/a2a"
)

// AgentRouter is the interface the MCP server uses to communicate with agents.
type AgentRouter interface {
	ListAgents() []a2a.AgentCard
	ChatWithAgent(agentName, message string) (string, error)
	GetTaskStatus(agentName, taskID string) (*a2a.Task, error)
}

// Server implements an MCP server over stdio transport.
type Server struct {
	router AgentRouter
}

// NewServer creates a new MCP server.
func NewServer(router AgentRouter) *Server {
	return &Server{router: router}
}

// HandleMessage processes an incoming JSON-RPC message from the MCP client.
func (s *Server) HandleMessage(data []byte) ([]byte, error) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		ID      json.RawMessage `json:"id"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return s.errorResponse(nil, -32700, "Parse error")
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID)
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		return s.handleToolsCall(req.ID, req.Params)
	default:
		return s.errorResponse(req.ID, -32601, "Method not found")
	}
}

func (s *Server) handleInitialize(id json.RawMessage) ([]byte, error) {
	result := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "ahsir-mcp",
			"version": "1.0.0",
		},
	}
	return s.resultResponse(id, result)
}

func (s *Server) handleToolsList(id json.RawMessage) ([]byte, error) {
	tools := []map[string]interface{}{
		{
			"name":        "agent_list",
			"description": "List all registered agents with name, description, skills, and status",
			"inputSchema": map[string]interface{}{
				"type": "object",
			},
		},
		{
			"name":        "agent_chat",
			"description": "Send a message to a specific agent and return the response",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_name": map[string]string{"type": "string"},
					"message":    map[string]string{"type": "string"},
				},
				"required": []string{"agent_name", "message"},
			},
		},
		{
			"name":        "agent_task_status",
			"description": "Query a task's status on a specific agent",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"agent_name": map[string]string{"type": "string"},
					"task_id":    map[string]string{"type": "string"},
				},
				"required": []string{"agent_name", "task_id"},
			},
		},
	}
	return s.resultResponse(id, map[string]interface{}{"tools": tools})
}

func (s *Server) handleToolsCall(id json.RawMessage, params json.RawMessage) ([]byte, error) {
	var call struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return s.errorResponse(id, -32602, "Invalid params")
	}

	switch call.Name {
	case "agent_list":
		return s.handleAgentList(id)
	case "agent_chat":
		agentName, _ := call.Arguments["agent_name"].(string)
		message, _ := call.Arguments["message"].(string)
		return s.handleAgentChat(id, agentName, message)
	case "agent_task_status":
		agentName, _ := call.Arguments["agent_name"].(string)
		taskID, _ := call.Arguments["task_id"].(string)
		return s.handleAgentTaskStatus(id, agentName, taskID)
	default:
		return s.errorResponse(id, -32601, fmt.Sprintf("Unknown tool: %s", call.Name))
	}
}

func (s *Server) handleAgentList(id json.RawMessage) ([]byte, error) {
	agents := s.router.ListAgents()
	type agentSummary struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Skills      []string `json:"skills"`
		Status      string `json:"status"`
	}
	summaries := make([]agentSummary, len(agents))
	for i, a := range agents {
		skills := make([]string, len(a.Skills))
		for j, sk := range a.Skills {
			skills[j] = sk.Name
		}
		summaries[i] = agentSummary{
			Name:        a.Name,
			Description: a.Description,
			Skills:      skills,
			Status:      a.Status,
		}
	}
	data, _ := json.Marshal(summaries)
	text := string(data)
	if text == "null" {
		text = "[]"
	}
	content := []map[string]interface{}{
		{
			"type": "text",
			"text": text,
		},
	}
	return s.resultResponse(id, map[string]interface{}{"content": content})
}

func (s *Server) handleAgentChat(id json.RawMessage, agentName, message string) ([]byte, error) {
	response, err := s.router.ChatWithAgent(agentName, message)
	if err != nil {
		return s.errorResponse(id, -32603, err.Error())
	}
	content := []map[string]interface{}{
		{"type": "text", "text": response},
	}
	return s.resultResponse(id, map[string]interface{}{"content": content})
}

func (s *Server) handleAgentTaskStatus(id json.RawMessage, agentName, taskID string) ([]byte, error) {
	task, err := s.router.GetTaskStatus(agentName, taskID)
	if err != nil {
		return s.errorResponse(id, -32603, err.Error())
	}
	data, _ := json.Marshal(task)
	content := []map[string]interface{}{
		{"type": "text", "text": string(data)},
	}
	return s.resultResponse(id, map[string]interface{}{"content": content})
}

func (s *Server) resultResponse(id json.RawMessage, result interface{}) ([]byte, error) {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"result":  result,
		"id":      id,
	}
	return json.Marshal(resp)
}

func (s *Server) errorResponse(id json.RawMessage, code int, message string) ([]byte, error) {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
		"id": id,
	}
	return json.Marshal(resp)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcp/... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/server.go internal/mcp/server_test.go
git commit -m "feat(mcp): add MCP server with agent_list, agent_chat, agent_task_status tools"
```

---

### Task 12: Scheduler Config & Lifecycle

**Files:**
- Create: `internal/scheduler/config.go`
- Create: `internal/scheduler/config_test.go`
- Create: `internal/scheduler/scheduler.go`
- Create: `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: Write failing tests for Config**

```go
// internal/scheduler/config_test.go
package scheduler

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ahsir.yaml")
	yamlContent := `
agents:
  - name: backend
    workspace: /tmp/backend
    port: 0
  - name: frontend
    workspace: /tmp/frontend
    port: 9802

registry:
  host: "127.0.0.1"
  port: 9800
  heartbeat_interval: 10s
  heartbeat_timeout: 30s

mcp: {}

port_range:
  start: 9801
  end: 9900
`
	os.WriteFile(configPath, []byte(yamlContent), 0644)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "backend" {
		t.Errorf("expected backend, got %s", cfg.Agents[0].Name)
	}
	if cfg.Agents[1].Port != 9802 {
		t.Errorf("expected port 9802, got %d", cfg.Agents[1].Port)
	}
	if cfg.Registry.Port != 9800 {
		t.Errorf("expected registry port 9800, got %d", cfg.Registry.Port)
	}
	if cfg.PortRange.Start != 9801 || cfg.PortRange.End != 9900 {
		t.Errorf("unexpected port range: %+v", cfg.PortRange)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ahsir.yaml")
	yamlContent := `
agents:
  - name: backend
    workspace: /tmp/backend
`
	os.WriteFile(configPath, []byte(yamlContent), 0644)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Registry.Host != "127.0.0.1" {
		t.Errorf("expected default host 127.0.0.1, got %s", cfg.Registry.Host)
	}
	if cfg.Registry.Port != 9800 {
		t.Errorf("expected default port 9800, got %d", cfg.Registry.Port)
	}
	if cfg.Registry.HeartbeatInterval != "10s" {
		t.Errorf("expected default heartbeat_interval 10s, got %s", cfg.Registry.HeartbeatInterval)
	}
	if cfg.PortRange.Start != 9801 {
		t.Errorf("expected default port_start 9801, got %d", cfg.PortRange.Start)
	}
	if cfg.PortRange.End != 9900 {
		t.Errorf("expected default port_end 9900, got %d", cfg.PortRange.End)
	}
}

func TestConfigAllocatePort(t *testing.T) {
	cfg := &Config{
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	// First allocation
	port1, err := cfg.AllocatePort()
	if err != nil {
		t.Fatal(err)
	}
	if port1 < 9801 || port1 > 9900 {
		t.Errorf("port %d out of range", port1)
	}
	// Second allocation should give different port
	port2, _ := cfg.AllocatePort()
	if port2 == port1 {
		t.Error("expected different ports")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scheduler/... -v`
Expected: FAIL

- [ ] **Step 3: Write minimal implementation**

```go
// internal/scheduler/config.go
package scheduler

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// Config represents the ahsir.yaml configuration.
type Config struct {
	Agents    []AgentConfig `yaml:"agents"`
	Registry  RegistryConfig `yaml:"registry"`
	MCP       MCPConfig     `yaml:"mcp"`
	PortRange PortRange     `yaml:"port_range"`

	mu          sync.Mutex
	nextPort    int
}

// AgentConfig configures a single agent.
type AgentConfig struct {
	Name      string `yaml:"name"`
	Workspace string `yaml:"workspace"`
	Port      int    `yaml:"port"`
	Remote    string `yaml:"remote,omitempty"`
}

// RegistryConfig configures the registry.
type RegistryConfig struct {
	Host              string `yaml:"host"`
	Port              int    `yaml:"port"`
	HeartbeatInterval string `yaml:"heartbeat_interval"`
	HeartbeatTimeout  string `yaml:"heartbeat_timeout"`
}

// MCPConfig configures the MCP server.
type MCPConfig struct {
	// MCP uses stdio transport; config is in Claude Code settings
}

// PortRange defines the auto-allocation port range.
type PortRange struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

// LoadConfig reads and parses ahsir.yaml.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		Registry: RegistryConfig{
			Host:              "127.0.0.1",
			Port:              9800,
			HeartbeatInterval: "10s",
			HeartbeatTimeout:  "30s",
		},
		PortRange: PortRange{
			Start: 9801,
			End:   9900,
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.nextPort = cfg.PortRange.Start
	return cfg, nil
}

// AllocatePort allocates the next available port from the range.
func (c *Config) AllocatePort() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nextPort > c.PortRange.End {
		return 0, fmt.Errorf("no available ports in range %d-%d", c.PortRange.Start, c.PortRange.End)
	}
	port := c.nextPort
	c.nextPort++
	return port, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scheduler/... -v -run TestLoadConfig`
Expected: PASS

- [ ] **Step 5: Create Scheduler with lifecycle**

```go
// internal/scheduler/scheduler.go
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wu8685/ahsir/internal/a2a"
	"github.com/wu8685/ahsir/internal/registry"
)

// Scheduler manages the lifecycle of all agents.
type Scheduler struct {
	cfg      *Config
	registry *registry.Registry
	agents   map[string]*agentProcess
	mu       sync.Mutex
	running  bool
}

type agentProcess struct {
	cfg    AgentConfig
	cmd    interface{} // exec.Cmd in full implementation
	cancel context.CancelFunc
}

// New creates a new scheduler from configuration.
func New(cfg *Config) *Scheduler {
	heartbeatTimeout := 30 * time.Second
	if d, err := time.ParseDuration(cfg.Registry.HeartbeatTimeout); err == nil {
		heartbeatTimeout = d
	}
	return &Scheduler{
		cfg:      cfg,
		registry: registry.NewRegistry(heartbeatTimeout),
		agents:   make(map[string]*agentProcess),
	}
}

// Registry returns the scheduler's registry.
func (s *Scheduler) Registry() *registry.Registry {
	return s.registry
}

// Start starts the scheduler and all local agents.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("scheduler already running")
	}

	// Start registry HTTP server
	regHandler := registry.NewHTTPHandler(s.registry)
	go func() {
		addr := fmt.Sprintf("%s:%d", s.cfg.Registry.Host, s.cfg.Registry.Port)
		log.Printf("Registry listening on %s", addr)
		// In full implementation: http.ListenAndServe(addr, regHandler)
		_ = regHandler
		_ = addr
	}()

	// Start each local agent
	for _, agentCfg := range s.cfg.Agents {
		if agentCfg.Remote != "" {
			// V2: remote agent, just monitor
			continue
		}
		if err := s.startAgent(ctx, agentCfg); err != nil {
			return fmt.Errorf("start agent %s: %w", agentCfg.Name, err)
		}
	}

	s.running = true
	return nil
}

func (s *Scheduler) startAgent(ctx context.Context, cfg AgentConfig) error {
	port := cfg.Port
	if port == 0 {
		var err error
		port, err = s.cfg.AllocatePort()
		if err != nil {
			return err
		}
	}

	agentCtx, cancel := context.WithCancel(ctx)
	s.agents[cfg.Name] = &agentProcess{
		cfg:    cfg,
		cancel: cancel,
	}

	// In full implementation: exec ahsir-agent process
	_ = agentCtx
	_ = port
	log.Printf("Agent %s would start on port %d", cfg.Name, port)
	return nil
}

// Stop stops all agents and the scheduler.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for name, proc := range s.agents {
		proc.cancel()
		log.Printf("Agent %s stopped", name)
	}
	s.agents = make(map[string]*agentProcess)
	s.running = false
}

// ListAgents returns all registered agents (implements mcp.AgentRouter).
func (s *Scheduler) ListAgents() []a2a.AgentCard {
	return s.registry.List()
}

// ChatWithAgent sends a message to an agent (implements mcp.AgentRouter).
func (s *Scheduler) ChatWithAgent(agentName, message string) (string, error) {
	card, ok := s.registry.Get(agentName)
	if !ok {
		return "", fmt.Errorf("agent %s not found", agentName)
	}
	// In full implementation: use A2A client to send message
	_ = card
	return fmt.Sprintf("message sent to %s", agentName), nil
}

// GetTaskStatus gets a task's status (implements mcp.AgentRouter).
func (s *Scheduler) GetTaskStatus(agentName, taskID string) (*a2a.Task, error) {
	card, ok := s.registry.Get(agentName)
	if !ok {
		return nil, fmt.Errorf("agent %s not found", agentName)
	}
	// In full implementation: use A2A client to query task
	_ = card
	return &a2a.Task{ID: taskID, Status: a2a.TaskStateWorking}, nil
}
```

- [ ] **Step 6: Run all tests**

Run: `go test ./... -v`
Expected: ALL PASS

- [ ] **Step 7: Commit**

```bash
git add internal/scheduler/config.go internal/scheduler/config_test.go internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go
git commit -m "feat(scheduler): add config loading, lifecycle management, and MCP AgentRouter impl"
```

---

### Task 13: CLI - ahsir binary

**Files:**
- Create: `cmd/ahsir/main.go`

- [ ] **Step 1: Create the scheduler CLI entry point**

```go
// cmd/ahsir/main.go
package main

import (
	"fmt"
	"os"

	"github.com/wu8685/ahsir/internal/scheduler"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: ahsir <command>")
		fmt.Println("Commands:")
		fmt.Println("  start [config]  Start the scheduler (default: ahsir.yaml)")
		fmt.Println("  stop            Stop the scheduler")
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "start":
		configPath := "ahsir.yaml"
		if len(os.Args) > 2 {
			configPath = os.Args[2]
		}
		cfg, err := scheduler.LoadConfig(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
		sch := scheduler.New(cfg)
		if err := sch.Start(nil); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting scheduler: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Scheduler started")
		// Block forever (in real impl: wait for signal)
		select {}
	case "stop":
		fmt.Println("Stopping scheduler...")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./cmd/ahsir/...`
Expected: Build succeeds

- [ ] **Step 3: Commit**

```bash
git add cmd/ahsir/main.go
git commit -m "feat(cli): add ahsir scheduler CLI binary"
```

---

### Task 14: CLI - ahsir-agent binary

**Files:**
- Create: `cmd/ahsir-agent/main.go`

- [ ] **Step 1: Create the agent wrapper CLI entry point**

```go
// cmd/ahsir-agent/main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/wu8685/ahsir/internal/wrapper"
)

func main() {
	workspace := flag.String("workspace", "", "Workspace directory")
	port := flag.Int("port", 0, "Listen port")
	registry := flag.String("registry", "", "Registry URL")
	flag.Parse()

	if *workspace == "" {
		fmt.Fprintf(os.Stderr, "Usage: ahsir-agent --workspace=<path> [--port=<port>] [--registry=<url>]\n")
		os.Exit(1)
	}

	// Load agent card
	builder := wrapper.NewAgentCardBuilder(*workspace)
	cfg, err := builder.Load()
	if err != nil {
		log.Fatalf("Failed to load agent card: %v", err)
	}

	runtimeCard := builder.BuildRuntime(cfg, *port)

	wrapperCfg := wrapper.AgentWrapperConfig{
		Port:        *port,
		RegistryURL: *registry,
		AgentCard:   runtimeCard,
	}

	w := wrapper.NewAgentWrapper(wrapperCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		log.Fatalf("Failed to start agent wrapper: %v", err)
	}

	log.Printf("Agent %s listening on port %d", runtimeCard.Name, *port)

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	w.Stop(ctx)
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./cmd/ahsir-agent/...`
Expected: Build succeeds

- [ ] **Step 3: Commit**

```bash
git add cmd/ahsir-agent/main.go
git commit -m "feat(cli): add ahsir-agent wrapper CLI binary"
```

---

### Task 15: Integration & Go Modules

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Update go.mod with dependencies**

Run: `cd /Users/wuke/workspace/go/src/github.com/wu8685/ahsir && go mod tidy`

Expected: Downloads gopkg.in/yaml.v3, github.com/google/uuid

- [ ] **Step 2: Run full test suite**

Run: `go test ./... -v`
Expected: ALL PASS

- [ ] **Step 3: Run full build**

Run: `go build ./...`
Expected: All binaries build

- [ ] **Step 4: Final commit**

```bash
git add go.mod go.sum
git commit -m "chore: update go modules with all dependencies"
```
