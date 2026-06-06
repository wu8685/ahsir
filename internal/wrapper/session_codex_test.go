package wrapper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBuildCodexExecArgs_NewThread(t *testing.T) {
	got := buildCodexExecArgs([]string{"--json", "--model=gpt-5.4", "--sandbox=workspace-write"}, "", "hello")
	want := []string{
		"exec",
		"--model=gpt-5.4",
		"--sandbox=workspace-write",
		"--json",
		"--skip-git-repo-check",
		"hello",
	}
	if !equalStrings(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBuildCodexExecArgs_Resume(t *testing.T) {
	got := buildCodexExecArgs(nil, "thread-123", "continue")
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
	got := buildCodexExecArgs([]string{"--ask-for-approval=never", "-a", "never"}, "", "hello")
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
