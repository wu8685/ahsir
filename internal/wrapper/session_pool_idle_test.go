package wrapper

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoolAnyMidTurnReflectsGateState: AnyMidTurn is false when nothing runs,
// true while a turn is streaming, and false again once it drains. This is the
// single source of truth the idle reaper cross-checks.
func TestPoolAnyMidTurnReflectsGateState(t *testing.T) {
	probe := newGateProbeSession()
	pool := newGatedTestPool(t, probe)
	ctx := context.Background()

	if pool.AnyMidTurn() {
		t.Fatal("AnyMidTurn true before any turn started")
	}

	s, err := pool.LookupOrCreate(ctx, "ctx-mid")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.Stream(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	// The probe blocks until released — the turn is in flight now.
	deadline := time.Now().Add(2 * time.Second)
	for !pool.AnyMidTurn() {
		if time.Now().After(deadline) {
			t.Fatal("AnyMidTurn never became true during an in-flight turn")
		}
		time.Sleep(2 * time.Millisecond)
	}

	probe.release <- struct{}{}
	for range ch { // drain to completion
	}
	if pool.AnyMidTurn() {
		t.Fatal("AnyMidTurn still true after the turn drained")
	}
}

// TestPoolIdleReaperFiresWhenIdle: with the reaper enabled and no traffic, the
// agent's onReap is invoked (scale-to-zero on an idle boot).
func TestPoolIdleReaperFiresWhenIdle(t *testing.T) {
	probe := newGateProbeSession()
	pool := newGatedTestPool(t, probe)

	var reaped atomic.Bool
	pool.EnableIdleReaper(15*time.Millisecond, func() { reaped.Store(true) })

	deadline := time.Now().Add(2 * time.Second)
	for !reaped.Load() {
		if time.Now().After(deadline) {
			t.Fatal("idle reaper never fired on an idle pool")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestPoolIdleReaperHeldOffByInflightTurn: an in-flight turn must keep the
// reaper from committing — the cross-check against AnyMidTurn catches it even
// if the timer fires mid-turn.
func TestPoolIdleReaperHeldOffByInflightTurn(t *testing.T) {
	probe := newGateProbeSession()
	pool := newGatedTestPool(t, probe)
	ctx := context.Background()

	var reaped atomic.Bool
	pool.EnableIdleReaper(15*time.Millisecond, func() { reaped.Store(true) })

	s, err := pool.LookupOrCreate(ctx, "ctx-busy")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.Stream(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	// Hold the turn open well past the idle window.
	time.Sleep(80 * time.Millisecond)
	if reaped.Load() {
		t.Fatal("idle reaper committed while a turn was in flight")
	}

	probe.release <- struct{}{}
	for range ch {
	}
	// Once idle again, the reaper should eventually fire.
	deadline := time.Now().Add(2 * time.Second)
	for !reaped.Load() {
		if time.Now().After(deadline) {
			t.Fatal("idle reaper never fired after the turn drained")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestGatedSessionTurnTracksRefcount: the Turn path (not just Stream) brackets
// the turn for the reaper — mid-Turn the pool reports a mid-turn session, and it
// clears afterwards.
func TestGatedSessionTurnTracksRefcount(t *testing.T) {
	probe := newGateProbeSession()
	pool := newGatedTestPool(t, probe)
	pool.EnableIdleReaper(time.Hour, func() {}) // enabled so gatedSession carries the monitor
	ctx := context.Background()

	s, err := pool.LookupOrCreate(ctx, "ctx-turn")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		reply, _ := s.Turn(ctx, "q")
		done <- reply
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !pool.AnyMidTurn() {
		if time.Now().After(deadline) {
			t.Fatal("AnyMidTurn never true during Turn")
		}
		time.Sleep(2 * time.Millisecond)
	}
	probe.release <- struct{}{}
	<-done
	if pool.AnyMidTurn() {
		t.Fatal("AnyMidTurn still true after Turn completed")
	}
}

// TestGatedSessionRejectsTurnsAfterReap: once the reaper has committed, both
// Stream and Turn refuse new turns with ErrAgentIdleStopping rather than run
// them on a dying agent (R1 tail; the activator retries elsewhere).
func TestGatedSessionRejectsTurnsAfterReap(t *testing.T) {
	probe := newGateProbeSession()
	pool := newGatedTestPool(t, probe)

	reaped := make(chan struct{})
	var once sync.Once
	pool.EnableIdleReaper(10*time.Millisecond, func() { once.Do(func() { close(reaped) }) })
	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper never committed")
	}

	ctx := context.Background()
	s, err := pool.LookupOrCreate(ctx, "ctx-late")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stream(ctx, "late"); err != ErrAgentIdleStopping {
		t.Fatalf("Stream after reap: got %v, want ErrAgentIdleStopping", err)
	}
	if _, err := s.Turn(ctx, "late"); err != ErrAgentIdleStopping {
		t.Fatalf("Turn after reap: got %v, want ErrAgentIdleStopping", err)
	}
}

// TestPoolIdleReaperDisabledIsResident: EnableIdleReaper(0) is a no-op — the
// pool stays resident, matching the explicit `agent_idle_timeout: 0` escape hatch.
func TestPoolIdleReaperDisabledIsResident(t *testing.T) {
	probe := newGateProbeSession()
	pool := newGatedTestPool(t, probe)

	var reaped atomic.Bool
	pool.EnableIdleReaper(0, func() { reaped.Store(true) })

	time.Sleep(60 * time.Millisecond)
	if reaped.Load() {
		t.Fatal("reaper fired despite being disabled (timeout=0)")
	}
	if pool.monitor != nil {
		t.Fatal("monitor should not be set when reaping is disabled")
	}
}
