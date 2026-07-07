package wrapper

// Scale-to-zero idle detection (specs/2026-07-07-agent-scale-to-zero — issue #6).
//
// An ahsir agent registers once but is idle most of the time; the process still
// pins RAM, a port, and a live LLM CLI subprocess. This file implements the
// agent-side half of scale-to-zero: detect that no turn has run for a while and
// trigger a graceful self-exit. Waking a reaped agent back up is the
// scheduler's job (the activator).
//
// Why the detection lives in the agent, not the scheduler (decision A in the
// spec): the scheduler is NOT on every turn's path. Streaming CLI clients and
// agent→agent A2A_CALL dial the agent endpoint directly, bypassing
// ChatWithAgentAs. The session pool's gated sessions are the ONLY choke point
// every turn funnels through, so a refcount is only accurate here.

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultIdleTimeout is the global default idle-reap window applied when an
	// agent card leaves runtime.idle_timeout unset (decision B in the spec).
	DefaultIdleTimeout = 10 * time.Minute

	// IdleStopExitCode is the process exit status an ahsir-agent uses when it
	// voluntarily shuts down after going idle. The scheduler keys on it to tell
	// a controlled scale-to-zero exit apart from a crash: an idle exit is marked
	// idle-stopped and NOT restarted, whereas any other non-zero / signal exit
	// goes through the normal supervisor crash path (decision, spec §4.3).
	// Chosen well outside the range shells use for signals (128+n) and the
	// common 0 / 1 / 2 so it cannot be confused with an ordinary failure.
	IdleStopExitCode = 111
)

// ErrAgentIdleStopping is returned by a gated session when a turn arrives in the
// instant AFTER the idle reaper has already committed to shutting the agent
// down. It is a retriable signal — the caller (scheduler activator) wakes the
// agent and retries. It never means an in-flight turn was dropped: the reaper
// only commits when no turn is in flight (see idleMonitor.fire).
var ErrAgentIdleStopping = errors.New("agent idle-stopping")

// ParseIdleTimeout interprets a card's runtime.idle_timeout value.
//
//   - ""          -> (DefaultIdleTimeout, true, nil): unset uses the global default
//   - "0" / "0s"  -> (0, false, nil): explicit zero disables reaping (resident agent)
//   - "15m" etc   -> (d, true, nil)
//   - invalid     -> (0, false, err)
//
// enabled reports whether idle reaping should run at all. A per-agent explicit
// zero is the documented "pin this agent resident" escape hatch, byte-for-byte
// the historical always-on behaviour.
func ParseIdleTimeout(s string) (timeout time.Duration, enabled bool, err error) {
	if strings.TrimSpace(s) == "" {
		return DefaultIdleTimeout, true, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false, err
	}
	if d <= 0 {
		return 0, false, nil
	}
	return d, true, nil
}

// idleMonitor is the single in-process aggregation point for "is this agent
// doing any work right now". Every turn, whatever ingress it arrived through,
// funnels through the session pool's gated sessions, which bracket the turn
// with turnStarted / release. When the last turn ends the monitor arms a timer;
// when the timer fires with still no work in flight it invokes onReap exactly
// once to trigger a graceful self-exit.
//
// Safety bias (spec §4.2.1): the only tolerated failure direction is a
// stuck-high refcount (agent never reaped — wastes resources, but loses no
// work). Reaping a busy agent is prevented structurally:
//
//  1. turnStarted disarms the timer under the SAME mutex fire uses to decide,
//     so a turn arriving as the timer fires is never lost to the race.
//  2. fire re-confirms against an INDEPENDENT predicate (confirmIdle, backed by
//     the pool's live gate state) before committing — a disagreement with our
//     own refcount biases to NOT reaping.
//  3. release is idempotent (keyed by turn id), so a double-release on an
//     error+timeout+cancel exit path can't drive the count negative.
type idleMonitor struct {
	timeout     time.Duration
	onReap      func()
	confirmIdle func() bool // independent second source; nil skips the cross-check

	mu       sync.Mutex
	inflight map[uint64]struct{}
	nextID   uint64
	timer    *time.Timer
	reaped   bool
}

func newIdleMonitor(timeout time.Duration, onReap func(), confirmIdle func() bool) *idleMonitor {
	return &idleMonitor{
		timeout:     timeout,
		onReap:      onReap,
		confirmIdle: confirmIdle,
		inflight:    make(map[uint64]struct{}),
	}
}

// turnStarted registers a turn and returns an idempotent release closure to call
// (via defer) on EVERY exit path of the turn — success, error, timeout, evict,
// cancel, panic. ok is false when the agent has already committed to idle
// shutdown; the caller must NOT run the turn and should surface
// ErrAgentIdleStopping so it is retried elsewhere.
func (m *idleMonitor) turnStarted() (release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reaped {
		return func() {}, false
	}
	// A turn is starting — cancel any pending reap. Doing this under m.mu (the
	// same lock fire takes) is what makes R1 safe: fire cannot slip between the
	// refcount bump below and the disarm.
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	id := m.nextID
	m.nextID++
	m.inflight[id] = struct{}{}
	var once sync.Once
	return func() { once.Do(func() { m.turnEnded(id) }) }, true
}

func (m *idleMonitor) turnEnded(id uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.inflight[id]; !ok {
		return
	}
	delete(m.inflight, id)
	if len(m.inflight) == 0 && !m.reaped {
		m.armLocked()
	}
}

// armLocked (re)starts the idle timer. Caller holds m.mu.
func (m *idleMonitor) armLocked() {
	if m.timeout <= 0 {
		return
	}
	if m.timer != nil {
		m.timer.Stop()
	}
	m.timer = time.AfterFunc(m.timeout, m.fire)
}

// fire is the timer callback: reap iff still idle. See the struct doc for why
// this cannot kill a busy agent.
func (m *idleMonitor) fire() {
	m.mu.Lock()
	if m.reaped || len(m.inflight) > 0 {
		m.mu.Unlock()
		return
	}
	// Independent confirmation: even if our own refcount says idle, defer to the
	// pool's live gate state. A disagreement means a turn is running that we
	// somehow lost track of — bias to NOT reaping, and re-arm so we try again.
	if m.confirmIdle != nil && !m.confirmIdle() {
		log.Printf("idle reaper: fire aborted, pool reports a mid-turn session; re-arming")
		m.armLocked()
		m.mu.Unlock()
		return
	}
	m.reaped = true
	m.mu.Unlock()
	log.Printf("idle reaper: no turn in flight for %s, initiating graceful self-exit", m.timeout)
	if m.onReap != nil {
		m.onReap()
	}
}
