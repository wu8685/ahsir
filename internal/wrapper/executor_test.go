package wrapper

import (
	"context"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
)

// mockMessageSender returns a fixed string per call (one entry from lineSets
// per Send), joined with newlines. lineSets is a queue; each Send call
// consumes the next set.
type mockMessageSender struct {
	lineSets [][]string
	callIdx  int
}

func (m *mockMessageSender) Send(ctx context.Context, prompt string) (string, error) {
	var lines []string
	if m.callIdx < len(m.lineSets) {
		lines = m.lineSets[m.callIdx]
		m.callIdx++
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// openSessionFromSender wraps a (ctx,prompt)→(string,error) sender into the
// OpenSession factory shape expected by ExecutorConfig. Tests use this to
// keep existing mock senders unchanged while exercising the Session interface.
func openSessionFromSender(sender func(ctx context.Context, prompt string) (string, error)) func(ctx context.Context, contextID string) (Session, error) {
	return func(ctx context.Context, contextID string) (Session, error) {
		return &OneshotSession{sender: sender}, nil
	}
}

func TestExecutorSimpleMessage(t *testing.T) {
	sender := &mockMessageSender{
		lineSets: [][]string{{"I'll help you with that.", "Here's the code:", "```go", "func main() {}", "```"}},
	}

	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSessionFromSender(sender.Send),
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
		OpenSession: openSessionFromSender(sender.Send),
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{
				{Name: "backend", URL: "http://127.0.0.1:9801/", Skills: []a2a.AgentSkill{{Name: "api-design"}}},
			}
		},
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
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

// TestExecutorPropagatesContextIDToDelegate is the regression test for the
// "agent-to-agent calls reset contextID" bug. The executor must thread its
// task.ContextID through to the sub-agent call so the callee's pool can
// reuse a session across multiple delegations from the same conversation.
//
// Concretely: when student gets a curl with contextID=X and delegates to
// teacher, teacher's pool must be keyed on X (not on empty / not on a
// newly-generated id). Otherwise teacher spawns a new claude process for
// every delegation, even within one conversation.
func TestExecutorPropagatesContextIDToDelegate(t *testing.T) {
	sender := &mockMessageSender{
		lineSets: [][]string{
			{
				"---A2A_CALL---",
				`{"agent": "backend", "task": "design API"}`,
				"---END---",
			},
			{"OK, done."},
		},
	}

	var capturedContextID string
	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSessionFromSender(sender.Send),
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{{Name: "backend", URL: "http://127.0.0.1:9801/"}}
		},
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
			capturedContextID = contextID
			return "done", nil
		},
		MaxDepth: 5,
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "build"})
	msg.ContextID = "outer-conv-xyz"

	if _, err := executor.Execute(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	if capturedContextID != "outer-conv-xyz" {
		t.Errorf("sub-agent contextID: got %q want outer-conv-xyz", capturedContextID)
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
		OpenSession: openSessionFromSender(sender.Send),
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{
				{Name: "backend", URL: "http://127.0.0.1:9801/"},
			}
		},
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
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
		OpenSession: openSessionFromSender(sender.Send),
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

// TestExecutorPropagatesContextID verifies that the contextID on the incoming
// message is carried onto the resulting task — the linchpin for cross-request
// memory.
func TestExecutorPropagatesContextID(t *testing.T) {
	sender := &mockMessageSender{lineSets: [][]string{{"ok"}}}
	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSessionFromSender(sender.Send),
		ListAgents: func() []*a2a.AgentCard { return nil },
		MaxDepth:   3,
		BasePrompt: "you are a helper",
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hi"})
	msg.ContextID = "ctx-fixed"

	task, err := executor.Execute(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if task.ContextID != "ctx-fixed" {
		t.Errorf("expected task.ContextID=ctx-fixed, got %q", task.ContextID)
	}
}

// TestExecutorOmitsPriorHistoryFromPrompt verifies that the wrapper no
// longer prepends a "Conversation so far" block to the prompt — claude
// itself maintains conversation history across turns of the same Session.
func TestExecutorOmitsPriorHistoryFromPrompt(t *testing.T) {
	var capturedPrompt string
	sender := func(ctx context.Context, prompt string) (string, error) {
		capturedPrompt = prompt
		return "ok\n", nil
	}
	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSessionFromSender(sender),
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    3,
		BasePrompt:  "you are a helper",
	})
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "and a channel?"})
	msg.ContextID = "ctx-1"

	if _, err := executor.Execute(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capturedPrompt, "Conversation so far") {
		t.Errorf("prompt must not contain history header, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "and a channel?") {
		t.Errorf("prompt missing current user turn:\n%s", capturedPrompt)
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
		OpenSession: openSessionFromSender(sender.Send),
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
