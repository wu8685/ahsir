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

// fakeStreamSession implements Session for ExecuteStream tests. Each call to
// Stream() emits the queued deltas, then the canonical EventText, then
// EventTurnDone. Multi-turn tests (e.g. A2A_CALL recursion) drive a slice of
// turn payloads.
type fakeStreamSession struct {
	turns   [][]string // per turn: delta chunks
	finals  []string   // per turn: final aggregated EventText
	idx     int
	healthy bool
}

func newFakeStreamSession(turns [][]string, finals []string) *fakeStreamSession {
	return &fakeStreamSession{turns: turns, finals: finals, healthy: true}
}

func (f *fakeStreamSession) Stream(ctx context.Context, _ string) (<-chan Event, error) {
	ch := make(chan Event, 16)
	turn := f.idx
	f.idx++
	go func() {
		defer close(ch)
		if turn < len(f.turns) {
			for _, d := range f.turns[turn] {
				ch <- EventTextDelta{Text: d}
			}
		}
		if turn < len(f.finals) {
			ch <- EventText{Text: f.finals[turn]}
		}
		ch <- EventTurnDone{}
	}()
	return ch, nil
}

func (f *fakeStreamSession) Turn(ctx context.Context, userText string) (string, error) {
	ch, err := f.Stream(ctx, userText)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for ev := range ch {
		if t, ok := ev.(EventText); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String(), nil
}

func (f *fakeStreamSession) SessionID() string { return "" }
func (f *fakeStreamSession) IsHealthy() bool   { return f.healthy }
func (f *fakeStreamSession) Close() error      { f.healthy = false; return nil }

// TestExecutor_ExecuteStream_YieldsDeltas verifies the streaming executor
// emits one TaskStatusUpdateEvent per delta then a final *a2a.Task with the
// completed state. The final task's history must carry the canonical full
// response — same as the non-streaming Execute path — so consumers that drop
// the deltas still see a usable answer.
func TestExecutor_ExecuteStream_YieldsDeltas(t *testing.T) {
	fake := newFakeStreamSession(
		[][]string{{"Hel", "lo ", "world"}},
		[]string{"Hello world"},
	)
	openSession := func(ctx context.Context, contextID string) (Session, error) {
		return fake, nil
	}

	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSession,
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    5,
		BasePrompt:  "You are a helper.",
	})

	ctx := context.Background()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "say hi"})

	var deltas []string
	var finalTask *a2a.Task
	var sawWorkingAnnouncement bool
	for ev, err := range executor.ExecuteStream(ctx, msg) {
		if err != nil {
			t.Fatalf("yield err: %v", err)
		}
		switch e := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			if e.Status.Message == nil {
				if e.Status.State == a2a.TaskStateWorking {
					sawWorkingAnnouncement = true
				}
				continue
			}
			for _, p := range e.Status.Message.Parts {
				if tp, ok := p.(a2a.TextPart); ok {
					deltas = append(deltas, tp.Text)
				}
			}
		case *a2a.Task:
			finalTask = e
		}
	}

	if !sawWorkingAnnouncement {
		t.Error("want initial Working status update without message")
	}
	wantDeltas := []string{"Hel", "lo ", "world"}
	if !equalStreamStrings(deltas, wantDeltas) {
		t.Errorf("want deltas %v, got %v", wantDeltas, deltas)
	}
	if finalTask == nil {
		t.Fatal("ExecuteStream did not yield a final *a2a.Task")
	}
	if finalTask.Status.State != a2a.TaskStateCompleted {
		t.Errorf("want final state completed, got %s", finalTask.Status.State)
	}
	if len(finalTask.History) == 0 {
		t.Fatal("final task history empty")
	}
	last := finalTask.History[len(finalTask.History)-1]
	if tp, ok := last.Parts[0].(a2a.TextPart); !ok || tp.Text != "Hello world" {
		t.Errorf("want last history part 'Hello world', got %+v", last.Parts[0])
	}
}

// TestExecutor_ExecuteStream_A2ACallRecurses ensures that an A2A_CALL marker
// in the first turn dispatches to callAgent, then resumes streaming for the
// second turn. Deltas from both turns are visible in order.
func TestExecutor_ExecuteStream_A2ACallRecurses(t *testing.T) {
	firstResponse := strings.Join([]string{
		"checking with helper.",
		"---A2A_CALL---",
		`{"agent": "helper", "task": "what is 2+2?"}`,
		"---END---",
	}, "\n") + "\n"
	fake := newFakeStreamSession(
		[][]string{{"check", "ing..."}, {"final"}},
		[]string{firstResponse, "final answer"},
	)
	openSession := func(ctx context.Context, contextID string) (Session, error) {
		return fake, nil
	}
	var calledAgent, calledTask string
	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSession,
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{{Name: "helper"}}
		},
		CallAgent: func(ctx context.Context, name, ctxID, task string) (string, error) {
			calledAgent = name
			calledTask = task
			return "2+2=4", nil
		},
		MaxDepth:   5,
		BasePrompt: "You are a router.",
	})

	ctx := context.Background()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "compute 2+2"})

	var finalTask *a2a.Task
	for ev, err := range executor.ExecuteStream(ctx, msg) {
		if err != nil {
			t.Fatalf("yield err: %v", err)
		}
		if t2, ok := ev.(*a2a.Task); ok {
			finalTask = t2
		}
	}

	if calledAgent != "helper" {
		t.Errorf("want sub-agent 'helper', got %q", calledAgent)
	}
	if !strings.Contains(calledTask, "2+2") {
		t.Errorf("want sub-task text containing '2+2', got %q", calledTask)
	}
	if finalTask == nil {
		t.Fatal("no final task yielded")
	}
	if finalTask.Status.State != a2a.TaskStateCompleted {
		t.Errorf("want completed, got %s", finalTask.Status.State)
	}
	// Two assistant history entries: first turn (the A2A_CALL message itself)
	// + second turn after sub-agent injection. The first message contains the
	// raw A2A_CALL block — that's the canonical record of what the LLM said.
	if len(finalTask.History) < 2 {
		t.Fatalf("want >=2 history entries after recursion, got %d", len(finalTask.History))
	}
}

func equalStreamStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
