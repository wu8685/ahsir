package wrapper

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

// mockMessageSender returns a channel that yields predefined lines.
// lineSets is a queue of line sets; each Send call consumes the next set.
type mockMessageSender struct {
	lineSets [][]string
	callIdx  int
	delay    time.Duration
}

func (m *mockMessageSender) Send(ctx context.Context, prompt string) (<-chan string, error) {
	var lines []string
	if m.callIdx < len(m.lineSets) {
		lines = m.lineSets[m.callIdx]
		m.callIdx++
	}
	ch := make(chan string, len(lines))
	go func() {
		defer close(ch)
		for _, line := range lines {
			if m.delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(m.delay):
				}
			}
			select {
			case ch <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func TestExecutorSimpleMessage(t *testing.T) {
	sender := &mockMessageSender{
		lineSets: [][]string{{"I'll help you with that.", "Here's the code:", "```go", "func main() {}", "```"}},
	}

	executor := NewExecutor(ExecutorConfig{
		SendPrompt:  sender.Send,
		ListAgents:  func() []*a2a.AgentCard { return nil },
		CallAgent:   nil,
		MaxDepth:    5,
		BasePrompt:  "You are a helpful assistant.",
	})

	ctx := context.Background()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "write a main function"})

	task, err := executor.Execute(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil {
		t.Fatal("expected non-nil task")
	}
	if len(task.History) == 0 {
		t.Fatal("expected non-empty history")
	}

	// Last message should contain the response
	lastMsg := task.History[len(task.History)-1]
	found := false
	for _, part := range lastMsg.Parts {
		if tp, ok := part.(a2a.TextPart); ok && strings.Contains(tp.Text, "func main()") {
			found = true
		}
	}
	if !found {
		t.Error("expected response to contain 'func main()'")
	}
}

func TestExecutorWithA2ACall(t *testing.T) {
	sender := &mockMessageSender{
		lineSets: [][]string{
			{
				"I need help from the backend agent.",
				"---A2A_CALL---",
				`{"agent": "backend", "task": "design a user API"}`,
				"---END---",
			},
			{
				"Got the API design. Now I can complete the task.",
				"Here's the final solution.",
			},
		},
	}

	callRecorded := false
	var calledAgent string
	var calledTask string

	executor := NewExecutor(ExecutorConfig{
		SendPrompt: sender.Send,
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{
				{Name: "backend", URL: "http://127.0.0.1:9801/", Skills: []a2a.AgentSkill{{Name: "api-design"}}},
			}
		},
		CallAgent: func(ctx context.Context, agentName, task string) (string, error) {
			callRecorded = true
			calledAgent = agentName
			calledTask = task
			return "API designed successfully", nil
		},
		MaxDepth:    5,
		BasePrompt:  "You are a Go developer.",
	})

	ctx := context.Background()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "build a full system"})

	task, err := executor.Execute(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}

	if !callRecorded {
		t.Error("expected CallAgent to be called for A2A_CALL")
	}
	if calledAgent != "backend" {
		t.Errorf("expected agent 'backend', got '%s'", calledAgent)
	}
	if calledTask != "design a user API" {
		t.Errorf("expected task 'design a user API', got '%s'", calledTask)
	}
	if task == nil {
		t.Fatal("expected non-nil task")
	}
}

func TestExecutorMaxDepthExceeded(t *testing.T) {
	callCount := 0
	sender := &mockMessageSender{
		lineSets: [][]string{
			{
				"---A2A_CALL---",
				`{"agent": "backend", "task": "do something"}`,
				"---END---",
			},
			{
				"---A2A_CALL---",
				`{"agent": "backend", "task": "do something else"}`,
				"---END---",
			},
		},
	}

	executor := NewExecutor(ExecutorConfig{
		SendPrompt: sender.Send,
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{
				{Name: "backend", URL: "http://127.0.0.1:9801/"},
			}
		},
		CallAgent: func(ctx context.Context, agentName, task string) (string, error) {
			callCount++
			return "result", nil
		},
		MaxDepth:    0,
		BasePrompt:  "You are a helper.",
	})

	ctx := context.Background()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "test"})

	_, err := executor.Execute(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}

	if callCount > 0 {
		t.Errorf("expected no calls (depth exceeded), got %d calls", callCount)
	}
}

func TestExecutorNoA2ACallMarker(t *testing.T) {
	sender := &mockMessageSender{
		lineSets: [][]string{{"Here is the complete solution:", "All done."}},
	}

	executor := NewExecutor(ExecutorConfig{
		SendPrompt:  sender.Send,
		ListAgents:  func() []*a2a.AgentCard { return nil },
		CallAgent:   nil,
		MaxDepth:    5,
		BasePrompt:  "You are a helper.",
	})

	ctx := context.Background()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "simple task"})

	task, err := executor.Execute(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}

	// Verify history contains both user message and response
	if len(task.History) < 2 {
		t.Fatalf("expected at least 2 history entries, got %d", len(task.History))
	}
}

func TestExecutorInvalidA2ACallJSON(t *testing.T) {
	sender := &mockMessageSender{
		lineSets: [][]string{{
			"---A2A_CALL---",
			`{invalid json`,
			"---END---",
			"I'll continue on my own then.",
		}},
	}

	executor := NewExecutor(ExecutorConfig{
		SendPrompt: sender.Send,
		ListAgents: func() []*a2a.AgentCard { return nil },
		CallAgent:  nil,
		MaxDepth:   5,
		BasePrompt: "You are a helper.",
	})

	ctx := context.Background()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "test"})

	task, err := executor.Execute(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil {
		t.Fatal("expected non-nil task even with invalid A2A_CALL JSON")
	}
}
