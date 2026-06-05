package wrapper

import (
	"context"
	"fmt"
	"iter"
	"log"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

// ExecutorConfig configures the agent execution loop.
type ExecutorConfig struct {
	// OpenSession returns the Session to use for the current request,
	// keyed by A2A contextID. Production wires this to SessionPool.LookupOrCreate
	// so multiple requests sharing a contextID hit the same long-running
	// claude process — claude itself maintains conversation history.
	// Implementations must surface non-zero exits / stderr via Session.Turn's
	// returned error rather than returning ("", nil).
	OpenSession func(ctx context.Context, contextID string) (Session, error)
	ListAgents  func() []*a2a.AgentCard
	// CallAgent invokes another agent over A2A. The contextID arg is the
	// CURRENT task's contextID — passing it through to the callee makes the
	// callee's SessionPool reuse a single claude process across multiple
	// delegations within one conversation. Empty contextID means "no
	// conversation continuity" (callee will auto-generate one).
	CallAgent func(ctx context.Context, agentName, contextID, task string) (string, error)
	MaxDepth    int
	BasePrompt  string
	// SelfName is the agent name running this executor — surfaced in A2A_CALL
	// dispatch / reply logs so operators can read inter-agent traffic. Optional;
	// empty falls back to "agent".
	SelfName string
}

// Executor runs the core agent loop: receive message → prompt the LLM →
// parse A2A_CALL markers → execute sub-agent calls → inject results → return task.
type Executor struct {
	openSession func(ctx context.Context, contextID string) (Session, error)
	listAgents  func() []*a2a.AgentCard
	callAgent   func(ctx context.Context, agentName, contextID, task string) (string, error)
	maxDepth    int
	basePrompt  string
	selfName    string
}

// NewExecutor creates a new Executor.
// If MaxDepth is not set (0), it defaults to 5.
func NewExecutor(cfg ExecutorConfig) *Executor {
	return &Executor{
		openSession: cfg.OpenSession,
		listAgents:  cfg.ListAgents,
		callAgent:   cfg.CallAgent,
		maxDepth:    cfg.MaxDepth,
		basePrompt:  cfg.BasePrompt,
		selfName:    cfg.SelfName,
	}
}

// Execute runs the agent execution loop for an incoming message.
func (e *Executor) Execute(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
	// Pass msg as the TaskInfoProvider so msg.ContextID propagates onto the
	// new task — this is what links sequential message/send calls into one
	// conversation. NewSubmittedTask generates a fresh ContextID only if
	// msg.ContextID is empty.
	task := a2a.NewSubmittedTask(msg, msg)

	// Open a Session for this request. SessionPool.LookupOrCreate returns
	// the same long-running Session for a given contextID; Session lifetime
	// is owned by the pool, so this code intentionally does NOT defer Close.
	session, err := e.openSession(ctx, task.ContextID)
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		return task, err
	}

	// Build the full system prompt with available agents. Conversation
	// history is held inside the claude process across turns — wrapper no
	// longer prepends it to the prompt.
	agents := e.listAgents()
	systemPrompt := BuildSystemPrompt(e.basePrompt, agents, e.maxDepth)
	userText := messageText(msg)
	fullPrompt := systemPrompt + "\n\n" + userText + "\n"

	resultText, history, err := e.interact(ctx, session, task, fullPrompt, 0, userText)
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		return task, err
	}

	task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	task.History = history
	_ = resultText

	return task, nil
}

// interact runs the recursive agent interaction loop. The session is threaded
// through recursion so all turns in one Execute share the same Session
// instance — for ClaudeSession this means the same long-running process
// handles the initial user turn and any sub-agent injection rounds, with
// claude's own memory of intermediate state intact.
func (e *Executor) interact(ctx context.Context, session Session, task *a2a.Task, prompt string, depth int, originalTask string) (string, []*a2a.Message, error) {
	responseText, err := session.Turn(ctx, prompt)
	if err != nil {
		return "", task.History, fmt.Errorf("session turn: %w", err)
	}

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
		return e.interact(ctx, session, task, errorMsg, depth, originalTask)
	}

	if e.callAgent == nil {
		errorMsg := fmt.Sprintf("\n[Cannot call agent %s: no agent caller configured]\n", call.Agent)
		return e.interact(ctx, session, task, errorMsg, depth, originalTask)
	}

	// Execute the sub-agent call. Logs surround it so the scheduler tee shows
	// the cross-agent traffic — without these, A2A_CALL dispatches are
	// invisible at the wrapper layer.
	selfTag := e.selfName
	if selfTag == "" {
		selfTag = "agent"
	}
	log.Printf("[%s → %s] A2A_CALL: task=%q", selfTag, call.Agent, truncateForLog(call.Task, 300))
	callStart := time.Now()
	// Thread task.ContextID through so the callee's SessionPool can reuse a
	// session across multiple delegations within the same conversation.
	agentResult, err := e.callAgent(ctx, call.Agent, task.ContextID, call.Task)
	if err != nil {
		log.Printf("[%s ← %s] A2A_CALL failed in %v: %v", selfTag, call.Agent, time.Since(callStart), err)
		errorMsg := fmt.Sprintf("\n[Agent %s call failed: %v]\n", call.Agent, err)
		return e.interact(ctx, session, task, errorMsg, depth, originalTask)
	}
	log.Printf("[%s ← %s] reply: took=%v bytes=%d preview=%q", selfTag, call.Agent, time.Since(callStart), len(agentResult), truncateForLog(agentResult, 300))

	// Inject the result and continue
	injection := BuildInjectionPrompt(call.Agent, originalTask, agentResult)
	return e.interact(ctx, session, task, injection, depth+1, originalTask)
}

// ExecuteStream is the streaming counterpart of Execute. It runs the same
// agent loop (LLM turn → optional A2A_CALL recursion) but emits
// TaskStatusUpdateEvent for each EventTextDelta the Session produces, then
// yields the completed *a2a.Task as the final event. Non-streaming clients
// can still use Execute; both paths share the same Session (so a cached
// claude process persists conversation state across either mode).
//
// The yield function returns false when the consumer stops iterating —
// ExecuteStream honours that by ceasing further work, but still attempts a
// best-effort cleanup of the in-flight Session turn so the next call against
// the same contextID gets a healthy session.
func (e *Executor) ExecuteStream(ctx context.Context, msg *a2a.Message) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		task := a2a.NewSubmittedTask(msg, msg)

		session, err := e.openSession(ctx, task.ContextID)
		if err != nil {
			task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
			yield(task, err)
			return
		}

		agents := e.listAgents()
		systemPrompt := BuildSystemPrompt(e.basePrompt, agents, e.maxDepth)
		userText := messageText(msg)
		fullPrompt := systemPrompt + "\n\n" + userText + "\n"

		// Announce that we've picked up the task. State==Working so subscribers
		// can show a spinner before the first delta lands.
		if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil), nil) {
			return
		}

		if !e.interactStream(ctx, session, task, fullPrompt, 0, userText, yield) {
			return
		}

		task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
		yield(task, nil)
	}
}

// interactStream is the streaming sibling of interact(). Per-delta events are
// yielded as TaskStatusUpdateEvent; A2A_CALL handling mirrors interact() but
// after the streaming turn drains. Returns false when the consumer rejected a
// yield (so the caller can stop the outer iteration cleanly).
func (e *Executor) interactStream(ctx context.Context, session Session, task *a2a.Task, prompt string, depth int, originalTask string, yield func(a2a.Event, error) bool) bool {
	ch, err := session.Stream(ctx, prompt)
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		yield(task, fmt.Errorf("session stream: %w", err))
		return false
	}

	var fullText strings.Builder
	var turnErr error
	// Drain the channel fully even if yield is rejected mid-stream — otherwise
	// the underlying ClaudeSession stays in stateInFlight and the next request
	// against the same contextID errors out with "previous turn not drained".
	stopYielding := false
	for ev := range ch {
		switch e := ev.(type) {
		case EventTextDelta:
			if stopYielding {
				continue
			}
			deltaMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: e.Text})
			if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, deltaMsg), nil) {
				stopYielding = true
			}
		case EventText:
			fullText.WriteString(e.Text)
		case EventTurnDone:
			turnErr = e.Err
		}
	}
	if stopYielding {
		return false
	}

	responseText := fullText.String()
	if turnErr != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		yield(task, fmt.Errorf("session turn: %w", turnErr))
		return false
	}

	respMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: responseText})
	task.History = append(task.History, respMsg)

	call, ok := ParseA2ACall(responseText)
	if !ok {
		return true
	}
	if depth >= e.maxDepth {
		return true
	}
	if err := ValidateA2ACall(call); err != nil {
		errorMsg := fmt.Sprintf("\n[A2A_CALL error: %v]\n", err)
		return e.interactStream(ctx, session, task, errorMsg, depth, originalTask, yield)
	}
	if e.callAgent == nil {
		errorMsg := fmt.Sprintf("\n[Cannot call agent %s: no agent caller configured]\n", call.Agent)
		return e.interactStream(ctx, session, task, errorMsg, depth, originalTask, yield)
	}

	selfTag := e.selfName
	if selfTag == "" {
		selfTag = "agent"
	}
	log.Printf("[%s → %s] A2A_CALL: task=%q", selfTag, call.Agent, truncateForLog(call.Task, 300))
	callStart := time.Now()
	agentResult, err := e.callAgent(ctx, call.Agent, task.ContextID, call.Task)
	if err != nil {
		log.Printf("[%s ← %s] A2A_CALL failed in %v: %v", selfTag, call.Agent, time.Since(callStart), err)
		errorMsg := fmt.Sprintf("\n[Agent %s call failed: %v]\n", call.Agent, err)
		return e.interactStream(ctx, session, task, errorMsg, depth, originalTask, yield)
	}
	log.Printf("[%s ← %s] reply: took=%v bytes=%d preview=%q", selfTag, call.Agent, time.Since(callStart), len(agentResult), truncateForLog(agentResult, 300))

	injection := BuildInjectionPrompt(call.Agent, originalTask, agentResult)
	return e.interactStream(ctx, session, task, injection, depth+1, originalTask, yield)
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
