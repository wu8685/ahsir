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
		Parts:    []a2a.Part{a2a.TextPart{Text: text}},
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
