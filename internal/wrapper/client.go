package wrapper

import (
	"context"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
)

// AgentClient wraps the SDK's a2aclient.Client for agent-to-agent communication.
type AgentClient struct {
	client *a2aclient.Client
	card   *a2a.AgentCard
}

// noFieldTimeoutHTTPClient is the http.Client we hand to the A2A SDK. We
// deliberately leave Timeout = 0 (no field-level deadline) so the *context*
// timeout passed to SendMessage / GetTask is the single source of truth.
//
// Why this matters: the SDK's NewJSONRPCTransport defaults to
// `&http.Client{Timeout: 3*time.Minute}` if no client is supplied. That
// field-level timeout is independent of any context deadline — whichever
// fires first wins. So even though the scheduler hands a 10-minute context
// to ChatWithAgent, the http.Client would silently terminate the request at
// 3 minutes. Empty-string Timeout disables the ceiling and lets the context
// (set by the caller, e.g. scheduler.ChatWithAgent) be authoritative.
//
// SDK requests use http.NewRequestWithContext, so context cancellation
// already propagates correctly through the transport.
var noFieldTimeoutHTTPClient = &http.Client{}

// NewAgentClient creates a client for communicating with a target agent.
func NewAgentClient(ctx context.Context, card *a2a.AgentCard) (*AgentClient, error) {
	return NewAgentClientWithInternalToken(ctx, card, "")
}

// NewAgentClientWithInternalToken creates a client that attaches the scheduler
// internal token to every A2A call. Empty token means no header is added.
func NewAgentClientWithInternalToken(ctx context.Context, card *a2a.AgentCard, internalToken string) (*AgentClient, error) {
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(noFieldTimeoutHTTPClient),
	)
	if err != nil {
		return nil, fmt.Errorf("create client for %s: %w", card.Name, err)
	}
	if internalToken != "" {
		meta := a2aclient.CallMeta{}
		meta.Append(InternalTokenHeader, internalToken)
		client.AddCallInterceptor(a2aclient.NewStaticCallMetaInjector(meta))
	}
	return &AgentClient{client: client, card: card}, nil
}

// MetadataSpeakerKey is the A2A Message.Metadata key carrying the
// self-claimed speaker identity for shared-context attribution
// (specs/2026-06-08-shared-context-collaboration.md). The value is
// advisory at the current trust level (local machine) — when ingress auth
// lands, the same key carries the verified principal instead.
const MetadataSpeakerKey = "speaker"

// MetadataRequiredFilesystemPathsKey carries an explicit list of filesystem
// inputs that the scheduler must validate against the selected agent card
// before dispatch.
const MetadataRequiredFilesystemPathsKey = "requiredFilesystemPaths"

// SendMessage sends a text message to the agent. contextID, when non-empty,
// is set on the outgoing A2A Message so the callee's SessionPool can route
// the request to an existing session for that contextID. Empty contextID
// means "no conversation continuity" — callee will auto-generate one and
// each call starts a fresh session.
func (c *AgentClient) SendMessage(ctx context.Context, contextID, text string) (string, error) {
	return c.SendMessageWithSpeaker(ctx, contextID, "", text)
}

// SendMessageWithSpeaker is SendMessage plus speaker attribution: a non-empty
// speaker rides Message.Metadata[MetadataSpeakerKey] so the callee's executor
// can tag the turn and its transcript with who said it. Empty speaker keeps
// the wire shape byte-identical to SendMessage (no metadata key at all).
func (c *AgentClient) SendMessageWithSpeaker(ctx context.Context, contextID, speaker, text string) (string, error) {
	return c.SendMessageWithRequirements(ctx, contextID, speaker, text, nil)
}

// SendMessageWithRequirements is SendMessageWithSpeaker plus explicit
// filesystem inputs. Empty requiredPaths keeps the legacy A2A wire shape.
func (c *AgentClient) SendMessageWithRequirements(ctx context.Context, contextID, speaker, text string, requiredPaths []string) (string, error) {
	params := &a2a.MessageSendParams{Message: buildUserMessageWithRequirements(contextID, speaker, text, requiredPaths)}
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

// SendMessageNonBlocking submits a turn with the standard A2A
// configuration.blocking=false: the agent answers immediately with a
// `submitted` task whose progress is polled via GetTask.
func (c *AgentClient) SendMessageNonBlocking(ctx context.Context, contextID, speaker, text string) (*a2a.Task, error) {
	return c.SendMessageNonBlockingWithRequirements(ctx, contextID, speaker, text, nil)
}

func (c *AgentClient) SendMessageNonBlockingWithRequirements(ctx context.Context, contextID, speaker, text string, requiredPaths []string) (*a2a.Task, error) {
	blocking := false
	params := &a2a.MessageSendParams{
		Message: buildUserMessageWithRequirements(contextID, speaker, text, requiredPaths),
		Config:  &a2a.MessageSendConfig{Blocking: &blocking},
	}
	result, err := c.client.SendMessage(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("send message to %s: %w", c.card.Name, err)
	}
	task, ok := result.(*a2a.Task)
	if !ok {
		return nil, fmt.Errorf("non-blocking send to %s: expected a task result, got %T", c.card.Name, result)
	}
	return task, nil
}

// buildUserMessage assembles the outbound A2A user message shared by the
// blocking and non-blocking send paths.
func buildUserMessage(contextID, speaker, text string) *a2a.Message {
	return buildUserMessageWithRequirements(contextID, speaker, text, nil)
}

func buildUserMessageWithRequirements(contextID, speaker, text string, requiredPaths []string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: text})
	if contextID != "" {
		msg.ContextID = contextID
	}
	if speaker != "" {
		msg.Metadata = map[string]any{MetadataSpeakerKey: speaker}
	}
	if len(requiredPaths) > 0 {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata[MetadataRequiredFilesystemPathsKey] = append([]string(nil), requiredPaths...)
	}
	return msg
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
