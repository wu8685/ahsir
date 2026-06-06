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
	CallAgent  func(ctx context.Context, agentName, contextID, task string) (string, error)
	MaxDepth   int
	BasePrompt string
	// SelfName is the agent name running this executor — surfaced in
	// agent-call dispatch / reply logs so operators can read inter-agent
	// traffic. Optional; empty falls back to "agent".
	SelfName string
}

// Executor runs the core agent loop: receive message → prompt the LLM →
// handle structured agent calls (or legacy A2A_CALL markers) → execute
// sub-agent calls → inject results → return task.
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

type turnResult struct {
	text            string
	call            *A2ACall
	turnErr         error
	took            time.Duration
	streamOpen      time.Duration
	events          int
	textEvents      int
	deltas          int
	deltaBytes      int
	agentCallEvents int
	toolUseEvents   int
	callSource      string
	stats           TurnStats
}

// Execute runs the agent execution loop for an incoming message.
func (e *Executor) Execute(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
	started := time.Now()
	selfTag := e.logName()
	// Pass msg as the TaskInfoProvider so msg.ContextID propagates onto the
	// new task — this is what links sequential message/send calls into one
	// conversation. NewSubmittedTask generates a fresh ContextID only if
	// msg.ContextID is empty.
	task := a2a.NewSubmittedTask(msg, msg)
	log.Printf("[%s] executor start contextID=%s msgID=%s mode=send", selfTag, task.ContextID, msg.ID)

	// Open a Session for this request. SessionPool.LookupOrCreate returns
	// the same long-running Session for a given contextID; Session lifetime
	// is owned by the pool, so this code intentionally does NOT defer Close.
	openStarted := time.Now()
	session, err := e.openSession(ctx, task.ContextID)
	openTook := time.Since(openStarted)
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		log.Printf("[%s] executor open_session failed contextID=%s msgID=%s took=%v err=%v", selfTag, task.ContextID, msg.ID, openTook, err)
		return task, err
	}
	log.Printf("[%s] executor open_session done contextID=%s msgID=%s took=%v", selfTag, task.ContextID, msg.ID, openTook)

	// Build the full system prompt with available agents. Conversation
	// history is held inside the claude process across turns — wrapper no
	// longer prepends it to the prompt.
	promptStarted := time.Now()
	agents := e.listAgents()
	systemPrompt := BuildSystemPrompt(e.basePrompt, agents, e.maxDepth)
	userText := messageText(msg)
	fullPrompt := systemPrompt + "\n\n" + userText + "\n"
	log.Printf("[%s] executor prompt_ready contextID=%s msgID=%s agents=%d user_bytes=%d prompt_bytes=%d took=%v", selfTag, task.ContextID, msg.ID, len(agents), len(userText), len(fullPrompt), time.Since(promptStarted))

	resultText, history, err := e.interact(ctx, session, task, fullPrompt, 0, userText)
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		log.Printf("[%s] executor failed contextID=%s msgID=%s took=%v err=%v", selfTag, task.ContextID, msg.ID, time.Since(started), err)
		return task, err
	}

	task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	task.History = history
	_ = resultText
	log.Printf("[%s] executor done contextID=%s msgID=%s history=%d took=%v", selfTag, task.ContextID, msg.ID, len(task.History), time.Since(started))

	return task, nil
}

// interact runs the recursive agent interaction loop. The session is threaded
// through recursion so all turns in one Execute share the same Session
// instance — for ClaudeSession this means the same long-running process
// handles the initial user turn and any sub-agent injection rounds, with
// claude's own memory of intermediate state intact.
func (e *Executor) interact(ctx context.Context, session Session, task *a2a.Task, prompt string, depth int, originalTask string) (string, []*a2a.Message, error) {
	selfTag := e.logName()
	log.Printf("[%s] executor turn start contextID=%s depth=%d prompt_bytes=%d", selfTag, task.ContextID, depth, len(prompt))
	turn, err := e.runTurn(ctx, session, prompt, nil)
	if err != nil {
		log.Printf("[%s] executor turn stream_open failed contextID=%s depth=%d took=%v stream_open=%v err=%v", selfTag, task.ContextID, depth, turn.took, turn.streamOpen, err)
		return "", task.History, fmt.Errorf("session turn: %w", err)
	}
	log.Printf("[%s] executor turn done contextID=%s depth=%d took=%v stream_open=%v events=%d text_events=%d deltas=%d delta_bytes=%d agent_call_events=%d tool_use_events=%d response_bytes=%d input_tokens=%d output_tokens=%d cost_usd=%.6f provider_duration_ms=%d call=%t call_source=%s turn_err=%v", selfTag, task.ContextID, depth, turn.took, turn.streamOpen, turn.events, turn.textEvents, turn.deltas, turn.deltaBytes, turn.agentCallEvents, turn.toolUseEvents, len(turn.text), turn.stats.InputTokens, turn.stats.OutputTokens, turn.stats.CostUSD, turn.stats.DurationMS, turn.call != nil, turn.callSource, turn.turnErr)
	if turn.turnErr != nil {
		return "", task.History, fmt.Errorf("session turn: %w", turn.turnErr)
	}

	// Record agent response in history
	respMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: turn.text})
	task.History = append(task.History, respMsg)

	if turn.call == nil {
		return turn.text, task.History, nil
	}

	// If max depth reached, stop making sub-calls
	if depth >= e.maxDepth {
		log.Printf("[%s] executor max_depth_reached contextID=%s depth=%d max_depth=%d call_agent=%s", selfTag, task.ContextID, depth, e.maxDepth, turn.call.Agent)
		return turn.text, task.History, nil
	}

	if err := ValidateA2ACall(*turn.call); err != nil {
		// Invalid call, but continue processing
		errorMsg := fmt.Sprintf("\n[A2A_CALL error: %v]\n", err)
		return e.interact(ctx, session, task, errorMsg, depth, originalTask)
	}

	if e.callAgent == nil {
		errorMsg := fmt.Sprintf("\n[Cannot call agent %s: no agent caller configured]\n", turn.call.Agent)
		return e.interact(ctx, session, task, errorMsg, depth, originalTask)
	}

	// Execute the sub-agent call. Logs surround it so the scheduler tee shows
	// the cross-agent traffic — without these, A2A_CALL dispatches are
	// invisible at the wrapper layer.
	log.Printf("[%s → %s] A2A_CALL: contextID=%s depth=%d source=%s task=%q", selfTag, turn.call.Agent, task.ContextID, depth, turn.callSource, truncateForLog(turn.call.Task, 300))
	callStart := time.Now()
	// Thread task.ContextID through so the callee's SessionPool can reuse a
	// session across multiple delegations within the same conversation.
	agentResult, err := e.callAgent(ctx, turn.call.Agent, task.ContextID, turn.call.Task)
	if err != nil {
		log.Printf("[%s ← %s] A2A_CALL failed contextID=%s depth=%d took=%v err=%v", selfTag, turn.call.Agent, task.ContextID, depth, time.Since(callStart), err)
		errorMsg := fmt.Sprintf("\n[Agent %s call failed: %v]\n", turn.call.Agent, err)
		return e.interact(ctx, session, task, errorMsg, depth, originalTask)
	}
	log.Printf("[%s ← %s] reply: contextID=%s depth=%d took=%v bytes=%d preview=%q", selfTag, turn.call.Agent, task.ContextID, depth, time.Since(callStart), len(agentResult), truncateForLog(agentResult, 300))

	// Inject the result and continue
	injectStarted := time.Now()
	injection := BuildInjectionPrompt(turn.call.Agent, originalTask, agentResult)
	log.Printf("[%s] executor injection_ready contextID=%s depth=%d agent=%s result_bytes=%d injection_bytes=%d took=%v", selfTag, task.ContextID, depth, turn.call.Agent, len(agentResult), len(injection), time.Since(injectStarted))
	return e.interact(ctx, session, task, injection, depth+1, originalTask)
}

func (e *Executor) runTurn(ctx context.Context, session Session, prompt string, onDelta func(string) bool) (turnResult, error) {
	turnStarted := time.Now()
	streamStarted := time.Now()
	ch, err := session.Stream(ctx, prompt)
	streamOpen := time.Since(streamStarted)
	if err != nil {
		return turnResult{took: time.Since(turnStarted), streamOpen: streamOpen}, err
	}

	var fullText strings.Builder
	result := turnResult{streamOpen: streamOpen}
	stopDeltas := false
	for ev := range ch {
		result.events++
		switch x := ev.(type) {
		case EventTextDelta:
			result.deltas++
			result.deltaBytes += len(x.Text)
			if onDelta != nil && !stopDeltas {
				if !onDelta(x.Text) {
					stopDeltas = true
				}
			}
		case EventText:
			result.textEvents++
			fullText.WriteString(x.Text)
		case EventAgentCall:
			result.agentCallEvents++
			c := A2ACall{Agent: x.Agent, Task: x.Task}
			result.call = &c
			result.callSource = "structured"
		case EventToolUse:
			result.toolUseEvents++
		case EventTurnDone:
			result.turnErr = x.Err
			result.stats = x.Stats
		}
	}
	result.text = fullText.String()
	if result.call == nil {
		if parsed, ok := ParseA2ACall(result.text); ok {
			result.call = &parsed
			result.callSource = "legacy_text"
		}
	}
	if result.call == nil {
		result.callSource = "none"
	}
	result.took = time.Since(turnStarted)
	return result, nil
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
		started := time.Now()
		selfTag := e.logName()
		task := a2a.NewSubmittedTask(msg, msg)
		log.Printf("[%s] executor start contextID=%s msgID=%s mode=stream", selfTag, task.ContextID, msg.ID)

		openStarted := time.Now()
		session, err := e.openSession(ctx, task.ContextID)
		openTook := time.Since(openStarted)
		if err != nil {
			task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
			log.Printf("[%s] executor open_session failed contextID=%s msgID=%s took=%v err=%v", selfTag, task.ContextID, msg.ID, openTook, err)
			yield(task, err)
			return
		}
		log.Printf("[%s] executor open_session done contextID=%s msgID=%s took=%v", selfTag, task.ContextID, msg.ID, openTook)

		promptStarted := time.Now()
		agents := e.listAgents()
		systemPrompt := BuildSystemPrompt(e.basePrompt, agents, e.maxDepth)
		userText := messageText(msg)
		fullPrompt := systemPrompt + "\n\n" + userText + "\n"
		log.Printf("[%s] executor prompt_ready contextID=%s msgID=%s agents=%d user_bytes=%d prompt_bytes=%d took=%v", selfTag, task.ContextID, msg.ID, len(agents), len(userText), len(fullPrompt), time.Since(promptStarted))

		// Announce that we've picked up the task. State==Working so subscribers
		// can show a spinner before the first delta lands.
		if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil), nil) {
			return
		}

		if !e.interactStream(ctx, session, task, fullPrompt, 0, userText, yield) {
			return
		}

		task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
		log.Printf("[%s] executor done contextID=%s msgID=%s history=%d took=%v", selfTag, task.ContextID, msg.ID, len(task.History), time.Since(started))
		yield(task, nil)
	}
}

// interactStream is the streaming sibling of interact(). Per-delta events are
// yielded as TaskStatusUpdateEvent; A2A_CALL handling mirrors interact() but
// after the streaming turn drains. Returns false when the consumer rejected a
// yield (so the caller can stop the outer iteration cleanly).
func (e *Executor) interactStream(ctx context.Context, session Session, task *a2a.Task, prompt string, depth int, originalTask string, yield func(a2a.Event, error) bool) bool {
	stopYielding := false
	selfTag := e.logName()
	log.Printf("[%s] executor turn start contextID=%s depth=%d prompt_bytes=%d stream=true", selfTag, task.ContextID, depth, len(prompt))
	turn, err := e.runTurn(ctx, session, prompt, func(delta string) bool {
		deltaMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: delta})
		if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, deltaMsg), nil) {
			stopYielding = true
			return false
		}
		return true
	})
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		log.Printf("[%s] executor turn stream_open failed contextID=%s depth=%d err=%v", selfTag, task.ContextID, depth, err)
		yield(task, fmt.Errorf("session stream: %w", err))
		return false
	}
	log.Printf("[%s] executor turn done contextID=%s depth=%d took=%v stream_open=%v events=%d text_events=%d deltas=%d delta_bytes=%d agent_call_events=%d tool_use_events=%d response_bytes=%d input_tokens=%d output_tokens=%d cost_usd=%.6f provider_duration_ms=%d call=%t call_source=%s turn_err=%v stream=true", selfTag, task.ContextID, depth, turn.took, turn.streamOpen, turn.events, turn.textEvents, turn.deltas, turn.deltaBytes, turn.agentCallEvents, turn.toolUseEvents, len(turn.text), turn.stats.InputTokens, turn.stats.OutputTokens, turn.stats.CostUSD, turn.stats.DurationMS, turn.call != nil, turn.callSource, turn.turnErr)
	if stopYielding {
		return false
	}
	if turn.turnErr != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		yield(task, fmt.Errorf("session turn: %w", turn.turnErr))
		return false
	}

	respMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: turn.text})
	task.History = append(task.History, respMsg)

	if turn.call == nil {
		return true
	}
	if depth >= e.maxDepth {
		log.Printf("[%s] executor max_depth_reached contextID=%s depth=%d max_depth=%d call_agent=%s stream=true", selfTag, task.ContextID, depth, e.maxDepth, turn.call.Agent)
		return true
	}
	if err := ValidateA2ACall(*turn.call); err != nil {
		errorMsg := fmt.Sprintf("\n[A2A_CALL error: %v]\n", err)
		return e.interactStream(ctx, session, task, errorMsg, depth, originalTask, yield)
	}
	if e.callAgent == nil {
		errorMsg := fmt.Sprintf("\n[Cannot call agent %s: no agent caller configured]\n", turn.call.Agent)
		return e.interactStream(ctx, session, task, errorMsg, depth, originalTask, yield)
	}

	log.Printf("[%s → %s] A2A_CALL: contextID=%s depth=%d source=%s task=%q stream=true", selfTag, turn.call.Agent, task.ContextID, depth, turn.callSource, truncateForLog(turn.call.Task, 300))
	callStart := time.Now()
	agentResult, err := e.callAgent(ctx, turn.call.Agent, task.ContextID, turn.call.Task)
	if err != nil {
		log.Printf("[%s ← %s] A2A_CALL failed contextID=%s depth=%d took=%v err=%v stream=true", selfTag, turn.call.Agent, task.ContextID, depth, time.Since(callStart), err)
		errorMsg := fmt.Sprintf("\n[Agent %s call failed: %v]\n", turn.call.Agent, err)
		return e.interactStream(ctx, session, task, errorMsg, depth, originalTask, yield)
	}
	log.Printf("[%s ← %s] reply: contextID=%s depth=%d took=%v bytes=%d preview=%q stream=true", selfTag, turn.call.Agent, task.ContextID, depth, time.Since(callStart), len(agentResult), truncateForLog(agentResult, 300))

	injectStarted := time.Now()
	injection := BuildInjectionPrompt(turn.call.Agent, originalTask, agentResult)
	log.Printf("[%s] executor injection_ready contextID=%s depth=%d agent=%s result_bytes=%d injection_bytes=%d took=%v stream=true", selfTag, task.ContextID, depth, turn.call.Agent, len(agentResult), len(injection), time.Since(injectStarted))
	return e.interactStream(ctx, session, task, injection, depth+1, originalTask, yield)
}

func (e *Executor) logName() string {
	if e.selfName != "" {
		return e.selfName
	}
	return "agent"
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
