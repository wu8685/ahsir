package wrapper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// SessionConfig configures a Claude Code session.
type SessionConfig struct {
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

// Send executes the command with the prompt as a CLI argument and returns an output channel.
func (sm *SessionManager) Send(ctx context.Context, prompt string) (<-chan string, error) {
	sm.mu.Lock()
	if !sm.running {
		sm.mu.Unlock()
		return nil, errors.New("session not running")
	}
	sm.mu.Unlock()

	outputCh := make(chan string, 100)

	go func() {
		defer close(outputCh)

		// Build args: base args + prompt as last argument
		args := make([]string, len(sm.cfg.Args), len(sm.cfg.Args)+1)
		copy(args, sm.cfg.Args)
		args = append(args, prompt)

		cmdCtx, cancel := context.WithTimeout(ctx, sm.cfg.Timeout)
		defer cancel()

		cmd := exec.CommandContext(cmdCtx, sm.cfg.Command, args...)
		if sm.cfg.WorkDir != "" {
			cmd.Dir = sm.cfg.WorkDir
		}
		if len(sm.cfg.Env) > 0 {
			cmd.Env = sm.cfg.Env
		}

		// Capture combined stdout/stderr
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			outputCh <- fmt.Sprintf("ERROR: create stdout pipe: %v", err)
			return
		}

		if err := cmd.Start(); err != nil {
			outputCh <- fmt.Sprintf("ERROR: start command: %v", err)
			return
		}

		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				cmd.Process.Kill()
				return
			default:
			}

			line := scanner.Text()
			// Skip log lines emitted by Claude Code
			if strings.HasPrefix(line, "202") && strings.Contains(line, "logging/config.go") {
				continue
			}
			if strings.HasPrefix(line, "202") && strings.Contains(line, "Logging level") {
				continue
			}

			select {
			case outputCh <- line:
			case <-ctx.Done():
				cmd.Process.Kill()
				return
			}
		}

		cmd.Wait()
	}()

	return outputCh, nil
}
