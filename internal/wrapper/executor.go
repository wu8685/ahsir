package wrapper

import (
	"context"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-go/a2a"
)

// ExecutorConfig configures the agent execution loop.
type ExecutorConfig struct {
	// OpenSession returns the Session to use for the current request. The
	// contextID is the A2A contextID, which Step 2 uses to look up a long-
	// running ClaudeSession from a pool; Step 1's OneshotSession backend
	// ignores it. Implementations must surface non-zero exits / stderr via
	// Session.Turn's returned error rather than returning ("", nil), which
	// would silently produce an empty agent reply.
	OpenSession func(ctx context.Context, contextID string) (Session, error)
	ListAgents  func() []*a2a.AgentCard
	CallAgent   func(ctx context.Context, agentName string, task string) (string, error)
	// LookupHistory returns prior tasks belonging to a contextID, in
	// chronological order. Used to give the underlying LLM short-term memory
	// across separate message/send calls. May be nil — in that case each
	// request starts fresh.
	LookupHistory func(contextID string) []*a2a.Task
	MaxDepth      int
	BasePrompt    string
}

// Executor runs the core agent loop: receive message → prompt the LLM →
// parse A2A_CALL markers → execute sub-agent calls → inject results → return task.
type Executor struct {
	openSession   func(ctx context.Context, contextID string) (Session, error)
	listAgents    func() []*a2a.AgentCard
	callAgent     func(ctx context.Context, agentName string, task string) (string, error)
	lookupHistory func(contextID string) []*a2a.Task
	maxDepth      int
	basePrompt    string
}

// NewExecutor creates a new Executor.
// If MaxDepth is not set (0), it defaults to 5.
func NewExecutor(cfg ExecutorConfig) *Executor {
	return &Executor{
		openSession:   cfg.OpenSession,
		listAgents:    cfg.ListAgents,
		callAgent:     cfg.CallAgent,
		lookupHistory: cfg.LookupHistory,
		maxDepth:      cfg.MaxDepth,
		basePrompt:    cfg.BasePrompt,
	}
}

// Execute runs the agent execution loop for an incoming message.
func (e *Executor) Execute(ctx context.Context, msg *a2a.Message) (*a2a.Task, error) {
	// Pass msg as the TaskInfoProvider so msg.ContextID propagates onto the
	// new task — this is what links sequential message/send calls into one
	// conversation. NewSubmittedTask generates a fresh ContextID only if
	// msg.ContextID is empty.
	task := a2a.NewSubmittedTask(msg, msg)

	// Open a Session for this request. Step 1's OneshotSession is per-request
	// (defer Close); Step 2 will pool sessions by contextID and the defer
	// becomes a no-op return-to-pool.
	session, err := e.openSession(ctx, task.ContextID)
	if err != nil {
		task.Status = a2a.TaskStatus{State: a2a.TaskStateFailed}
		return task, err
	}
	defer session.Close()

	// Build the full system prompt with available agents
	agents := e.listAgents()
	systemPrompt := BuildSystemPrompt(e.basePrompt, agents, e.maxDepth)

	// Extract user text
	userText := messageText(msg)

	// Replay prior conversation in this context, if any.
	historyText := ""
	if e.lookupHistory != nil {
		prior := e.lookupHistory(task.ContextID)
		historyText = formatPriorHistory(prior)
	}

	// Compose initial prompt: system + prior conversation + new user turn.
	var sb strings.Builder
	sb.WriteString(systemPrompt)
	if historyText != "" {
		sb.WriteString("\n\n")
		sb.WriteString(historyText)
	}
	sb.WriteString("\n\n")
	sb.WriteString(userText)
	sb.WriteString("\n")
	fullPrompt := sb.String()

	// Run the agent interaction loop
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

// formatPriorHistory renders prior tasks in the same context as a
// conversation transcript that can be prepended to a new prompt. The format
// is intentionally simple text — provider-agnostic, no JSON or special
// tokens — so any LLM CLI configured via RuntimeConfig can consume it.
func formatPriorHistory(prior []*a2a.Task) string {
	if len(prior) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Conversation so far (prior turns in this context):\n")
	for _, t := range prior {
		for _, m := range t.History {
			role := "user"
			if m.Role == a2a.MessageRoleAgent {
				role = "assistant"
			}
			text := ""
			for _, p := range m.Parts {
				if tp, ok := p.(a2a.TextPart); ok {
					text = tp.Text
					break
				}
			}
			if text == "" {
				continue
			}
			sb.WriteString(fmt.Sprintf("- %s: %s\n", role, strings.TrimSpace(text)))
		}
	}
	return sb.String()
}

// interact runs the recursive agent interaction loop. The session is threaded
// through recursion so all turns in one Execute share the same Session
// instance — Step 1 this just means the same OneshotSession (fresh process
// per Turn anyway); Step 2 will let ClaudeSession's long-running process
// span all turns and sub-agent injection rounds.
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

	// Execute the sub-agent call
	agentResult, err := e.callAgent(ctx, call.Agent, call.Task)
	if err != nil {
		errorMsg := fmt.Sprintf("\n[Agent %s call failed: %v]\n", call.Agent, err)
		return e.interact(ctx, session, task, errorMsg, depth, originalTask)
	}

	// Inject the result and continue
	injection := BuildInjectionPrompt(call.Agent, originalTask, agentResult)
	return e.interact(ctx, session, task, injection, depth+1, originalTask)
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
