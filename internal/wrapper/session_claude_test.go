package wrapper

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// stdinSink is a thread-safe io.WriteCloser used in place of io.Pipe for the
// fake transport's stdin side. The session writes here synchronously — when
// Write returns the bytes are visible to subsequent String() calls, no
// asynchronous drainer goroutine needed. The previous io.Pipe + drainer
// design had a race window between pipe.Read returning and the drainer's
// buffer append, which made TestClaudeSession_StdinIncludesSessionID flake
// under load.
type stdinSink struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (s *stdinSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.buf.Write(p)
}

func (s *stdinSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *stdinSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// fakeClaudeTransport is an io.Pipe-based stub for stdout (so tests can stream
// NDJSON events line by line) plus a buffered sink for stdin (so writes are
// race-free observable via stdinBytes()).
type fakeClaudeTransport struct {
	stdoutW *io.PipeWriter
	stdin   *stdinSink
	tr      *claudeTransport

	once   sync.Once
	waitCh chan struct{}
}

func newFakeClaudeTransport() *fakeClaudeTransport {
	stdoutR, stdoutW := io.Pipe()
	sink := &stdinSink{}
	f := &fakeClaudeTransport{
		stdoutW: stdoutW,
		stdin:   sink,
		waitCh:  make(chan struct{}),
	}
	f.tr = &claudeTransport{
		stdin:  sink,
		stdout: stdoutR,
		wait:   func() error { <-f.waitCh; return nil },
		kill: func() error {
			f.once.Do(func() {
				_ = sink.Close()
				_ = stdoutW.Close()
				_ = stdoutR.Close()
				close(f.waitCh)
			})
			return nil
		},
	}
	return f
}

func (f *fakeClaudeTransport) writeLine(t *testing.T, line string) {
	t.Helper()
	if _, err := f.stdoutW.Write([]byte(line + "\n")); err != nil {
		// io.ErrClosedPipe after close() is normal in cleanup paths.
		if err != io.ErrClosedPipe {
			t.Logf("writeLine: %v", err)
		}
	}
}

func (f *fakeClaudeTransport) stdinBytes() string {
	return f.stdin.String()
}

func (f *fakeClaudeTransport) close() { _ = f.tr.kill() }

// Canned NDJSON event helpers.
func initEvent(sessionID string) string {
	return `{"type":"system","subtype":"init","session_id":"` + sessionID + `","cwd":"/tmp","tools":[],"mcp_servers":[]}`
}

func resultOK(sessionID string) string {
	return `{"type":"result","subtype":"success","is_error":false,"session_id":"` + sessionID + `"}`
}

func resultError(sessionID, msg string) string {
	return `{"type":"result","subtype":"success","is_error":true,"api_error_status":400,"result":"` + msg + `","session_id":"` + sessionID + `"}`
}

func assistantText(text string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}`
}

func assistantToolUse(name, inputJSON string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"` + name + `","input":` + inputJSON + `}]}}`
}

// --- injectStreamJSONFlags args sanitization ---

func TestInjectStreamJSONFlags_StripsUserConflicts(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "empty",
			in:   nil,
			want: []string{"-p", "--input-format=stream-json", "--output-format=stream-json", "--verbose"},
		},
		{
			name: "strips legacy -p --output-format text",
			in:   []string{"-p", "--output-format", "text"},
			want: []string{"-p", "--input-format=stream-json", "--output-format=stream-json", "--verbose"},
		},
		{
			name: "strips --print equiv-eq",
			in:   []string{"--print", "--output-format=text"},
			want: []string{"-p", "--input-format=stream-json", "--output-format=stream-json", "--verbose"},
		},
		{
			name: "preserves --add-dir / --allowedTools",
			in:   []string{"--add-dir=/tmp", "--allowedTools=Read,LS", "-p"},
			want: []string{"--add-dir=/tmp", "--allowedTools=Read,LS", "-p", "--input-format=stream-json", "--output-format=stream-json", "--verbose"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := injectStreamJSONFlags(append([]string(nil), tc.in...))
			if !equalStrings(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
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

// --- Red 1: single-turn protocol parsing ---

// TestClaudeSession_ParsesInitSessionID verifies that the session_id is
// captured from the first system/init event that arrives during the first
// turn. (Claude only emits init after stdin receives a user message — see
// the package doc on ClaudeSession.)
func TestClaudeSession_ParsesInitSessionID(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.writeLine(t, initEvent("sess-init"))
		f.writeLine(t, resultOK("sess-init"))
	}()
	for range ch {
	}

	if s.SessionID() != "sess-init" {
		t.Errorf("want sessionID 'sess-init', got %q", s.SessionID())
	}
}

func TestClaudeSession_DropsHookNoise(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	hookStarted := `{"type":"system","subtype":"hook_started","hook_id":"x","hook_name":"SessionStart:startup","session_id":""}`
	hookResponse := `{"type":"system","subtype":"hook_response","hook_id":"x","hook_name":"SessionStart:startup","exit_code":0,"outcome":"success","session_id":""}`
	go func() {
		f.writeLine(t, hookStarted)
		f.writeLine(t, hookResponse)
		f.writeLine(t, initEvent("sess-noise"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "ping")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		// Even mid-turn hook events must be dropped.
		f.writeLine(t, hookStarted)
		f.writeLine(t, resultOK("sess-noise"))
	}()

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event (turn-done only), got %d: %+v", len(got), got)
	}
	if _, ok := got[0].(EventTurnDone); !ok {
		t.Errorf("want EventTurnDone, got %T", got[0])
	}
}

func TestClaudeSession_AssistantTextEmitsEvent(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go f.writeLine(t, initEvent("sess-text"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.writeLine(t, assistantText("hello world"))
		f.writeLine(t, resultOK("sess-text"))
	}()

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(got), got)
	}
	tx, ok := got[0].(EventText)
	if !ok || tx.Text != "hello world" {
		t.Errorf("want EventText('hello world'), got %T %+v", got[0], got[0])
	}
	td, ok := got[1].(EventTurnDone)
	if !ok || td.Err != nil {
		t.Errorf("want EventTurnDone(nil), got %T %+v", got[1], got[1])
	}
}

func TestClaudeSession_AssistantToolUseEmitsEvent(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go f.writeLine(t, initEvent("sess-tool"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "list files")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.writeLine(t, assistantToolUse("Bash", `{"command":"ls"}`))
		f.writeLine(t, resultOK("sess-tool"))
	}()

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(got), got)
	}
	tu, ok := got[0].(EventToolUse)
	if !ok || tu.Name != "Bash" {
		t.Errorf("want EventToolUse(Bash), got %T %+v", got[0], got[0])
	}
	if string(tu.Input) != `{"command":"ls"}` {
		t.Errorf("want input {\"command\":\"ls\"}, got %s", tu.Input)
	}
}

func TestClaudeSession_AssistantMixedTextAndToolUse(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go f.writeLine(t, initEvent("sess-mixed"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "do stuff")
	if err != nil {
		t.Fatal(err)
	}
	mixed := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"sure"},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}},{"type":"text","text":"done"}]}}`
	go func() {
		f.writeLine(t, mixed)
		f.writeLine(t, resultOK("sess-mixed"))
	}()

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 events, got %d: %+v", len(got), got)
	}
	if t1, ok := got[0].(EventText); !ok || t1.Text != "sure" {
		t.Errorf("want EventText('sure') at [0], got %T %+v", got[0], got[0])
	}
	if _, ok := got[1].(EventToolUse); !ok {
		t.Errorf("want EventToolUse at [1], got %T", got[1])
	}
	if t2, ok := got[2].(EventText); !ok || t2.Text != "done" {
		t.Errorf("want EventText('done') at [2], got %T %+v", got[2], got[2])
	}
	if _, ok := got[3].(EventTurnDone); !ok {
		t.Errorf("want EventTurnDone at [3], got %T", got[3])
	}
}

func TestClaudeSession_ResultIsErrorPropagated(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go f.writeLine(t, initEvent("sess-err"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "ping")
	if err != nil {
		t.Fatal(err)
	}
	go f.writeLine(t, resultError("sess-err", "Credit balance is too low"))

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	td, ok := got[0].(EventTurnDone)
	if !ok {
		t.Fatalf("want EventTurnDone, got %T", got[0])
	}
	if td.Err == nil {
		t.Error("want non-nil Err on result.is_error=true")
	} else if !strings.Contains(td.Err.Error(), "Credit balance is too low") {
		t.Errorf("want Err to contain result.result text, got %v", td.Err)
	}
}

func TestClaudeSession_MalformedJSONIgnored(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go func() {
		f.writeLine(t, `not json at all`)
		f.writeLine(t, `{partial`)
		f.writeLine(t, initEvent("sess-mal"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatalf("init should survive preceding malformed lines: %v", err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "ping")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.writeLine(t, `garbage line`)
		f.writeLine(t, assistantText("ok"))
		f.writeLine(t, resultOK("sess-mal"))
	}()

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events (text + done), got %d: %+v", len(got), got)
	}
}

// --- Red 2: multi-turn state machine ---

func TestClaudeSession_StreamSerial_TwoTurns(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go f.writeLine(t, initEvent("sess-multi"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Turn 1
	ch1, err := s.Stream(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.writeLine(t, assistantText("turn-1-reply"))
		f.writeLine(t, resultOK("sess-multi"))
	}()
	var got1 []Event
	for ev := range ch1 {
		got1 = append(got1, ev)
	}
	if len(got1) != 2 {
		t.Fatalf("turn 1: want 2 events, got %d", len(got1))
	}

	// Turn 2 — must be allowed on the same session.
	ch2, err := s.Stream(ctx, "second")
	if err != nil {
		t.Fatalf("turn 2 stream: %v", err)
	}
	go func() {
		f.writeLine(t, assistantText("turn-2-reply"))
		f.writeLine(t, resultOK("sess-multi"))
	}()
	var got2 []Event
	for ev := range ch2 {
		got2 = append(got2, ev)
	}
	if len(got2) != 2 {
		t.Fatalf("turn 2: want 2 events, got %d", len(got2))
	}
	if tx, ok := got2[0].(EventText); !ok || tx.Text != "turn-2-reply" {
		t.Errorf("turn 2: want text 'turn-2-reply', got %+v", got2[0])
	}
}

func TestClaudeSession_StreamRejectsConcurrent(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go f.writeLine(t, initEvent("sess-conc"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.Stream(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	// Don't drain — turn is in flight.
	if _, err := s.Stream(ctx, "second"); err == nil {
		t.Error("want error on concurrent Stream, got nil")
	}
}

// --- Red 3: crash recovery and resume ---

// TestClaudeSession_MidTurnEOF_FailsTurnAndEvicts simulates the claude
// process exiting before a result event arrives. The in-flight turn must
// receive a synthetic EventTurnDone with the EOF error and the session
// must transition out of READY so the pool can spawn a replacement on the
// next access. sessionID is preserved across the failure so the pool can
// pass it as --resume.
func TestClaudeSession_MidTurnEOF_FailsTurnAndEvicts(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "ping")
	if err != nil {
		t.Fatal(err)
	}
	// Write init so sessionID is captured, then close stdout to simulate
	// the process dying before emitting a result.
	go func() {
		f.writeLine(t, initEvent("sess-eof"))
		_ = f.stdoutW.Close()
	}()

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event (turn-done), got %d: %+v", len(got), got)
	}
	td, ok := got[0].(EventTurnDone)
	if !ok || td.Err == nil {
		t.Fatalf("want EventTurnDone with non-nil Err, got %T %+v", got[0], got[0])
	}

	if _, err := s.Stream(ctx, "next"); err == nil {
		t.Error("want Stream to error after EOF (state should be EVICTED), got nil")
	}

	if s.SessionID() != "sess-eof" {
		t.Errorf("want sessionID preserved as 'sess-eof', got %q", s.SessionID())
	}
}

// TestClaudeSession_ResumeMatchOK verifies that supplying a matching
// resumeID does not surface a mismatch error during the turn.
func TestClaudeSession_ResumeMatchOK(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "sess-resume", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.writeLine(t, initEvent("sess-resume"))
		f.writeLine(t, resultOK("sess-resume"))
	}()

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	if td, ok := got[len(got)-1].(EventTurnDone); !ok || td.Err != nil {
		t.Errorf("matching resume should not surface error, got %+v", got[len(got)-1])
	}
}

// TestClaudeSession_ResumeMismatchFails verifies that supplying a
// resumeID that doesn't match the init event's session_id surfaces an
// error via EventTurnDone — protects against re-attaching to the wrong
// conversation if claude rotates session ids on us. Init now arrives in
// the event stream of the first turn, so the mismatch is detected there.
func TestClaudeSession_ResumeMismatchFails(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "expected-id", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.writeLine(t, initEvent("actual-id"))
		f.writeLine(t, resultOK("actual-id"))
	}()

	var got []Event
	for ev := range ch {
		got = append(got, ev)
	}
	td, ok := got[len(got)-1].(EventTurnDone)
	if !ok || td.Err == nil {
		t.Fatalf("want EventTurnDone with mismatch error, got %T %+v", got[len(got)-1], got[len(got)-1])
	}
	if !strings.Contains(td.Err.Error(), "resume") {
		t.Errorf("want error mentioning 'resume', got %v", td.Err)
	}
}

// TestClaudeSession_StdinIncludesSessionID verifies the wire format of user
// messages written to stdin. The first turn carries an empty session_id
// (claude hasn't told us its id yet); the second turn picks up the id that
// arrived via init during the first turn — this is the linchpin of multi-
// turn continuity over the stream-json protocol.
func TestClaudeSession_StdinIncludesSessionID(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Turn 1: send user message, then have claude reply with init+result.
	ch1, err := s.Stream(ctx, "ping")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		f.writeLine(t, initEvent("sess-stdin"))
		f.writeLine(t, resultOK("sess-stdin"))
	}()
	for range ch1 {
	}

	// Turn 2: now session_id should be in stdin payload.
	ch2, err := s.Stream(ctx, "again")
	if err != nil {
		t.Fatal(err)
	}
	go f.writeLine(t, resultOK("sess-stdin"))
	for range ch2 {
	}

	raw := f.stdinBytes()
	if !strings.Contains(raw, `"role":"user"`) {
		t.Errorf("stdin missing role:user, got: %s", raw)
	}
	if !strings.Contains(raw, `"ping"`) {
		t.Errorf("stdin missing turn-1 text 'ping', got: %s", raw)
	}
	if !strings.Contains(raw, `"again"`) {
		t.Errorf("stdin missing turn-2 text 'again', got: %s", raw)
	}
	if !strings.Contains(raw, `"session_id":"sess-stdin"`) {
		t.Errorf("stdin missing session_id on turn-2, got: %s", raw)
	}
}

// TestClaudeSession_StreamEventDeltas verifies that stream_event lines (emitted
// when claude is run with --include-partial-messages) surface as
// EventTextDelta while the canonical final EventText still arrives from the
// trailing `type:assistant` event. Non-text stream_event payloads
// (content_block_start / message_delta) must be dropped.
func TestClaudeSession_StreamEventDeltas(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go f.writeLine(t, initEvent("sess-delta"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ch, err := s.Stream(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}

	// Synthetic protocol shape: a content_block_start (no text), three text
	// deltas, then a stop frame, then the canonical assistant event, then
	// result. Mirrors what claude emits with --include-partial-messages.
	go func() {
		f.writeLine(t, `{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"text"}},"session_id":"sess-delta"}`)
		f.writeLine(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}},"session_id":"sess-delta"}`)
		f.writeLine(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo "}},"session_id":"sess-delta"}`)
		f.writeLine(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"world"}},"session_id":"sess-delta"}`)
		f.writeLine(t, `{"type":"stream_event","event":{"type":"content_block_stop"},"session_id":"sess-delta"}`)
		f.writeLine(t, assistantText("Hello world"))
		f.writeLine(t, resultOK("sess-delta"))
	}()

	var deltas []string
	var full string
	var doneSeen bool
	for ev := range ch {
		switch e := ev.(type) {
		case EventTextDelta:
			deltas = append(deltas, e.Text)
		case EventText:
			full = e.Text
		case EventTurnDone:
			doneSeen = true
			if e.Err != nil {
				t.Errorf("turn done err: %v", e.Err)
			}
		}
	}
	if !doneSeen {
		t.Fatal("EventTurnDone not delivered")
	}
	wantDeltas := []string{"Hel", "lo ", "world"}
	if !equalStrings(deltas, wantDeltas) {
		t.Errorf("want deltas %v, got %v", wantDeltas, deltas)
	}
	if full != "Hello world" {
		t.Errorf("want final EventText='Hello world', got %q", full)
	}
}

// TestClaudeSession_TurnIgnoresDeltas ensures Turn() returns only the final
// EventText concatenation — deltas are dropped to keep non-streaming callers
// from receiving doubled text when --include-partial-messages is enabled.
func TestClaudeSession_TurnIgnoresDeltas(t *testing.T) {
	f := newFakeClaudeTransport()
	defer f.close()

	go f.writeLine(t, initEvent("sess-turn-d"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := newClaudeSessionWithTransport(ctx, SessionConfig{}, "", f.tr)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	go func() {
		f.writeLine(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"par"}},"session_id":"sess-turn-d"}`)
		f.writeLine(t, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"tial"}},"session_id":"sess-turn-d"}`)
		f.writeLine(t, assistantText("partial answer"))
		f.writeLine(t, resultOK("sess-turn-d"))
	}()

	got, err := s.Turn(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	if got != "partial answer" {
		t.Errorf("want full text 'partial answer', got %q", got)
	}
}
