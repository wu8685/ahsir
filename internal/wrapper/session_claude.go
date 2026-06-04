package wrapper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
)

// Compile-time assertion that ClaudeSession satisfies the Session interface.
var _ Session = (*ClaudeSession)(nil)

// ClaudeSession is a long-running conversation backed by a single `claude -p
// --input-format stream-json` process. Conversation history is held by the
// claude process; the wrapper just shuttles user turns in and emits parsed
// events out.
//
// Protocol caveat: `--input-format stream-json` has no officially documented
// schema (GH#24594). Empirically claude does NOT emit any events on its own
// after startup — it waits for the first user message on stdin, then emits
// hook noise, system/init, the assistant turn, and the result. Session_id
// is therefore captured opportunistically during the first turn, not via a
// construction-time handshake. The parser is also intentionally lenient —
// unknown event types and unknown fields are dropped silently rather than
// erroring, so a future claude version that adds new event types doesn't
// crash production.
type ClaudeSession struct {
	transport *claudeTransport
	cfg       SessionConfig
	resumeID  string // expected session_id when --resume is in effect

	mu        sync.Mutex
	state     sessionState
	sessionID string
	resumeErr error      // surfaces on next EventTurnDone if init returns mismatched id
	eventsCh  chan Event // non-nil only while a turn is in flight
	// onSessionIDKnown is fired once, from handleInit, when sessionID
	// transitions from "" to the value reported by claude. Used by
	// SessionPool to persist (contextID → sessionID) only AFTER the real id
	// is known — previously the pool froze an empty value at construction
	// and never updated it. The callback runs from the reader goroutine, so
	// implementations must be cheap and non-blocking.
	onSessionIDKnown func(string)

	readerDone chan struct{}
}

// claudeTransport abstracts the OS-level subprocess IO so tests can inject
// io.Pipe stubs without involving exec. wait blocks until the underlying
// process exits; kill terminates it and must be idempotent.
type claudeTransport struct {
	stdin  io.WriteCloser
	stdout io.Reader
	wait   func() error
	kill   func() error
}

// transportFactory builds a fresh claudeTransport. resumeID, when non-empty,
// is passed as --resume to the spawned claude — used by SessionPool to
// re-hydrate an EVICTED session.
type transportFactory func(ctx context.Context, resumeID string) (*claudeTransport, error)

type sessionState int

const (
	stateReady sessionState = iota
	stateInFlight
	stateEvicted
	stateClosed
)


// NewClaudeSession spawns a real `claude` subprocess in stream-json mode
// and returns a ready Session. resumeID, when non-empty, is passed as
// --resume so claude rehydrates the prior conversation from its local
// session store (~/.claude/projects/).
//
// The cfg's Args field is sanitized — any user-supplied -p / --input-format
// / --output-format are stripped and replaced with the canonical stream-json
// trio. Other args (e.g. --add-dir, --allowedTools) are preserved.
func NewClaudeSession(ctx context.Context, cfg SessionConfig, resumeID string) (*ClaudeSession, error) {
	tr, err := buildProductionTransport(ctx, cfg, resumeID)
	if err != nil {
		return nil, err
	}
	return newClaudeSessionWithTransport(ctx, cfg, resumeID, tr)
}

// buildProductionTransport spawns claude with stream-json IO and wires its
// stdin/stdout pipes into a claudeTransport. stderr is drained to log so a
// stuck pipe never backpressures the subprocess.
func buildProductionTransport(ctx context.Context, cfg SessionConfig, resumeID string) (*claudeTransport, error) {
	args := injectStreamJSONFlags(append([]string(nil), cfg.Args...))
	if resumeID != "" {
		args = append(args, "--resume="+resumeID)
	}

	cmd := exec.CommandContext(ctx, cfg.Command, args...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	if len(cfg.Env) > 0 {
		cmd.Env = cfg.Env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude start: %w", err)
	}

	log.Printf("claude session: started pid=%d cmd=%s args=%v", cmd.Process.Pid, cfg.Command, args)

	// Drain stderr in background so pipe buffer never fills; surface a
	// preview if claude writes anything (auth failures / hook crashes /
	// deprecation notes typically land here).
	go drainClaudeStderr(cfg.Name, stderr)

	// Reap the process eventually so we don't leak zombies. The reader
	// goroutine sees stdout EOF independently and signals EVICTED via the
	// usual path.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var killOnce sync.Once
	killFn := func() error {
		var killErr error
		killOnce.Do(func() {
			if cmd.Process != nil {
				killErr = cmd.Process.Kill()
			}
		})
		return killErr
	}

	return &claudeTransport{
		stdin:  stdin,
		stdout: stdout,
		wait:   func() error { return <-waitCh },
		kill:   killFn,
	}, nil
}

// injectStreamJSONFlags strips any conflicting user-supplied -p /
// --input-format / --output-format from args and appends the canonical
// stream-json trio. Other flags (--add-dir, --allowedTools, ...) pass
// through unchanged.
func injectStreamJSONFlags(args []string) []string {
	filtered := args[:0]
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		switch {
		case a == "-p", a == "--print":
			continue
		case a == "--input-format", a == "--output-format":
			skipNext = true // user gave "--input-format stream-json" style; drop both tokens
			continue
		case strings.HasPrefix(a, "--input-format="), strings.HasPrefix(a, "--output-format="):
			continue
		}
		filtered = append(filtered, a)
	}
	return append(filtered,
		"-p",
		"--input-format=stream-json",
		"--output-format=stream-json",
		"--verbose",
	)
}

// drainClaudeStderr reads stderr to EOF, logging the content in chunks. The
// reader exits when the pipe closes (process death).
func drainClaudeStderr(name string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 8*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if name != "" {
			log.Printf("claude session [%s] stderr: %s", name, line)
		} else {
			log.Printf("claude session stderr: %s", line)
		}
	}
}


// newClaudeSessionWithTransport is the test seam: callers (production code
// or tests) provide a ready-to-read claudeTransport. Construction returns
// immediately; the session is READY to accept Stream calls. The first
// Stream's response stream is what triggers claude's system/init emission;
// session_id is captured there, not here.
func newClaudeSessionWithTransport(ctx context.Context, cfg SessionConfig, resumeID string, tr *claudeTransport) (*ClaudeSession, error) {
	s := &ClaudeSession{
		transport:  tr,
		cfg:        cfg,
		resumeID:   resumeID,
		state:      stateReady,
		readerDone: make(chan struct{}),
	}
	go s.reader()
	return s, nil
}

// SessionID returns the session_id captured from claude's first system/init
// event. Empty until the first Stream call completes (or at least progresses
// far enough for init to arrive on the event stream).
func (s *ClaudeSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// IsHealthy reports false once the underlying claude process is gone —
// either explicitly Close()'d or surfaced via reader-goroutine EOF
// (handleEOF transitions state to stateEvicted on any unexpected stdout
// close, e.g. SIGKILL). Pool uses this to detect zombies in the hot path
// and trigger transparent recreation with `--resume`.
func (s *ClaudeSession) IsHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state != stateEvicted && s.state != stateClosed
}

// OnSessionIDKnown registers a callback fired once when claude's first init
// event delivers a real session_id. If the id has already arrived by the
// time OnSessionIDKnown is called, the callback fires synchronously with
// the current value. Re-registering replaces the previous callback. Used by
// SessionPool to persist the id only when it's actually known.
func (s *ClaudeSession) OnSessionIDKnown(fn func(string)) {
	s.mu.Lock()
	sid := s.sessionID
	s.onSessionIDKnown = fn
	s.mu.Unlock()
	if sid != "" && fn != nil {
		fn(sid)
	}
}

// Stream sends one user turn over stdin and returns a channel of events.
// The channel closes after EventTurnDone is delivered. Callers must drain
// the channel before calling Stream again (concurrent calls return error).
func (s *ClaudeSession) Stream(ctx context.Context, userText string) (<-chan Event, error) {
	s.mu.Lock()
	switch s.state {
	case stateInFlight:
		s.mu.Unlock()
		return nil, errors.New("claude session: previous turn not drained")
	case stateClosed, stateEvicted:
		s.mu.Unlock()
		return nil, fmt.Errorf("claude session: not ready (state=%d)", s.state)
	}
	ch := make(chan Event, 16)
	s.eventsCh = ch
	s.state = stateInFlight
	sessionID := s.sessionID
	s.mu.Unlock()

	payload, err := json.Marshal(map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": userText,
		},
		"session_id":         sessionID,
		"parent_tool_use_id": nil,
	})
	if err != nil {
		s.failTurn(fmt.Errorf("marshal user message: %w", err))
		return nil, err
	}
	payload = append(payload, '\n')

	if _, err := s.transport.stdin.Write(payload); err != nil {
		s.failTurn(fmt.Errorf("write user message: %w", err))
		return nil, err
	}
	return ch, nil
}

// Turn drains Stream and aggregates EventText into a single string. Partial
// text accumulated before an error is preserved in the returned string.
func (s *ClaudeSession) Turn(ctx context.Context, userText string) (string, error) {
	ch, err := s.Stream(ctx, userText)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	var doneErr error
	for ev := range ch {
		switch e := ev.(type) {
		case EventText:
			sb.WriteString(e.Text)
		case EventTurnDone:
			doneErr = e.Err
		}
	}
	return sb.String(), doneErr
}

// Close kills the underlying process and waits for the reader goroutine to
// exit. Idempotent.
func (s *ClaudeSession) Close() error {
	s.mu.Lock()
	if s.state == stateClosed {
		s.mu.Unlock()
		return nil
	}
	s.state = stateClosed
	s.mu.Unlock()
	_ = s.transport.kill()
	<-s.readerDone
	return nil
}

// reader is the single goroutine that owns transport.stdout. It parses
// NDJSON events and dispatches them per state.
func (s *ClaudeSession) reader() {
	defer close(s.readerDone)
	scanner := bufio.NewScanner(s.transport.stdout)
	// Default Scanner buffer (64KB) is too small for assistant messages with
	// large tool inputs. Bump generously.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		var env protocolEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue // malformed line — drop
		}
		s.dispatchEvent(env, line)
	}
	s.handleEOF(scanner.Err())
}

// protocolEnvelope is the minimal shape we extract from every NDJSON line to
// route it. The full payload is re-decoded into typed structs inside
// dispatchEvent.
type protocolEnvelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

func (s *ClaudeSession) dispatchEvent(env protocolEnvelope, raw []byte) {
	if env.Type == "system" {
		switch env.Subtype {
		case "init":
			s.handleInit(env)
		case "hook_started", "hook_response":
			// startup hook noise — drop. Real-world traces show several
			// of these arrive before init even on a clean run.
		}
		return
	}

	s.mu.Lock()
	ch := s.eventsCh
	s.mu.Unlock()
	if ch == nil {
		// Event arrived between turns — drop. Shouldn't happen in a
		// well-behaved claude run, but staying defensive against future
		// protocol additions.
		return
	}

	switch env.Type {
	case "assistant":
		s.emitAssistant(ch, raw)
	case "result":
		s.emitResult(ch, raw)
	}
}

func (s *ClaudeSession) handleInit(env protocolEnvelope) {
	s.mu.Lock()
	if s.sessionID != "" {
		s.mu.Unlock()
		return // already captured; subsequent inits (e.g. re-init mid-stream) are ignored
	}
	s.sessionID = env.SessionID
	// When resuming, claude is supposed to echo back the same session_id we
	// asked for via --resume. A mismatch means the resume failed silently
	// (claude rotated the id) and the prior conversation context is gone —
	// surface it on EventTurnDone so the caller can react.
	if s.resumeID != "" && s.resumeID != env.SessionID {
		s.resumeErr = fmt.Errorf("claude resume mismatch: got session_id %q, want %q", env.SessionID, s.resumeID)
	}
	cb := s.onSessionIDKnown
	sid := s.sessionID
	s.mu.Unlock()
	// Fire callback OUTSIDE s.mu so it can do further work (e.g. acquire
	// pool-level locks) without risking nested-lock surprises.
	if cb != nil {
		cb(sid)
	}
}

func (s *ClaudeSession) emitAssistant(ch chan<- Event, raw []byte) {
	var msg struct {
		Message struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return
	}
	for _, part := range msg.Message.Content {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(part, &head); err != nil {
			continue
		}
		switch head.Type {
		case "text":
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(part, &p); err == nil {
				ch <- EventText{Text: p.Text}
			}
		case "tool_use":
			var p struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(part, &p); err == nil {
				ch <- EventToolUse{Name: p.Name, Input: p.Input}
			}
		}
	}
}

func (s *ClaudeSession) emitResult(ch chan<- Event, raw []byte) {
	var r struct {
		IsError        bool   `json:"is_error"`
		Result         string `json:"result"`
		APIErrorStatus int    `json:"api_error_status"`
		DurationMS     int64  `json:"duration_ms"`
		TotalCostUSD   float64 `json:"total_cost_usd"`
		Usage          struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &r)
	var err error
	if r.IsError {
		// result.subtype=="success" can coexist with is_error==true; the
		// authoritative success flag is is_error.
		if r.Result != "" {
			err = fmt.Errorf("claude turn failed: %s (api_status=%d)", r.Result, r.APIErrorStatus)
		} else {
			err = fmt.Errorf("claude turn failed (api_status=%d)", r.APIErrorStatus)
		}
	}
	// Resume mismatch trumps a clean result — the context is wrong even if
	// the LLM returned a plausible-looking answer.
	s.mu.Lock()
	if err == nil && s.resumeErr != nil {
		err = s.resumeErr
	}
	s.mu.Unlock()
	ch <- EventTurnDone{
		Err: err,
		Stats: TurnStats{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
			CostUSD:      r.TotalCostUSD,
			DurationMS:   r.DurationMS,
		},
	}
	s.endTurn()
}

func (s *ClaudeSession) endTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eventsCh != nil {
		close(s.eventsCh)
		s.eventsCh = nil
	}
	if s.state == stateInFlight {
		s.state = stateReady
	}
}

// failTurn aborts an in-flight turn synchronously (used when stdin write
// itself fails — the reader goroutine never gets to see the corresponding
// events).
func (s *ClaudeSession) failTurn(err error) {
	s.mu.Lock()
	ch := s.eventsCh
	s.eventsCh = nil
	if s.state == stateInFlight {
		s.state = stateEvicted
	}
	s.mu.Unlock()
	if ch != nil {
		ch <- EventTurnDone{Err: err}
		close(ch)
	}
}

// handleEOF fires when the reader's scanner exits (process died or stdout
// closed). If a turn was in flight, it gets a synthetic EventTurnDone with
// the EOF/scanner error; state transitions to EVICTED so the pool can
// resume on the next access.
func (s *ClaudeSession) handleEOF(scanErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == stateClosed {
		return // intentional Close — nothing to signal
	}
	ch := s.eventsCh
	s.eventsCh = nil
	if ch != nil {
		eofErr := errors.New("claude process exited mid-turn")
		if scanErr != nil {
			eofErr = fmt.Errorf("claude process exited mid-turn: %w", scanErr)
		}
		ch <- EventTurnDone{Err: eofErr}
		close(ch)
	}
	s.state = stateEvicted
}
