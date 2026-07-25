package wrapper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBuildCodexExecArgs_NewThread(t *testing.T) {
	// User-supplied --sandbox is stripped: the wrapper owns the sandbox
	// posture and always pins read-only. Loosening must go through an
	// explicit card-schema knob, not raw arg passthrough.
	got := buildCodexExecArgs([]string{"--json", "--model=gpt-5.4", "--sandbox=workspace-write"}, "", "hello", false, false)
	want := []string{
		"exec",
		"--model=gpt-5.4",
		"--json",
		"--sandbox=read-only",
		"--skip-git-repo-check",
		"hello",
	}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildCodexExecArgs_CardWriteAccessUsesWorkspaceWrite(t *testing.T) {
	got := buildCodexExecArgs([]string{"--sandbox=danger-full-access"}, "", "hello", true, false)
	want := []string{
		"exec",
		"--json",
		"--sandbox=workspace-write",
		"--skip-git-repo-check",
		"hello",
	}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildCodexExecArgs_CardNetworkAccessUsesTrustedOverride(t *testing.T) {
	got := buildCodexExecArgs([]string{"--config=sandbox_workspace_write.network_access=false"}, "", "hello", true, true)
	want := []string{
		"exec",
		"--json",
		"--sandbox=workspace-write",
		"-c", "sandbox_workspace_write.network_access=true",
		"--skip-git-repo-check",
		"hello",
	}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildCodexExecArgs_NetworkAccessCannotWidenReadOnly(t *testing.T) {
	got := buildCodexExecArgs(nil, "", "hello", false, true)
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "network_access=true") {
		t.Fatalf("read-only sandbox must ignore network access, got %v", got)
	}
}

func TestSanitizeCodexExecArgs_StripsSandboxBypassVectors(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "sandbox flag separate value",
			in:   []string{"--sandbox", "danger-full-access", "--model=gpt-5.4"},
			want: []string{"--model=gpt-5.4"},
		},
		{
			name: "sandbox short flag eq form",
			in:   []string{"-s=danger-full-access"},
			want: []string{},
		},
		{
			name: "dangerously bypass and aliases",
			in:   []string{"--dangerously-bypass-approvals-and-sandbox", "--yolo", "--full-auto"},
			want: []string{},
		},
		{
			name: "config override of sandbox_mode",
			in:   []string{"-c", "sandbox_mode=danger-full-access", "--config", `approval_policy="never"`},
			want: []string{},
		},
		{
			name: "config override eq form",
			in:   []string{"-c=sandbox_mode=danger-full-access", "--config=sandbox_workspace_write.network_access=true"},
			want: []string{},
		},
		{
			name: "provider auth overrides",
			in: []string{
				"-c", `model_provider="evil"`,
				"-c", `model_providers.evil.env_key="OPENAI_API_KEY"`,
				"-c", `model_providers.evil.experimental_bearer_token="literal"`,
				"-c", `model_providers.evil.requires_openai_auth=true`,
				`--config=model_providers.evil.base_url="https://evil.example"`,
				`--config=openai_base_url="https://evil.example"`,
			},
			want: []string{},
		},
		{
			name: "benign config overrides survive",
			in:   []string{"-c", "model_reasoning_effort=high", "--config=model_verbosity=low"},
			want: []string{"-c", "model_reasoning_effort=high", "--config=model_verbosity=low"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeCodexExecArgs(append([]string(nil), tc.in...))
			if !equalStrings(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildCodexExecArgs_Resume(t *testing.T) {
	got := buildCodexExecArgs(nil, "thread-123", "continue", false, false)
	want := []string{
		"exec",
		"--json",
		"--sandbox=read-only",
		"--skip-git-repo-check",
		"resume",
		"thread-123",
		"continue",
	}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildCodexExecArgs_StripsUnsupportedApprovalFlag(t *testing.T) {
	got := buildCodexExecArgs([]string{"--ask-for-approval=never", "-a", "never"}, "", "hello", false, false)
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "ask-for-approval") || strings.Contains(joined, " -a ") {
		t.Fatalf("approval flags should be stripped for codex exec compatibility, got %v", got)
	}
}

func TestParseCodexJSONL(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-abc"}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"done"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":5,"reasoning_output_tokens":2}}`,
	}, "\n"))

	got, err := parseCodexJSONL(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThreadID != "thread-abc" {
		t.Errorf("ThreadID = %q", got.ThreadID)
	}
	if got.Text != "done" {
		t.Errorf("Text = %q", got.Text)
	}
	if got.Stats.InputTokens != 12 || got.Stats.OutputTokens != 5 {
		t.Errorf("Stats = %+v", got.Stats)
	}
	if len(got.Tools) != 1 || got.Tools[0].Name != "command_execution" {
		t.Errorf("Tools = %+v", got.Tools)
	}
}

func TestParseCodexJSONL_A2AToolCall(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread-abc"}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"tool_call","name":"a2a_call","input":{"agent":"backend","task":"design API"}}}`,
		`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":5}}`,
	}, "\n"))

	got, err := parseCodexJSONL(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.AgentCalls) != 1 {
		t.Fatalf("want 1 agent call, got %+v", got.AgentCalls)
	}
	call := got.AgentCalls[0]
	if call.Agent != "backend" || call.Task != "design API" {
		t.Fatalf("unexpected agent call: %+v", call)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("a2a_call should not be emitted as generic tool: %+v", got.Tools)
	}
}

func TestParseCodexJSONL_ErrorEvent(t *testing.T) {
	got, err := parseCodexJSONL(strings.NewReader(`{"type":"turn.failed","message":"no auth"}` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "no auth") {
		t.Fatalf("expected no auth error, got result=%+v err=%v", got, err)
	}
}

func TestCodexSession_TurnStoresThreadID(t *testing.T) {
	var calls int
	s := newCodexSessionWithRunner(SessionConfig{Command: "codex"}, "", func(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error) {
		calls++
		if resumeID != "" {
			t.Fatalf("first call resumeID = %q", resumeID)
		}
		if prompt != "hi" {
			t.Fatalf("prompt = %q", prompt)
		}
		return codexRunResult{ThreadID: "thread-1", Text: "hello"}, nil
	})

	got, err := s.Turn(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Fatalf("got %q", got)
	}
	if s.SessionID() != "thread-1" {
		t.Fatalf("SessionID = %q", s.SessionID())
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestCodexSession_NotifiesThreadID(t *testing.T) {
	s := newCodexSessionWithRunner(SessionConfig{Command: "codex"}, "", func(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error) {
		return codexRunResult{ThreadID: "thread-known", Text: "ok"}, nil
	})
	got := make(chan string, 1)
	s.OnSessionIDKnown(func(sid string) {
		got <- sid
	})
	if _, err := s.Turn(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	select {
	case sid := <-got:
		if sid != "thread-known" {
			t.Fatalf("sid = %q", sid)
		}
	default:
		t.Fatal("callback did not fire")
	}
}

func TestCodexSession_UsesResumeID(t *testing.T) {
	s := newCodexSessionWithRunner(SessionConfig{Command: "codex"}, "thread-prior", func(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error) {
		if resumeID != "thread-prior" {
			t.Fatalf("resumeID = %q", resumeID)
		}
		return codexRunResult{ThreadID: "thread-prior", Text: "ok"}, nil
	})
	if _, err := s.Turn(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
}

func TestCodexSession_RejectsConcurrentTurn(t *testing.T) {
	block := make(chan struct{})
	s := newCodexSessionWithRunner(SessionConfig{Command: "codex"}, "", func(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error) {
		<-block
		return codexRunResult{Text: "done"}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ch, err := s.Stream(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stream(ctx, "second"); err == nil {
		t.Fatal("expected concurrent turn error")
	}
	close(block)
	for range ch {
	}
}

func TestCodexSession_TurnReturnsPartialTextWithError(t *testing.T) {
	s := newCodexSessionWithRunner(SessionConfig{Command: "codex"}, "", func(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error) {
		return codexRunResult{Text: "partial"}, errors.New("boom")
	})
	got, err := s.Turn(context.Background(), "x")
	if got != "partial" {
		t.Fatalf("text = %q", got)
	}
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}

func TestCodexSession_ZeroTimeoutDoesNotSetTurnDeadline(t *testing.T) {
	s := newCodexSessionWithRunner(SessionConfig{Command: "codex", Timeout: 0}, "", func(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("expected no deadline when cfg.Timeout is 0")
		}
		return codexRunResult{Text: "ok"}, nil
	})
	if _, err := s.Turn(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestRunCodexExecContextCancelKillsProcessTree(t *testing.T) {
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	scriptPath := filepath.Join(dir, "fake-codex")
	script := "#!/bin/sh\nsleep 1000 &\necho $! > \"$AHSIR_TEST_CODEX_TREE_CHILD_PID\"\nwait\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runCodexExec(ctx, SessionConfig{
			Command: scriptPath,
			Env: append(os.Environ(),
				"AHSIR_TEST_CODEX_TREE_CHILD_PID="+childPIDPath,
			),
		}, "", "hello")
	}()

	childPID := waitForCodexPIDFile(t, childPIDPath)
	defer killCodexPID(childPID)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runCodexExec did not return after context cancel")
	}
	deadline := time.Now().Add(2 * time.Second)
	for codexPIDExists(childPID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if codexPIDExists(childPID) {
		t.Fatalf("codex child pid %d still exists after context cancel", childPID)
	}
}

func waitForCodexPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil {
				t.Fatalf("parse pid file: %v", convErr)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for pid file %s", path)
	return 0
}

func codexPIDExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	return exec.Command("kill", "-0", strconv.Itoa(pid)).Run() == nil
}

func killCodexPID(pid int) {
	if pid > 0 {
		_ = exec.Command("kill", "-KILL", strconv.Itoa(pid)).Run()
	}
}

// TestCodexSession_ConcurrentStreamReturnsBusy is the Codex side of the
// same-context busy contract — same sentinel, same wire marker.
func TestCodexSession_ConcurrentStreamReturnsBusy(t *testing.T) {
	release := make(chan struct{})
	s := newCodexSessionWithRunner(SessionConfig{Command: "codex"}, "", func(ctx context.Context, cfg SessionConfig, resumeID, prompt string) (codexRunResult, error) {
		<-release
		return codexRunResult{Text: "done"}, nil
	})
	defer s.Close()

	ctx := context.Background()
	ch, err := s.Stream(ctx, "first turn")
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.Stream(ctx, "second turn while first in flight")
	if err == nil {
		t.Fatal("concurrent Stream on one session must fail fast")
	}
	if !errors.Is(err, ErrTurnInFlight) {
		t.Errorf("want errors.Is(err, ErrTurnInFlight), got %v", err)
	}
	if !strings.Contains(err.Error(), "agent busy:") {
		t.Errorf("busy error must carry the 'agent busy:' wire marker, got %q", err.Error())
	}

	close(release)
	for range ch {
	}
}
