package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionConfig configures a per-agent runtime invocation (typically the
// `claude` CLI). Used by ClaudeSession and any other future Session backend
// that needs to spawn an external process.
type SessionConfig struct {
	// Name is a human-readable identifier (typically the agent name) used
	// only for log lines. Optional — empty Name just produces logs without
	// the agent= field. Setting it makes grepping logs by agent trivial
	// when multiple agents share the same scheduler tee.
	Name     string
	Provider string
	Command  string
	Args     []string
	Env      []string
	WorkDir  string
	Timeout  time.Duration
	// WriteAccess is the card-owned filesystem capability. Codex maps it to
	// workspace-write; false remains read-only. It is deliberately a bool so
	// callers cannot request danger-full-access through this config.
	WriteAccess bool
	// NetworkAccess is a card-owned capability. Codex only honors it while
	// running in workspace-write and maps it to the narrow workspace sandbox
	// network override; false keeps outbound network disabled.
	NetworkAccess bool
}

// Validate checks the config for required fields.
func (c SessionConfig) Validate() error {
	if c.Command == "" {
		return errors.New("command is required")
	}
	if c.Timeout < 0 {
		return errors.New("timeout must be non-negative")
	}
	return nil
}

// truncateForLog produces a single-line, length-bounded version of s suitable
// for inclusion in a log message. Newlines are replaced with literal `\n` so
// the log stays grep-friendly; oversize content gets a `…(<N> bytes total)`
// suffix instead of the truncated tail being silently dropped.
func truncateForLog(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("…(%d bytes total)", len(s))
}

// Event is a turn-level event from an agent runtime. Implementations are
// provider-neutral so future Session backends (claude stream-json, codex,
// gemini, ...) can emit the same envelope.
type Event interface{ isEvent() }

// EventText is an assistant text part. When partial-messages streaming is
// disabled (default), the entire assistant text for a turn arrives as a
// single EventText. When partial-messages streaming is enabled, the runtime
// still emits one final EventText carrying the canonical full text — the
// per-chunk increments arrive as EventTextDelta and are not summed into
// EventText (so Turn() can ignore deltas and return the authoritative full
// text without double-counting).
type EventText struct {
	Text string
}

// EventTextDelta is an incremental text chunk emitted while a turn is in
// progress. Only produced when the runtime is configured for partial-message
// streaming (e.g. claude --include-partial-messages). Callers that only need
// the final text should ignore EventTextDelta and read EventText.
type EventTextDelta struct {
	Text string
}

// EventToolUse is an informational signal that the runtime invoked one of
// its built-in tools (Read/Bash/MCP/...). Tool execution is internal to the
// runtime — the wrapper does not need to acknowledge or route results.
type EventToolUse struct {
	// Id is the runtime's tool-use identifier (claude's toolu_*), used to
	// correlate a later tool_result with this call.
	Id    string
	Name  string
	Input json.RawMessage
}

// EventThinking signals the runtime emitted a reasoning/thinking block. It
// carries no payload — the CMA agent.thinking event is a marker only. Only
// produced when the runtime surfaces thinking (e.g. claude with extended
// thinking enabled); dormant otherwise.
type EventThinking struct{}

// EventToolResult is the result of a tool invocation fed back to the model. The
// runtime executes the tool internally; this surfaces the outcome so callers can
// observe it. Content is the result text (polymorphic content is flattened to
// text); ToolUseID correlates with the EventToolUse that produced it.
type EventToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// EventAgentCall is a structured request from the runtime to call another
// ahsir agent. It is the provider-neutral successor to the legacy
// ---A2A_CALL--- text marker. Sessions emit it when their runtime surfaces a
// matching tool-call shape; Executor still falls back to parsing text markers
// for older prompts/providers.
type EventAgentCall struct {
	Agent string
	Task  string
}

// EventTurnDone is the last event delivered before the channel closes.
// Channel closure is the canonical "turn finished" signal; Err carries any
// LLM/runtime error so callers can react without inspecting raw protocol.
type EventTurnDone struct {
	Err   error
	Stats TurnStats
}

// TurnStats reports per-turn cost/usage when available. Zero values are fine
// for backends that don't surface these (e.g. OneshotSession).
type TurnStats struct {
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	DurationMS   int64
}

func (EventText) isEvent()       {}
func (EventTextDelta) isEvent()  {}
func (EventToolUse) isEvent()    {}
func (EventThinking) isEvent()   {}
func (EventToolResult) isEvent() {}
func (EventAgentCall) isEvent()  {}
func (EventTurnDone) isEvent()   {}

// ErrTurnInFlight is returned by Session.Stream when a turn is already
// running on the same session (one contextId = one session = one physically
// serial provider channel). The message doubles as the wire marker: error
// TYPES don't survive the A2A JSON-RPC boundary, so the scheduler gateway
// detects this condition by the literal "agent busy:" substring and maps it
// to HTTP 409. Contract: docs/superpowers/specs/2026-06-07-same-context-busy-semantics.md.
var ErrTurnInFlight = errors.New("agent busy: a turn is already running for this conversation; retry after it completes")

// ErrTurnCanceled marks a turn aborted because its ctx was cancelled — A2A
// tasks/cancel (via the asyncCancels-registered cancel) or the caller/client
// disconnecting. The executor maps this to a CANCELED task state instead of a
// failure so a deliberate interrupt settles cleanly rather than surfacing as an
// error.
var ErrTurnCanceled = errors.New("turn canceled")

// Session is a long-running conversation with one agent runtime instance.
//
// Implementations must serialize Stream calls: the caller is required to
// drain the previous turn's channel (read until close) before calling
// Stream again on the same Session. A violating call fails fast with an
// error wrapping ErrTurnInFlight.
type Session interface {
	// Stream sends one user turn and returns a channel of events. The
	// channel is closed after EventTurnDone is delivered.
	Stream(ctx context.Context, userText string) (<-chan Event, error)

	// Turn blocks until the turn completes and returns aggregated assistant
	// text. It is a convenience helper over Stream for callers that don't
	// need incremental events.
	Turn(ctx context.Context, userText string) (string, error)

	// SessionID returns the runtime's session identifier (e.g. used for
	// claude --resume). Returns "" when the session has no persistent id.
	SessionID() string

	// IsHealthy reports whether this Session can still serve Stream/Turn
	// calls. Returns false once the underlying runtime is gone — for
	// ClaudeSession this means the claude process exited or was killed
	// (stdout EOF). SessionPool consults this on every reuse so a
	// `kill -9`'d claude triggers a transparent recreate-with-resume on
	// the next request rather than serving up a zombie session.
	IsHealthy() bool

	// Close releases resources held by the session. Must be idempotent.
	Close() error
}
