package wrapper

import (
	"context"
	"fmt"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
)

// AgentClient wraps the SDK's a2aclient.Client for agent-to-agent communication.
type AgentClient struct {
	client *a2aclient.Client
	card   *a2a.AgentCard
}

// NewAgentClient creates a client for communicating with a target agent.
func NewAgentClient(ctx context.Context, card *a2a.AgentCard) (*AgentClient, error) {
	client, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", card.Name, err)
	}
	return &AgentClient{client: client, card: card}, nil
}

// SendMessage sends a text message to the agent.
func (c *AgentClient) SendMessage(ctx context.Context, text string) (string, error) {
	params := &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: text}),
	}
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
