package wrapper

import (
	"context"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-go/a2a"
)

// ExecutorConfig configures the agent execution loop.
type ExecutorConfig struct {
	SendPrompt func(ctx context.Context, prompt string) (<-chan string, error)
	ListAgents func() []*a2a.AgentCard
	CallAgent  func(ctx context.Context, agentName string, task string) (string, error)
	MaxDepth   int
	BasePrompt string
}

// Executor runs the core agent loop: receive message → prompt Claude Code →
// parse A2A_CALL markers → execute sub-agent calls → inject results → return task.
type Executor struct {
	sendPrompt func(ctx context.Context, prompt string) (<-chan string, error)
	listAgents func() []*a2a.AgentCard
	callAgent  func(ctx context.Context, agentName string, task string) (string, error)
	maxDepth   int
	basePrompt string
}

// NewExecutor creates a new Executor.
// If MaxDepth is not set (0), it defaults to 5.
func NewExecutor(cfg ExecutorConfig) *Executor {
	return &Executor{
		sendPrompt: cfg.SendPrompt,
		listAgents: cfg.ListAgents,
		callAgent:  cfg.CallAgent,
		maxDepth:   cfg.MaxDepth,
		basePrompt: cfg.BasePrompt,
	}
}

// Execute runs the agent execution loop for an incoming message.
func (e *Executor) Execute(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
	task := a2a.NewSubmittedTask(a2a.TaskInfo{}, msg)

	// Build the full system prompt with available agents
	agents := e.listAgents()
	systemPrompt := BuildSystemPrompt(e.basePrompt, agents, e.maxDepth)

	// Extract user text
	userText := messageText(msg)

	// Compose initial prompt
	fullPrompt := systemPrompt + "\n\n" + userText + "\n"

	// Run the agent interaction loop
	resultText, history, err := e.interact(ctx, task, fullPrompt, 0, userText)
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		return task, err
	}

	task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	task.History = history
	_ = resultText

	return task, nil
}

// interact runs the recursive agent interaction loop.
func (e *Executor) interact(ctx context.Context, task *a2a.Task, prompt string, depth int, originalTask string) (string, []*a2a.Message, error) {
	outputCh, err := e.sendPrompt(ctx, prompt)
	if err != nil {
		return "", task.History, fmt.Errorf("send prompt: %w", err)
	}

	var output strings.Builder
	for line := range outputCh {
		output.WriteString(line)
		output.WriteString("\n")
	}

	responseText := output.String()

	// Record agent response in history
	respMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: responseText})
	task.History = append(task.History, respMsg)

	// Check for A2A_CALL markers
	call, ok := ParseA2ACall(responseText)
	if !ok {
		return responseText, task.History, nil
	}

	// If max depth reached, stop making sub-calls
	if depth >= e.maxDepth {
		return responseText, task.History, nil
	}

	if err := ValidateA2ACall(call); err != nil {
		// Invalid call, but continue processing
		errorMsg := fmt.Sprintf("\n[A2A_CALL error: %v]\n", err)
		return e.interact(ctx, task, errorMsg, depth, originalTask)
	}

	if e.callAgent == nil {
		errorMsg := fmt.Sprintf("\n[Cannot call agent %s: no agent caller configured]\n", call.Agent)
		return e.interact(ctx, task, errorMsg, depth, originalTask)
	}

	// Execute the sub-agent call
	agentResult, err := e.callAgent(ctx, call.Agent, call.Task)
	if err != nil {
		errorMsg := fmt.Sprintf("\n[Agent %s call failed: %v]\n", call.Agent, err)
		return e.interact(ctx, task, errorMsg, depth, originalTask)
	}

	// Inject the result and continue
	injection := BuildInjectionPrompt(call.Agent, originalTask, agentResult)
	return e.interact(ctx, task, injection, depth+1, originalTask)
}

// messageText extracts text content from a message's parts.
func messageText(msg *a2a.Message) string {
	for _, part := range msg.Parts {
		if tp, ok := part.(a2a.TextPart); ok {
			return tp.Text
		}
	}
	return ""
}
