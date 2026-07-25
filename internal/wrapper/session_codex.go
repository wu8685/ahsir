package wrapper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"

	ahprocess "github.com/wu8685/ahsir/internal/process"
)

// Compile-time assertion that CodexSession satisfies the Session interface.
var _ Session = (*CodexSession)(nil)

// CodexSession is a Session backend powered by `codex exec --json`.
//
// Unlike ClaudeSession, Codex does not keep a single subprocess alive.
// Instead each turn forks one non-interactive Codex run. When Codex reports a
// thread_id, the next turn resumes that thread with `codex exec resume <id>`,
// so conversation continuity is owned by Codex's local transcript store.
type CodexSession struct {
	cfg    SessionConfig
	runner codexRunner

	mu               sync.Mutex
	closed           bool
	inFlight         bool
	sessionID        string
	onSessionIDKnown func(string)
}

type codexRunner func(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error)

type codexRunResult struct {
	ThreadID   string
	Text       string
	Stats      TurnStats
	Tools      []EventToolUse
	AgentCalls []EventAgentCall
}

// NewCodexSession constructs a Codex-backed Session. resumeID, when non-empty,
// is used on the next turn via `codex exec resume <resumeID>`.
func NewCodexSession(_ context.Context, cfg SessionConfig, resumeID string) (*CodexSession, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newCodexSessionWithRunner(cfg, resumeID, runCodexExec), nil
}

func newCodexSessionWithRunner(cfg SessionConfig, resumeID string, runner codexRunner) *CodexSession {
	return &CodexSession{
		cfg:       cfg,
		runner:    runner,
		sessionID: resumeID,
	}
}

// SessionID returns the Codex thread_id once known. It may be empty before the
// first successful turn.
func (s *CodexSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// OnSessionIDKnown registers a callback fired when Codex reports a thread_id.
// SessionPool uses this to persist contextID -> thread_id after the first
// successful turn of a fresh Codex thread.
func (s *CodexSession) OnSessionIDKnown(fn func(string)) {
	s.mu.Lock()
	sid := s.sessionID
	s.onSessionIDKnown = fn
	s.mu.Unlock()
	if sid != "" && fn != nil {
		fn(sid)
	}
}

// IsHealthy reports whether the session can accept another turn. CodexSession
// has no long-lived subprocess, so only explicit Close marks it unhealthy.
func (s *CodexSession) IsHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// Stream starts one non-interactive Codex turn and emits provider-neutral
// events. Concurrent turns on the same session are rejected to preserve the
// Session contract.
func (s *CodexSession) Stream(ctx context.Context, userText string) (<-chan Event, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("codex session: closed")
	}
	if s.inFlight {
		s.mu.Unlock()
		return nil, fmt.Errorf("codex session: %w", ErrTurnInFlight)
	}
	s.inFlight = true
	resumeID := s.sessionID
	s.mu.Unlock()

	ch := make(chan Event, 8)
	go func() {
		defer close(ch)
		defer func() {
			s.mu.Lock()
			s.inFlight = false
			s.mu.Unlock()
		}()

		runCtx := ctx
		var cancel context.CancelFunc
		if s.cfg.Timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, s.cfg.Timeout)
			defer cancel()
		}

		result, err := s.runner(runCtx, s.cfg, resumeID, userText)
		for _, tool := range result.Tools {
			ch <- tool
		}
		for _, call := range result.AgentCalls {
			ch <- call
		}
		if result.Text != "" {
			ch <- EventText{Text: result.Text}
		}
		var cb func(string)
		if result.ThreadID != "" {
			s.mu.Lock()
			s.sessionID = result.ThreadID
			cb = s.onSessionIDKnown
			s.mu.Unlock()
		}
		if cb != nil {
			cb(result.ThreadID)
		}
		ch <- EventTurnDone{Err: err, Stats: result.Stats}
	}()
	return ch, nil
}

// Turn drains Stream and aggregates final assistant text.
func (s *CodexSession) Turn(ctx context.Context, userText string) (string, error) {
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

// Close marks the session closed. There is no resident process to kill.
func (s *CodexSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func runCodexExec(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error) {
	args := buildCodexExecArgs(cfg.Args, resumeID, prompt, cfg.WriteAccess, cfg.NetworkAccess, cfg.CodexProviderArgs)
	cmd := exec.CommandContext(ctx, cfg.Command, args...)
	ahprocess.PrepareCommand(cmd)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	if len(cfg.Env) > 0 {
		cmd.Env = cfg.Env
	}

	var stderr bytes.Buffer
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return codexRunResult{}, fmt.Errorf("codex stdout pipe: %w", err)
	}
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return codexRunResult{}, fmt.Errorf("codex start: %w", err)
	}
	log.Printf("codex session: started pid=%d cmd=%s args=%v", cmd.Process.Pid, cfg.Command, args)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = ahprocess.KillTree(cmd.Process)
		case <-done:
		}
	}()

	result, parseErr := parseCodexJSONL(stdout)
	waitErr := cmd.Wait()
	close(done)
	if parseErr != nil {
		return result, parseErr
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return result, fmt.Errorf("codex exec failed: %w: %s", waitErr, truncateForLog(msg, 1000))
		}
		return result, fmt.Errorf("codex exec failed: %w", waitErr)
	}
	return result, nil
}

// buildCodexExecArgs returns arguments for the Codex CLI binary. Runtime args
// are treated as `codex exec` flags; the provider owns --json so the parser can
// consume a stable JSONL event stream.
func buildCodexExecArgs(runtimeArgs []string, resumeID, prompt string, writeAccess, networkAccess bool, trustedProviderArgs ...[]string) []string {
	args := []string{"exec"}
	args = append(args, sanitizeCodexExecArgs(runtimeArgs)...)
	args = ensureCodexFlag(args, "--json")
	sandbox := "read-only"
	if writeAccess {
		sandbox = "workspace-write"
	}
	args = ensureCodexFlagValue(args, "--sandbox", "-s", sandbox)
	if writeAccess && networkAccess {
		args = append(args, "-c", "sandbox_workspace_write.network_access=true")
	}
	if len(trustedProviderArgs) > 0 {
		args = append(args, trustedProviderArgs[0]...)
	}
	args = ensureCodexFlag(args, "--skip-git-repo-check")
	if resumeID != "" {
		args = append(args, "resume", resumeID, prompt)
	} else {
		args = append(args, prompt)
	}
	return args
}

// sanitizeCodexExecArgs filters runtime.args before they reach `codex exec`.
// Two classes are dropped:
//
//   - protocol-owned flags (--json, -o, ...) the provider must control so the
//     JSONL parser sees a stable stream;
//   - sandbox/approval policy flags. The wrapper owns the security posture
//     (it appends --sandbox=read-only in buildCodexExecArgs); a config or
//     template author must not be able to silently loosen it via raw CLI
//     args (--sandbox danger-full-access, --yolo, -c sandbox_mode=...).
//     Widening access should go through an explicit card-schema knob, not
//     arg passthrough. This is a denylist of the known bypass vectors —
//     defense in depth, not a sandbox proof.
func sanitizeCodexExecArgs(in []string) []string {
	out := make([]string, 0, len(in))
	for i := 0; i < len(in); i++ {
		a := in[i]
		switch {
		case a == "exec":
			continue
		case a == "--json":
			continue
		case a == "--ask-for-approval", a == "-a",
			a == "-o", a == "--output-last-message", a == "--output-schema",
			a == "--sandbox", a == "-s":
			i++ // drop the flag and its separate value token
			continue
		case strings.HasPrefix(a, "--ask-for-approval="), strings.HasPrefix(a, "-a="),
			strings.HasPrefix(a, "-o="), strings.HasPrefix(a, "--output-last-message="), strings.HasPrefix(a, "--output-schema="),
			strings.HasPrefix(a, "--sandbox="), strings.HasPrefix(a, "-s="):
			continue
		case a == "--dangerously-bypass-approvals-and-sandbox", a == "--yolo", a == "--full-auto":
			continue
		case a == "-c", a == "--config":
			if i+1 < len(in) && codexOverrideBypassesPolicy(in[i+1]) {
				i++ // drop the override and its key=value token
				continue
			}
			out = append(out, a)
		case strings.HasPrefix(a, "-c="):
			if codexOverrideBypassesPolicy(a[len("-c="):]) {
				continue
			}
			out = append(out, a)
		case strings.HasPrefix(a, "--config="):
			if codexOverrideBypassesPolicy(a[len("--config="):]) {
				continue
			}
			out = append(out, a)
		default:
			out = append(out, a)
		}
	}
	return out
}

// codexOverrideBypassesPolicy reports whether a `-c key=value` generic config
// override targets security policy or provider authentication owned by ahsir.
func codexOverrideBypassesPolicy(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	key = strings.TrimSpace(key)
	switch {
	case key == "sandbox_mode", key == "approval_policy",
		key == "model_provider", key == "openai_base_url",
		key == "preferred_auth_method", key == "cli_auth_credentials_store":
		return true
	case strings.HasPrefix(key, "sandbox_workspace_write"),
		strings.HasPrefix(key, "model_providers."):
		return true
	}
	return false
}

func ensureCodexFlag(args []string, flag string) []string {
	for _, a := range args {
		if a == flag {
			return args
		}
	}
	return append(args, flag)
}

func ensureCodexFlagValue(args []string, long, short, value string) []string {
	for i, a := range args {
		if a == long || a == short {
			if i+1 < len(args) {
				return args
			}
		}
		if strings.HasPrefix(a, long+"=") || strings.HasPrefix(a, short+"=") {
			return args
		}
	}
	return append(args, long+"="+value)
}

func parseCodexJSONL(r io.Reader) (codexRunResult, error) {
	var result codexRunResult
	var protocolErr error
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		var env struct {
			Type     string          `json:"type"`
			ThreadID string          `json:"thread_id"`
			Item     json.RawMessage `json:"item"`
			Usage    struct {
				InputTokens           int `json:"input_tokens"`
				OutputTokens          int `json:"output_tokens"`
				ReasoningOutputTokens int `json:"reasoning_output_tokens"`
			} `json:"usage"`
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}

		switch env.Type {
		case "thread.started":
			result.ThreadID = env.ThreadID
		case "turn.completed":
			result.Stats.InputTokens = env.Usage.InputTokens
			result.Stats.OutputTokens = env.Usage.OutputTokens
		case "turn.failed", "error":
			msg := env.Message
			if msg == "" {
				msg = env.Error
			}
			if msg == "" {
				msg = env.Type
			}
			protocolErr = errors.New(msg)
		case "item.completed":
			applyCodexCompletedItem(&result, env.Item)
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read codex jsonl: %w", err)
	}
	return result, protocolErr
}

func applyCodexCompletedItem(result *codexRunResult, raw json.RawMessage) {
	var head struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Command string          `json:"command"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return
	}
	switch head.Type {
	case "agent_message":
		result.Text = head.Text
	case "command_execution":
		input := json.RawMessage(`{}`)
		if head.Command != "" {
			b, _ := json.Marshal(map[string]string{"command": head.Command})
			input = b
		}
		result.Tools = append(result.Tools, EventToolUse{Name: "command_execution", Input: input})
	case "mcp_tool_call", "tool_call":
		name := head.Name
		if name == "" {
			name = head.Type
		}
		input := head.Input
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		if call, ok := ParseA2ACallTool(name, input); ok {
			result.AgentCalls = append(result.AgentCalls, EventAgentCall{Agent: call.Agent, Task: call.Task})
			return
		}
		result.Tools = append(result.Tools, EventToolUse{Name: name, Input: input})
	}
}
