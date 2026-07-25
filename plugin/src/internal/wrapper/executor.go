package wrapper

import (
	"context"
	"errors"
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
	// MaxDepth bounds the TOTAL number of sub-call rounds in one Execute —
	// failed attempts (invalid A2A_CALL, missing caller, sub-call error)
	// consume the budget exactly like successful delegations. Without that,
	// a model that keeps emitting the same broken call would recurse at
	// constant depth until the transport timeout.
	MaxDepth   int
	BasePrompt string
	// SelfName is the agent name running this executor — surfaced in
	// agent-call dispatch / reply logs so operators can read inter-agent
	// traffic. Optional; empty falls back to "agent".
	SelfName string
	// Transcript, when non-nil, records every finished turn (speaker, full
	// user text, full reply) per contextID so people joining a shared
	// context can replay what happened. Optional — nil disables recording.
	Transcript *TranscriptStore
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
	transcript  *TranscriptStore
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
		transcript:  cfg.Transcript,
	}
}

// appendTranscript records one finished Execute/ExecuteStream round as a
// transcript turn. One user message → one turn, regardless of how many
// sub-agent injection rounds ran inside. No-op without a store. Append
// failures are logged, never propagated — transcript trouble must not fail
// the turn that already succeeded.
func (e *Executor) appendTranscript(task *a2a.Task, msg *a2a.Message, reply string, started time.Time, turnErr error) {
	if e.transcript == nil {
		return
	}
	turn := TranscriptTurn{
		TS:         time.Now(),
		TaskID:     string(task.ID),
		Speaker:    speakerOf(msg),
		UserText:   messageText(msg),
		Reply:      reply,
		Status:     "completed",
		DurationMS: time.Since(started).Milliseconds(),
	}
	if turnErr != nil {
		turn.Status = "failed"
		turn.Error = turnErr.Error()
		turn.Reply = ""
	}
	if err := e.transcript.Append(task.ContextID, turn); err != nil {
		log.Printf("[%s] transcript append failed contextID=%s: %v", e.logName(), task.ContextID, err)
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
		e.appendTranscript(task, msg, "", started, err)
		return task, err
	}
	log.Printf("[%s] executor open_session done contextID=%s msgID=%s took=%v", selfTag, task.ContextID, msg.ID, openTook)

	// Build the full system prompt with available agents. Conversation
	// history is held inside the claude process across turns — wrapper no
	// longer prepends it to the prompt.
	promptStarted := time.Now()
	agents := e.listAgents()
	fullPrompt, userText := e.buildTurnPrompt(agents, msg)
	log.Printf("[%s] executor prompt_ready contextID=%s msgID=%s agents=%d user_bytes=%d prompt_bytes=%d took=%v", selfTag, task.ContextID, msg.ID, len(agents), len(userText), len(fullPrompt), time.Since(promptStarted))

	resultText, history, err := e.interact(ctx, session, task, fullPrompt, 0, userText)
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		log.Printf("[%s] executor failed contextID=%s msgID=%s took=%v err=%v", selfTag, task.ContextID, msg.ID, time.Since(started), err)
		e.appendTranscript(task, msg, "", started, err)
		return task, err
	}

	task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	task.History = history
	log.Printf("[%s] executor done contextID=%s msgID=%s history=%d took=%v", selfTag, task.ContextID, msg.ID, len(task.History), time.Since(started))
	e.appendTranscript(task, msg, resultText, started, nil)

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
	turn, err := e.runTurn(ctx, session, prompt, nil, nil)
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
		// Invalid call: feed the error back so the model can retry, but at
		// depth+1 — failed attempts consume the budget like successful ones,
		// otherwise a model stuck on a malformed call recurses forever.
		errorMsg := fmt.Sprintf("\n[A2A_CALL error: %v]\n", err)
		return e.interact(ctx, session, task, errorMsg, depth+1, originalTask)
	}

	if e.callAgent == nil {
		errorMsg := fmt.Sprintf("\n[Cannot call agent %s: no agent caller configured]\n", turn.call.Agent)
		return e.interact(ctx, session, task, errorMsg, depth+1, originalTask)
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
		return e.interact(ctx, session, task, errorMsg, depth+1, originalTask)
	}
	log.Printf("[%s ← %s] reply: contextID=%s depth=%d took=%v bytes=%d preview=%q", selfTag, turn.call.Agent, task.ContextID, depth, time.Since(callStart), len(agentResult), truncateForLog(agentResult, 300))

	// Inject the result and continue
	injectStarted := time.Now()
	injection := BuildInjectionPrompt(turn.call.Agent, originalTask, agentResult)
	log.Printf("[%s] executor injection_ready contextID=%s depth=%d agent=%s result_bytes=%d injection_bytes=%d took=%v", selfTag, task.ContextID, depth, turn.call.Agent, len(agentResult), len(injection), time.Since(injectStarted))
	return e.interact(ctx, session, task, injection, depth+1, originalTask)
}

// runTurn drives one provider turn. onDelta receives incremental text;
// onAux receives non-text observability events (tool_use, ...) for the streaming
// path to surface on the A2A wire. Either callback may be nil.
func (e *Executor) runTurn(ctx context.Context, session Session, prompt string, onDelta func(string) bool, onAux func(Event) bool) (turnResult, error) {
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
			if onAux != nil {
				onAux(x)
			}
		case EventThinking:
			if onAux != nil {
				onAux(x)
			}
		case EventToolResult:
			if onAux != nil {
				onAux(x)
			}
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
			e.appendTranscript(task, msg, "", started, err)
			yield(task, err)
			return
		}
		log.Printf("[%s] executor open_session done contextID=%s msgID=%s took=%v", selfTag, task.ContextID, msg.ID, openTook)

		promptStarted := time.Now()
		agents := e.listAgents()
		fullPrompt, userText := e.buildTurnPrompt(agents, msg)
		log.Printf("[%s] executor prompt_ready contextID=%s msgID=%s agents=%d user_bytes=%d prompt_bytes=%d took=%v", selfTag, task.ContextID, msg.ID, len(agents), len(userText), len(fullPrompt), time.Since(promptStarted))

		// Announce that we've picked up the task. State==Working so subscribers
		// can show a spinner before the first delta lands.
		if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, nil), nil) {
			return
		}

		// Wrap yield so the terminal error (if any) is observable here —
		// interactStream reports failures by yielding (task, err) and
		// returning false, which otherwise leaves this scope without the
		// error value the transcript needs.
		var streamErr error
		observingYield := func(ev a2a.Event, err error) bool {
			if err != nil {
				streamErr = err
			}
			return yield(ev, err)
		}

		if !e.interactStream(ctx, session, task, fullPrompt, 0, userText, observingYield) {
			// false means either a real failure (task marked Failed, error
			// captured above) or the consumer stopped reading. Only the
			// former is a finished turn worth recording — an abandoned
			// stream has no defined outcome.
			if task.Status.State == a2a.TaskStateFailed && streamErr != nil {
				e.appendTranscript(task, msg, "", started, streamErr)
			}
			return
		}

		task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
		// Mirror the final agent reply into Status.Message. Real providers
		// stream text deltas that fill the consumer's buffer directly, but a
		// delta-less runtime's reply lives only in History — and the A2A
		// "terminal message" fallback (e.g. the gateway's ChatStream no-delta
		// path) reads Status.Message. Setting it here keeps the aggregated
		// reply non-empty over message/stream regardless of whether deltas
		// were emitted. See issue #15.
		if final := finalAgentMessage(task); final != nil {
			task.Status.Message = final
		}
		log.Printf("[%s] executor done contextID=%s msgID=%s history=%d took=%v", selfTag, task.ContextID, msg.ID, len(task.History), time.Since(started))
		e.appendTranscript(task, msg, finalAgentText(task), started, nil)
		yield(task, nil)
	}
}

// finalAgentText extracts the last agent-authored text from a task's history —
// the canonical "reply" of a streaming round, mirroring what taskToString
// returns for the blocking path.
func finalAgentText(task *a2a.Task) string {
	if msg := finalAgentMessage(task); msg != nil {
		return messageText(msg)
	}
	return ""
}

// finalAgentMessage returns the last agent-authored message that carries text,
// or nil when the task has none. This is the message form of finalAgentText:
// ExecuteStream mirrors it into the terminal task's Status.Message so streaming
// consumers relying on the A2A "terminal message" fallback still see the reply
// when the runtime emitted no text deltas (see issue #15).
func finalAgentMessage(task *a2a.Task) *a2a.Message {
	for i := len(task.History) - 1; i >= 0; i-- {
		if task.History[i].Role != a2a.MessageRoleAgent {
			continue
		}
		if messageText(task.History[i]) != "" {
			return task.History[i]
		}
	}
	return nil
}

// interactStream is the streaming sibling of interact(). Per-delta events are
// yielded as TaskStatusUpdateEvent; A2A_CALL handling mirrors interact() but
// after the streaming turn drains. Returns false when the consumer rejected a
// yield (so the caller can stop the outer iteration cleanly).
func (e *Executor) interactStream(ctx context.Context, session Session, task *a2a.Task, prompt string, depth int, originalTask string, yield func(a2a.Event, error) bool) bool {
	stopYielding := false
	selfTag := e.logName()
	log.Printf("[%s] executor turn start contextID=%s depth=%d prompt_bytes=%d stream=true", selfTag, task.ContextID, depth, len(prompt))

	// span: one model-request span per provider turn (this interactStream
	// invocation). The matching span_end (with usage) is emitted after runTurn.
	if !yield(dataStatusUpdate(task, map[string]any{"ev": "span_start"}), nil) {
		return false
	}

	turn, err := e.runTurn(ctx, session, prompt,
		func(delta string) bool {
			deltaMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: delta})
			if !yield(a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, deltaMsg), nil) {
				stopYielding = true
				return false
			}
			return true
		},
		func(aux Event) bool {
			// Surface structured observability events on the wire as DataParts
			// (a2a_call / text are handled elsewhere).
			var data map[string]any
			switch x := aux.(type) {
			case EventToolUse:
				data = map[string]any{"ev": "tool_use", "id": x.Id, "name": x.Name}
				if len(x.Input) > 0 {
					data["input"] = x.Input
				}
			case EventThinking:
				data = map[string]any{"ev": "thinking"}
			case EventToolResult:
				data = map[string]any{"ev": "tool_result", "tool_use_id": x.ToolUseID, "content": x.Content, "is_error": x.IsError}
			default:
				return true
			}
			if !yield(dataStatusUpdate(task, data), nil) {
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

	// span: model request ended — carry per-turn usage.
	yield(dataStatusUpdate(task, map[string]any{"ev": "span_end", "usage": map[string]any{
		"input_tokens":  turn.stats.InputTokens,
		"output_tokens": turn.stats.OutputTokens,
	}}), nil)
	log.Printf("[%s] executor turn done contextID=%s depth=%d took=%v stream_open=%v events=%d text_events=%d deltas=%d delta_bytes=%d agent_call_events=%d tool_use_events=%d response_bytes=%d input_tokens=%d output_tokens=%d cost_usd=%.6f provider_duration_ms=%d call=%t call_source=%s turn_err=%v stream=true", selfTag, task.ContextID, depth, turn.took, turn.streamOpen, turn.events, turn.textEvents, turn.deltas, turn.deltaBytes, turn.agentCallEvents, turn.toolUseEvents, len(turn.text), turn.stats.InputTokens, turn.stats.OutputTokens, turn.stats.CostUSD, turn.stats.DurationMS, turn.call != nil, turn.callSource, turn.turnErr)
	if stopYielding {
		return false
	}
	if turn.turnErr != nil {
		// A deliberate cancellation (A2A tasks/cancel) is a clean terminal
		// state, not a failure: emit a CANCELED task with no error so the
		// caller settles to canceled rather than surfacing an error.
		if errors.Is(turn.turnErr, ErrTurnCanceled) {
			task.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
			yield(task, nil)
			return false
		}
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
		// Failed attempts consume the depth budget — see interact().
		errorMsg := fmt.Sprintf("\n[A2A_CALL error: %v]\n", err)
		return e.interactStream(ctx, session, task, errorMsg, depth+1, originalTask, yield)
	}
	if e.callAgent == nil {
		errorMsg := fmt.Sprintf("\n[Cannot call agent %s: no agent caller configured]\n", turn.call.Agent)
		return e.interactStream(ctx, session, task, errorMsg, depth+1, originalTask, yield)
	}

	log.Printf("[%s → %s] A2A_CALL: contextID=%s depth=%d source=%s task=%q stream=true", selfTag, turn.call.Agent, task.ContextID, depth, turn.callSource, truncateForLog(turn.call.Task, 300))
	callStart := time.Now()
	agentResult, err := e.callAgent(ctx, turn.call.Agent, task.ContextID, turn.call.Task)
	if err != nil {
		log.Printf("[%s ← %s] A2A_CALL failed contextID=%s depth=%d took=%v err=%v stream=true", selfTag, turn.call.Agent, task.ContextID, depth, time.Since(callStart), err)
		errorMsg := fmt.Sprintf("\n[Agent %s call failed: %v]\n", turn.call.Agent, err)
		return e.interactStream(ctx, session, task, errorMsg, depth+1, originalTask, yield)
	}
	log.Printf("[%s ← %s] reply: contextID=%s depth=%d took=%v bytes=%d preview=%q stream=true", selfTag, turn.call.Agent, task.ContextID, depth, time.Since(callStart), len(agentResult), truncateForLog(agentResult, 300))

	injectStarted := time.Now()
	injection := BuildInjectionPrompt(turn.call.Agent, originalTask, agentResult)
	log.Printf("[%s] executor injection_ready contextID=%s depth=%d agent=%s result_bytes=%d injection_bytes=%d took=%v stream=true", selfTag, task.ContextID, depth, turn.call.Agent, len(agentResult), len(injection), time.Since(injectStarted))
	return e.interactStream(ctx, session, task, injection, depth+1, originalTask, yield)
}

// dataStatusUpdate builds a working status-update whose message carries a single
// DataPart — the channel for structured observability events (tool_use, span,
// ...) that cma-service maps to CMA agent.*/span.* events. Text deltas continue
// to ride as TextParts on their own status-updates.
func dataStatusUpdate(task *a2a.Task, data map[string]any) a2a.Event {
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.DataPart{Data: data})
	return a2a.NewStatusUpdateEvent(task, a2a.TaskStateWorking, msg)
}

func (e *Executor) logName() string {
	if e.selfName != "" {
		return e.selfName
	}
	return "agent"
}

// speakerInstruction is appended to the system prompt when a turn carries
// speaker attribution (shared-context collaboration). Without it the model
// silently attributes every prior statement to the current sender — see the
// misattribution experiment in specs/2026-06-08-shared-context-collaboration.md.
const speakerInstruction = "Messages in this conversation may come from multiple people. Each message is tagged with [speaker: <name>] at the start. Attribute statements, preferences, and requests to the speaker who made them; do not assume consecutive messages come from the same person."

// speakerOf extracts the self-claimed speaker from message metadata. Empty
// when absent — old clients and internal delegation produce no tag.
func speakerOf(msg *a2a.Message) string {
	if msg == nil || msg.Metadata == nil {
		return ""
	}
	s, _ := msg.Metadata[MetadataSpeakerKey].(string)
	return s
}

// buildTurnPrompt assembles the opening-turn prompt. A non-empty speaker tags
// the user text with [speaker: <name>] and appends speakerInstruction to the
// system prompt; an empty speaker reproduces the legacy prompt byte-for-byte.
// Returns the full prompt and the (possibly tagged) user text — the latter
// feeds logging and the originalTask thread for sub-agent injections, where
// keeping the tag preserves attribution across delegation.
func (e *Executor) buildTurnPrompt(agents []*a2a.AgentCard, msg *a2a.Message) (fullPrompt, userText string) {
	systemPrompt := BuildSystemPrompt(e.basePrompt, agents, e.maxDepth)
	userText = messageText(msg)
	if speaker := speakerOf(msg); speaker != "" {
		systemPrompt += "\n\n" + speakerInstruction
		userText = "[speaker: " + speaker + "] " + userText
	}
	return systemPrompt + "\n\n" + userText + "\n", userText
}

// MessageText extracts text content from a message's parts. Exported for the
// scheduler, which reads failure text off async task status messages.
func MessageText(msg *a2a.Message) string {
	return messageText(msg)
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
