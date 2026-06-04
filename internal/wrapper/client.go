package wrapper

import (
	"context"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
)

// AgentClient wraps the SDK's a2aclient.Client for agent-to-agent communication.
type AgentClient struct {
	client *a2aclient.Client
	card   *a2a.AgentCard
}

// noFieldTimeoutHTTPClient is the http.Client we hand to the A2A SDK. We
// deliberately leave Timeout = 0 (no field-level deadline) so the *context*
// timeout passed to SendMessage / GetTask is the single source of truth.
//
// Why this matters: the SDK's NewJSONRPCTransport defaults to
// `&http.Client{Timeout: 3*time.Minute}` if no client is supplied. That
// field-level timeout is independent of any context deadline — whichever
// fires first wins. So even though the scheduler hands a 10-minute context
// to ChatWithAgent, the http.Client would silently terminate the request at
// 3 minutes. Empty-string Timeout disables the ceiling and lets the context
// (set by the caller, e.g. scheduler.ChatWithAgent) be authoritative.
//
// SDK requests use http.NewRequestWithContext, so context cancellation
// already propagates correctly through the transport.
var noFieldTimeoutHTTPClient = &http.Client{}

// NewAgentClient creates a client for communicating with a target agent.
func NewAgentClient(ctx context.Context, card *a2a.AgentCard) (*AgentClient, error) {
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(noFieldTimeoutHTTPClient),
	)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", card.Name, err)
	}
	return &AgentClient{client: client, card: card}, nil
}

// SendMessage sends a text message to the agent. contextID, when non-empty,
// is set on the outgoing A2A Message so the callee's SessionPool can route
// the request to an existing session for that contextID. Empty contextID
// means "no conversation continuity" — callee will auto-generate one and
// each call starts a fresh session.
func (c *AgentClient) SendMessage(ctx context.Context, contextID, text string) (string, error) {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: text})
	if contextID != "" {
		msg.ContextID = contextID
	}
	params := &a2a.MessageSendParams{Message: msg}
	result, err := c.client.SendMessage(ctx, params)
	if err != nil {
		return "", fmt.Errorf("send message to %s: %w", c.card.Name, err)
	}

	switch r := result.(type) {
	case *a2a.Message:
		return messageToString(r), nil
	case *a2a.Task:
		return taskToString(r), nil
	default:
		return fmt.Sprintf("%v", result), nil
	}
}

// GetTask retrieves a task's status from the agent.
func (c *AgentClient) GetTask(ctx context.Context, taskID string) (*a2a.Task, error) {
	params := &a2a.TaskQueryParams{ID: a2a.TaskID(taskID)}
	task, err := c.client.GetTask(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("get task %s: %w", taskID, err)
	}
	return task, nil
}

func messageToString(msg *a2a.Message) string {
	for _, part := range msg.Parts {
		if tp, ok := part.(a2a.TextPart); ok {
			return tp.Text
		}
	}
	return ""
}

func taskToString(task *a2a.Task) string {
	// Return the last agent message in history
	for i := len(task.History) - 1; i >= 0; i-- {
		msg := task.History[i]
		if msg.Role == a2a.MessageRoleAgent {
			txt := messageToString(msg)
			if txt != "" {
				return txt
			}
		}
	}
	// Fallback to any non-empty message
	for _, msg := range task.History {
		txt := messageToString(msg)
		if txt != "" {
			return txt
		}
	}
	return string(task.Status.State)
}
