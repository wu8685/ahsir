package wrapper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SessionConfig configures a Claude Code session.
type SessionConfig struct {
	// Name is a human-readable identifier (typically the agent name) used
	// only for log lines. Optional — empty Name just produces logs without
	// the agent= field. Setting it makes grepping logs by agent trivial
	// when multiple agents share the same scheduler tee.
	Name    string
	Command string
	Args    []string
	Env     []string
	WorkDir string
	Timeout time.Duration
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

// SessionManager manages Claude Code invocations.
type SessionManager struct {
	cfg     SessionConfig
	mu      sync.Mutex
	running bool
}

// NewSessionManager creates a new session manager.
func NewSessionManager(cfg SessionConfig) *SessionManager {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &SessionManager{
		cfg: cfg,
	}
}

// Start validates the session config and marks the session as ready.
func (sm *SessionManager) Start(ctx context.Context) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.running {
		return errors.New("session already running")
	}

	if err := sm.cfg.Validate(); err != nil {
		return err
	}

	sm.running = true
	return nil
}

// Stop marks the session as stopped.
func (sm *SessionManager) Stop() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if !sm.running {
		return nil
	}
	sm.running = false
	return nil
}

// IsRunning returns whether the session is currently running.
func (sm *SessionManager) IsRunning() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.running
}

// Send executes the configured command, feeds the prompt through stdin, and
// returns the collected stdout.
//
// Two design choices, both in service of "fail loudly when the underlying CLI
// misbehaves":
//
//  1. The prompt goes through stdin, never as a positional argument. This
//     decouples the prompt from any flag parsing on the CLI side — variadic
//     flags (e.g. `claude --add-dir`, `--allowedTools`) cannot ever
//     accidentally swallow a trailing positional that turns out to be the
//     prompt.
//  2. Stderr is captured and surfaced via the returned error on non-zero
//     exit. Without this, a CLI that errored on stderr (auth failure, bad
//     flag, hook crash, ...) would simply produce empty stdout and the
//     wrapper would happily return "" as the agent reply — a silent failure
//     that's hard to diagnose from the outside.
func (sm *SessionManager) Send(ctx context.Context, prompt string) (string, error) {
	sm.mu.Lock()
	if !sm.running {
		sm.mu.Unlock()
		return "", errors.New("session not running")
	}
	sm.mu.Unlock()

	cmdCtx, cancel := context.WithTimeout(ctx, sm.cfg.Timeout)
	defer cancel()

	// Per-call latency log: lets operators spot whether the time is being
	// spent in the LLM (long claude exec) or elsewhere in the chain
	// (scheduler, MCP shim). Always logged, since session.Send is the
	// single LLM-bound exec point — the cost of a few log lines is
	// trivial compared to the LLM round-trip itself.
	//
	// Each call gets a 6-hex correlation id so concurrent Sends interleaving
	// in the log can still be reconstructed (start ↔ end pair).
	start := time.Now()
	callID := newCallID()
	tag := sm.logTag(callID)
	log.Printf("session.Send: claude starting (%s, prompt=%dB, timeout=%v)", tag, len(prompt), sm.cfg.Timeout)

	cmd := exec.CommandContext(cmdCtx, sm.cfg.Command, sm.cfg.Args...)
	if sm.cfg.WorkDir != "" {
		cmd.Dir = sm.cfg.WorkDir
	}
	if len(sm.cfg.Env) > 0 {
		cmd.Env = sm.cfg.Env
	}
	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("create stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start command: %w", err)
	}

	var out strings.Builder
	scanner := bufio.NewScanner(stdoutPipe)
	for scanner.Scan() {
		line := scanner.Text()
		// Drop Claude Code's own startup logging lines so they don't pollute
		// the LLM response we hand back to the executor.
		if strings.HasPrefix(line, "202") &&
			(strings.Contains(line, "logging/config.go") || strings.Contains(line, "Logging level")) {
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}

	waitErr := cmd.Wait()
	elapsed := time.Since(start)
	stderrBytes := stderrBuf.Len()
	if waitErr != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		// exitDescription: "exit status 7" / "signal: killed" / a generic
		// I/O error string if the process never started cleanly. ProcessState
		// is nil only when Wait returned before the process was reaped, which
		// shouldn't happen here but we fall back gracefully.
		exitDesc := "unknown"
		if cmd.ProcessState != nil {
			exitDesc = cmd.ProcessState.String()
		} else {
			exitDesc = waitErr.Error()
		}
		log.Printf("session.Send: claude FAILED in %v (%s, prompt=%dB, stdout=%dB, stderr=%dB, %s)", elapsed, tag, len(prompt), out.Len(), stderrBytes, exitDesc)
		if stderr != "" {
			log.Printf("session.Send: claude stderr (%s): %s", tag, truncateForLog(stderr, 400))
		}
		if stderr == "" {
			return "", fmt.Errorf("%s exited: %w", sm.cfg.Command, waitErr)
		}
		return "", fmt.Errorf("%s exited: %w; stderr: %s", sm.cfg.Command, waitErr, stderr)
	}

	log.Printf("session.Send: claude ok in %v (%s, prompt=%dB, stdout=%dB, stderr=%dB)", elapsed, tag, len(prompt), out.Len(), stderrBytes)
	if stderrBytes > 0 {
		// Successful exit but the CLI wrote to stderr — usually deprecation
		// warnings or hook noise (e.g. SessionEnd hook errors). Surface a
		// preview so operators don't miss it just because the call "succeeded".
		log.Printf("session.Send: claude stderr-on-success (%s): %s", tag, truncateForLog(strings.TrimSpace(stderrBuf.String()), 200))
	}
	return out.String(), nil
}

// logTag formats the per-call log identifier shared by start/end log lines.
// Includes the call id; agent name is appended only when configured (some
// tests construct SessionConfig without a Name).
func (sm *SessionManager) logTag(callID string) string {
	if sm.cfg.Name == "" {
		return "id=" + callID
	}
	return "id=" + callID + ", agent=" + sm.cfg.Name
}

// newCallID returns a 6-hex correlation id. Uses crypto/rand because it is
// always available and we don't need to seed; entropy quality is irrelevant
// here — collision risk over a single process run is negligible at 16M space.
func newCallID() string {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "000000"
	}
	return hex.EncodeToString(buf[:])
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
