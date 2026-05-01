package wrapper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// SessionConfig configures a Claude Code session.
type SessionConfig struct {
	Command     string
	Args        []string
	Env         []string
	WorkDir     string
	Timeout     time.Duration
	AutoRestart bool
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

// SessionManager manages a persistent Claude Code subprocess.
type SessionManager struct {
	cfg     SessionConfig
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
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

// Start launches the Claude Code subprocess.
func (sm *SessionManager) Start(ctx context.Context) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.running {
		return errors.New("session already running")
	}

	sm.cmd = exec.CommandContext(ctx, sm.cfg.Command, sm.cfg.Args...)
	if sm.cfg.WorkDir != "" {
		sm.cmd.Dir = sm.cfg.WorkDir
	}
	if len(sm.cfg.Env) > 0 {
		sm.cmd.Env = sm.cfg.Env
	}

	var err error
	sm.stdin, err = sm.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}
	sm.stdout, err = sm.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}
	// Capture stderr to avoid pipe blocking
	sm.cmd.Stderr = nil

	if err := sm.cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}
	sm.running = true
	return nil
}

// Stop terminates the Claude Code subprocess.
func (sm *SessionManager) Stop() error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if !sm.running {
		return nil
	}

	sm.stdin.Close()
	sm.stdout.Close()

	if sm.cmd != nil && sm.cmd.Process != nil {
		if err := sm.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("kill process: %w", err)
		}
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

// Send writes a prompt to the process and returns an output channel.
func (sm *SessionManager) Send(ctx context.Context, prompt string) (<-chan string, error) {
	sm.mu.Lock()
	if !sm.running {
		sm.mu.Unlock()
		return nil, errors.New("session not running")
	}

	if _, err := io.WriteString(sm.stdin, prompt); err != nil {
		sm.mu.Unlock()
		return nil, fmt.Errorf("write to stdin: %w", err)
	}
	sm.mu.Unlock()

	outputCh := make(chan string, 100)

	go func() {
		defer close(outputCh)
		scanner := bufio.NewScanner(sm.stdout)
		timeout := sm.cfg.Timeout

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()
			select {
			case outputCh <- line:
			case <-time.After(timeout):
				return
			}
		}
	}()

	return outputCh, nil
}
