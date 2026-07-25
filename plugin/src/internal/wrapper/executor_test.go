package wrapper

import (
	"bytes"
	"context"
	"log"
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
		MaxDepth:   5,
		BasePrompt: "You are a Go developer.",
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

type fakeAgentCallSession struct {
	calls []EventAgentCall
	idx   int
}

func (f *fakeAgentCallSession) Stream(ctx context.Context, _ string) (<-chan Event, error) {
	ch := make(chan Event, 4)
	turn := f.idx
	f.idx++
	go func() {
		defer close(ch)
		if turn < len(f.calls) {
			ch <- f.calls[turn]
		}
		if turn == 0 {
			ch <- EventText{Text: "delegating via structured tool"}
		} else {
			ch <- EventText{Text: "final answer"}
		}
		ch <- EventTurnDone{}
	}()
	return ch, nil
}

func (f *fakeAgentCallSession) Turn(ctx context.Context, userText string) (string, error) {
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

func (f *fakeAgentCallSession) SessionID() string { return "" }
func (f *fakeAgentCallSession) IsHealthy() bool   { return true }
func (f *fakeAgentCallSession) Close() error      { return nil }

func captureLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	return &buf
}

func TestExecutor_PerformanceLogs(t *testing.T) {
	logs := captureLogOutput(t)
	fake := &fakeAgentCallSession{calls: []EventAgentCall{{Agent: "backend", Task: "design API"}}}

	executor := NewExecutor(ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (Session, error) { return fake, nil },
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{{Name: "backend", URL: "http://127.0.0.1:9801/"}}
		},
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
			return "backend result", nil
		},
		MaxDepth:   5,
		BasePrompt: "You are a router.",
		SelfName:   "student",
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "build API"})
	msg.ContextID = "perf-context"
	msg.ID = "perf-msg"
	if _, err := executor.Execute(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	out := logs.String()
	for _, want := range []string{
		"[student] executor start contextID=perf-context msgID=perf-msg mode=send",
		"[student] executor open_session done contextID=perf-context msgID=perf-msg took=",
		"[student] executor prompt_ready contextID=perf-context msgID=perf-msg agents=1",
		"[student] executor turn done contextID=perf-context depth=0 took=",
		"input_tokens=0 output_tokens=0 cost_usd=0.000000 provider_duration_ms=0",
		"[student → backend] A2A_CALL: contextID=perf-context depth=0 source=structured",
		"[student ← backend] reply: contextID=perf-context depth=0 took=",
		"[student] executor injection_ready contextID=perf-context depth=0 agent=backend",
		"[student] executor done contextID=perf-context msgID=perf-msg history=3 took=",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected performance log %q in:\n%s", want, out)
		}
	}
}

func TestExecutorWithStructuredAgentCall(t *testing.T) {
	fake := &fakeAgentCallSession{calls: []EventAgentCall{{Agent: "backend", Task: "design a user API"}}}
	var calledAgent, calledTask string
	executor := NewExecutor(ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (Session, error) { return fake, nil },
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{{Name: "backend", URL: "http://127.0.0.1:9801/"}}
		},
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
			calledAgent = agentName
			calledTask = task
			return "API designed successfully", nil
		},
		MaxDepth: 5,
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "build"})
	if _, err := executor.Execute(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if calledAgent != "backend" || calledTask != "design a user API" {
		t.Fatalf("unexpected delegate call: agent=%q task=%q", calledAgent, calledTask)
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
		MaxDepth:   0,
		BasePrompt: "You are a helper.",
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
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    3,
		BasePrompt:  "you are a helper",
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
		ListAgents:  func() []*a2a.AgentCard { return nil },
		CallAgent:   nil,
		MaxDepth:    5,
		BasePrompt:  "You are a helper.",
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

// TestExecutor_ExecuteStream_NoDeltasSetsTerminalStatusMessage reproduces
// issue #15: a delta-less runtime (only a final EventText, no EventTextDelta)
// must still surface its reply to a streaming consumer. Such consumers (the
// gateway's ChatStream) aggregate status-update deltas into a buffer and, when
// that buffer stays empty, fall back to the terminal task's Status.Message.
// The reply lives in History, so unless ExecuteStream mirrors it into
// Status.Message the aggregated reply is empty. This test drives a session
// that emits no deltas and asserts the terminal Status.Message carries the
// text — and that replaying the documented consumer fallback yields a
// non-empty reply.
func TestExecutor_ExecuteStream_NoDeltasSetsTerminalStatusMessage(t *testing.T) {
	const reply = "echo: say hi"
	// No delta chunks for the single turn, only the final EventText — exactly
	// what a delta-less runtime (the echo provider) emits.
	fake := newFakeStreamSession([][]string{{}}, []string{reply})
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

	// Mirror the consumer (ChatStream) contract: accumulate text parts from
	// status-update deltas; the terminal task supplies the fallback.
	var buf strings.Builder
	var deltaCount int
	var finalTask *a2a.Task
	for ev, err := range executor.ExecuteStream(ctx, msg) {
		if err != nil {
			t.Fatalf("yield err: %v", err)
		}
		switch e := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			if e.Status.Message == nil {
				continue
			}
			for _, p := range e.Status.Message.Parts {
				if tp, ok := p.(a2a.TextPart); ok {
					buf.WriteString(tp.Text)
					deltaCount++
				}
			}
		case *a2a.Task:
			finalTask = e
		}
	}

	if deltaCount != 0 {
		t.Fatalf("precondition: want no streamed deltas for a delta-less runtime, got %d", deltaCount)
	}
	if finalTask == nil {
		t.Fatal("ExecuteStream did not yield a final *a2a.Task")
	}
	if finalTask.Status.State != a2a.TaskStateCompleted {
		t.Errorf("want completed state, got %s", finalTask.Status.State)
	}

	// The core regression: the terminal task carries the reply in Status.Message
	// so the no-delta consumer fallback can find it.
	if finalTask.Status.Message == nil {
		t.Fatal("terminal Status.Message is nil; no-delta consumers see an empty reply (issue #15)")
	}
	var terminalText strings.Builder
	for _, p := range finalTask.Status.Message.Parts {
		if tp, ok := p.(a2a.TextPart); ok {
			terminalText.WriteString(tp.Text)
		}
	}
	if terminalText.String() != reply {
		t.Errorf("want terminal Status.Message text %q, got %q", reply, terminalText.String())
	}
	if finalTask.Status.Message.Role != a2a.MessageRoleAgent {
		t.Errorf("want terminal Status.Message role agent, got %s", finalTask.Status.Message.Role)
	}

	// Replay the documented consumer fallback: buf is empty, so adopt the
	// terminal Status.Message. The aggregated reply must be non-empty.
	if buf.Len() == 0 && finalTask.Status.Message != nil {
		for _, p := range finalTask.Status.Message.Parts {
			if tp, ok := p.(a2a.TextPart); ok {
				buf.WriteString(tp.Text)
			}
		}
	}
	if buf.String() != reply {
		t.Errorf("aggregated reply: want %q, got %q", reply, buf.String())
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

func TestExecutor_ExecuteStream_StructuredAgentCallRecurses(t *testing.T) {
	fake := &fakeAgentCallSession{calls: []EventAgentCall{{Agent: "backend", Task: "stream API"}}}
	var called bool
	executor := NewExecutor(ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (Session, error) { return fake, nil },
		ListAgents: func() []*a2a.AgentCard {
			return []*a2a.AgentCard{{Name: "backend", URL: "http://127.0.0.1:9801/"}}
		},
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
			called = true
			if agentName != "backend" || task != "stream API" {
				t.Fatalf("unexpected call: agent=%q task=%q", agentName, task)
			}
			return "stream result", nil
		},
		MaxDepth: 5,
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "build"})
	var finalTask *a2a.Task
	for ev, err := range executor.ExecuteStream(context.Background(), msg) {
		if err != nil {
			t.Fatalf("yield err: %v", err)
		}
		if task, ok := ev.(*a2a.Task); ok {
			finalTask = task
		}
	}
	if !called {
		t.Fatal("expected structured agent call to recurse")
	}
	if finalTask == nil || finalTask.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("expected completed final task, got %+v", finalTask)
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

// failingCallSender always emits the same A2A_CALL — simulating a model
// stuck retrying a delegation. Used to pin the attempt-budget contract.
type failingCallSender struct {
	calls int
}

func (m *failingCallSender) Send(ctx context.Context, prompt string) (string, error) {
	m.calls++
	return "trying again\n---A2A_CALL---\n{\"agent\": \"backend\", \"task\": \"do it\"}\n---END---\n", nil
}

// TestExecutorFailedCallsConsumeDepthBudget verifies that failure rounds
// (sub-call errors) advance the depth budget: a model that keeps emitting a
// call whose execution always fails must terminate within MaxDepth+1 LLM
// turns instead of recursing at constant depth until the transport timeout.
func TestExecutorFailedCallsConsumeDepthBudget(t *testing.T) {
	const maxDepth = 3
	sender := &failingCallSender{}
	callAttempts := 0

	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSessionFromSender(sender.Send),
		ListAgents:  func() []*a2a.AgentCard { return []*a2a.AgentCard{{Name: "backend"}} },
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
			callAttempts++
			return "", context.DeadlineExceeded // every sub-call fails
		},
		MaxDepth:   maxDepth,
		BasePrompt: "base",
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "go"})
	task, err := executor.Execute(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("want completed task after budget exhaustion, got %v", task.Status.State)
	}
	// Depth 0..maxDepth-1 may attempt a sub-call; the turn at depth==maxDepth
	// stops before calling. So: maxDepth call attempts, maxDepth+1 LLM turns.
	if callAttempts != maxDepth {
		t.Errorf("want %d sub-call attempts, got %d", maxDepth, callAttempts)
	}
	if sender.calls != maxDepth+1 {
		t.Errorf("want %d LLM turns, got %d", maxDepth+1, sender.calls)
	}
}

// TestExecutorInvalidCallsConsumeDepthBudget is the same contract for calls
// that fail validation (e.g. naming an unknown agent shape) — the invalid-
// call feedback loop must also be bounded.
func TestExecutorInvalidCallsConsumeDepthBudget(t *testing.T) {
	const maxDepth = 3
	turns := 0
	// Always emits an A2A_CALL with an empty agent name → ValidateA2ACall fails.
	sender := func(ctx context.Context, prompt string) (string, error) {
		turns++
		return "retry\n---A2A_CALL---\n{\"agent\": \"\", \"task\": \"x\"}\n---END---\n", nil
	}

	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSessionFromSender(sender),
		ListAgents:  func() []*a2a.AgentCard { return nil },
		CallAgent: func(ctx context.Context, agentName, contextID, task string) (string, error) {
			t.Fatal("CallAgent must not run for an invalid call")
			return "", nil
		},
		MaxDepth:   maxDepth,
		BasePrompt: "base",
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "go"})
	if _, err := executor.Execute(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if turns != maxDepth+1 {
		t.Errorf("want %d LLM turns before budget exhaustion, got %d", maxDepth+1, turns)
	}
}

// --- speaker prompt injection (specs/2026-06-08-shared-context-collaboration.md) ---

// TestExecutorInjectsSpeakerIntoPrompt: when the message carries
// Metadata[MetadataSpeakerKey], the provider prompt must tag the user text
// with the speaker and carry the multi-speaker instruction so the model
// attributes statements to the right person.
func TestExecutorInjectsSpeakerIntoPrompt(t *testing.T) {
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
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "what is my favorite color?"})
	msg.ContextID = "ctx-speaker"
	msg.Metadata = map[string]any{MetadataSpeakerKey: "alice"}

	if _, err := executor.Execute(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedPrompt, "[speaker: alice] what is my favorite color?") {
		t.Errorf("prompt missing speaker tag before user text:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "multiple people") {
		t.Errorf("prompt missing multi-speaker instruction:\n%s", capturedPrompt)
	}
}

// TestExecutorWithoutSpeakerPromptByteIdentical pins backward compatibility:
// absent speaker metadata must produce exactly today's prompt — no tag, no
// instruction line, not even a changed byte.
func TestExecutorWithoutSpeakerPromptByteIdentical(t *testing.T) {
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
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "plain question"})
	msg.ContextID = "ctx-plain"

	if _, err := executor.Execute(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	expected := BuildSystemPrompt("you are a helper", nil, 3) + "\n\n" + "plain question" + "\n"
	if capturedPrompt != expected {
		t.Errorf("prompt not byte-identical without speaker.\nwant:\n%q\ngot:\n%q", expected, capturedPrompt)
	}
}

// promptCaptureStreamSession wraps fakeStreamSession to record the prompts
// handed to Stream — the streaming path must inject the speaker identically
// to the blocking path.
type promptCaptureStreamSession struct {
	*fakeStreamSession
	prompts []string
}

func (p *promptCaptureStreamSession) Stream(ctx context.Context, prompt string) (<-chan Event, error) {
	p.prompts = append(p.prompts, prompt)
	return p.fakeStreamSession.Stream(ctx, prompt)
}

func TestExecutorStreamInjectsSpeakerIntoPrompt(t *testing.T) {
	capture := &promptCaptureStreamSession{
		fakeStreamSession: newFakeStreamSession([][]string{{"ok"}}, []string{"ok"}),
	}
	executor := NewExecutor(ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (Session, error) { return capture, nil },
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    3,
		BasePrompt:  "you are a helper",
	})
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "stream question"})
	msg.ContextID = "ctx-stream-speaker"
	msg.Metadata = map[string]any{MetadataSpeakerKey: "bob"}

	for _, err := range executor.ExecuteStream(context.Background(), msg) {
		if err != nil {
			t.Fatalf("yield err: %v", err)
		}
	}
	if len(capture.prompts) == 0 {
		t.Fatal("stream session never received a prompt")
	}
	if !strings.Contains(capture.prompts[0], "[speaker: bob] stream question") {
		t.Errorf("stream prompt missing speaker tag:\n%s", capture.prompts[0])
	}
	if !strings.Contains(capture.prompts[0], "multiple people") {
		t.Errorf("stream prompt missing multi-speaker instruction:\n%s", capture.prompts[0])
	}
}

// --- transcript wiring (specs/2026-06-08-shared-context-collaboration.md) ---

func TestExecutorAppendsTranscriptTurn(t *testing.T) {
	store := NewTranscriptStore(t.TempDir())
	sender := func(ctx context.Context, prompt string) (string, error) {
		return "the answer is red\n", nil
	}
	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSessionFromSender(sender),
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    3,
		BasePrompt:  "you are a helper",
		Transcript:  store,
	})
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "what color?"})
	msg.ContextID = "ctx-transcript"
	msg.Metadata = map[string]any{MetadataSpeakerKey: "alice"}

	task, err := executor.Execute(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}

	turns, err := store.Read("ctx-transcript")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("transcript turns = %d, want 1", len(turns))
	}
	turn := turns[0]
	if turn.Speaker != "alice" {
		t.Errorf("speaker = %q, want alice", turn.Speaker)
	}
	// Raw user text — the [speaker:] tag is prompt-layer decoration; the
	// transcript has a dedicated speaker field.
	if turn.UserText != "what color?" {
		t.Errorf("userText = %q, want raw text", turn.UserText)
	}
	if !strings.Contains(turn.Reply, "the answer is red") {
		t.Errorf("reply = %q, want final text", turn.Reply)
	}
	if turn.Status != "completed" {
		t.Errorf("status = %q, want completed", turn.Status)
	}
	if turn.TaskID != string(task.ID) {
		t.Errorf("taskId = %q, want %q", turn.TaskID, task.ID)
	}
}

func TestExecutorAppendsFailedTranscriptTurn(t *testing.T) {
	store := NewTranscriptStore(t.TempDir())
	sender := func(ctx context.Context, prompt string) (string, error) {
		return "", context.DeadlineExceeded
	}
	executor := NewExecutor(ExecutorConfig{
		OpenSession: openSessionFromSender(sender),
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    3,
		BasePrompt:  "you are a helper",
		Transcript:  store,
	})
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "doomed"})
	msg.ContextID = "ctx-transcript-fail"

	if _, err := executor.Execute(context.Background(), msg); err == nil {
		t.Fatal("expected Execute to fail")
	}

	turns, err := store.Read("ctx-transcript-fail")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("transcript turns = %d, want 1", len(turns))
	}
	if turns[0].Status != "failed" || turns[0].Error == "" {
		t.Errorf("failed turn must record status=failed with error, got %+v", turns[0])
	}
}

func TestExecutorStreamAppendsTranscriptTurn(t *testing.T) {
	store := NewTranscriptStore(t.TempDir())
	fake := newFakeStreamSession([][]string{{"str", "eam"}}, []string{"stream reply"})
	executor := NewExecutor(ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (Session, error) { return fake, nil },
		ListAgents:  func() []*a2a.AgentCard { return nil },
		MaxDepth:    3,
		BasePrompt:  "you are a helper",
		Transcript:  store,
	})
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "stream it"})
	msg.ContextID = "ctx-transcript-stream"
	msg.Metadata = map[string]any{MetadataSpeakerKey: "bob"}

	for _, err := range executor.ExecuteStream(context.Background(), msg) {
		if err != nil {
			t.Fatalf("yield err: %v", err)
		}
	}

	turns, err := store.Read("ctx-transcript-stream")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("transcript turns = %d, want 1", len(turns))
	}
	if turns[0].Speaker != "bob" || !strings.Contains(turns[0].Reply, "stream reply") || turns[0].Status != "completed" {
		t.Errorf("stream transcript turn mismatch: %+v", turns[0])
	}
}
