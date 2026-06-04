package wrapper

import (
	"context"
	"encoding/json"
)

// Event is a turn-level event from an agent runtime. Implementations are
// provider-neutral so future Session backends (claude stream-json, codex,
// gemini, ...) can emit the same envelope.
type Event interface{ isEvent() }

// EventText is an assistant text part. In non-streaming-deltas mode (the
// current default) the entire assistant text for a turn arrives as a single
// EventText. When partial-messages streaming is enabled later, multiple
// EventText events may be emitted per turn.
type EventText struct {
	Text string
}

// EventToolUse is an informational signal that the runtime invoked one of
// its built-in tools (Read/Bash/MCP/...). Tool execution is internal to the
// runtime — the wrapper does not need to acknowledge or route results.
type EventToolUse struct {
	Name  string
	Input json.RawMessage
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

func (EventText) isEvent()     {}
func (EventToolUse) isEvent()  {}
func (EventTurnDone) isEvent() {}

// Session is a long-running conversation with one agent runtime instance.
//
// Implementations must serialize Stream calls: the caller is required to
// drain the previous turn's channel (read until close) before calling
// Stream again on the same Session.
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
