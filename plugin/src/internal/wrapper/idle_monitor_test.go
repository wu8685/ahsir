package wrapper

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseAgentIdleTimeout(t *testing.T) {
	cases := []struct {
		in          string
		wantTimeout time.Duration
		wantEnabled bool
		wantErr     bool
	}{
		{"", DefaultAgentIdleTimeout, true, false},
		{"   ", DefaultAgentIdleTimeout, true, false}, // whitespace == unset
		{"0", 0, false, false},                        // explicit resident
		{"0s", 0, false, false},
		{"-5m", 0, false, false}, // non-positive disables
		{"15m", 15 * time.Minute, true, false},
		{"2h", 2 * time.Hour, true, false},
		{"bogus", 0, false, true},
	}
	for _, c := range cases {
		got, enabled, err := ParseAgentIdleTimeout(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseAgentIdleTimeout(%q): expected error, got none", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAgentIdleTimeout(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.wantTimeout || enabled != c.wantEnabled {
			t.Errorf("ParseAgentIdleTimeout(%q) = (%v, %v), want (%v, %v)", c.in, got, enabled, c.wantTimeout, c.wantEnabled)
		}
	}
}

// reapFlag is a tiny helper to observe onReap firing across goroutines.
type reapFlag struct{ n int32 }

func (r *reapFlag) fire()       { atomic.AddInt32(&r.n, 1) }
func (r *reapFlag) count() int  { return int(atomic.LoadInt32(&r.n)) }
func (r *reapFlag) fired() bool { return r.count() > 0 }

// TestIdleMonitorReapsWhenIdle: a turn completes, and after the idle window the
// reaper fires exactly once.
func TestIdleMonitorReapsWhenIdle(t *testing.T) {
	var rf reapFlag
	m := newIdleMonitor(15*time.Millisecond, rf.fire, func() bool { return true })

	release, ok := m.turnStarted()
	if !ok {
		t.Fatal("turnStarted returned ok=false on fresh monitor")
	}
	release() // turn done -> arms the timer

	deadline := time.Now().Add(2 * time.Second)
	for !rf.fired() {
		if time.Now().After(deadline) {
			t.Fatal("reaper never fired after idle window")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Give any stray re-arm a chance; must stay at exactly one reap.
	time.Sleep(30 * time.Millisecond)
	if rf.count() != 1 {
		t.Fatalf("reaper fired %d times, want exactly 1", rf.count())
	}
}

// TestIdleMonitorDisarmsWhileTurnInFlight: an open turn suppresses the reaper
// even past the idle window (safe direction: never kill a busy agent).
func TestIdleMonitorDisarmsWhileTurnInFlight(t *testing.T) {
	var rf reapFlag
	m := newIdleMonitor(15*time.Millisecond, rf.fire, func() bool { return true })

	// Complete one turn (arms), then start another and hold it open.
	release, _ := m.turnStarted()
	release()
	hold, ok := m.turnStarted() // this disarms the pending timer
	if !ok {
		t.Fatal("second turnStarted returned ok=false")
	}

	time.Sleep(60 * time.Millisecond)
	if rf.fired() {
		t.Fatal("reaper fired while a turn was in flight")
	}
	hold() // now idle again -> re-arms
	deadline := time.Now().Add(2 * time.Second)
	for !rf.fired() {
		if time.Now().After(deadline) {
			t.Fatal("reaper never fired after the held turn finished")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestIdleMonitorFireAbortsWithInflight (R1, ordering A): the timer fires but a
// turn is in flight -> no reap.
func TestIdleMonitorFireAbortsWithInflight(t *testing.T) {
	var rf reapFlag
	// Large timeout so no real timer interferes; we drive fire() directly.
	m := newIdleMonitor(time.Hour, rf.fire, func() bool { return true })

	_, ok := m.turnStarted() // hold a turn open (no release)
	if !ok {
		t.Fatal("turnStarted returned ok=false")
	}
	m.fire() // simulate the timer racing in while the turn runs
	if rf.fired() {
		t.Fatal("reaper committed while a turn was in flight (R1 violated)")
	}
}

// TestIdleMonitorRejectsTurnAfterReap (R1, ordering B): the reaper commits, and
// a turn arriving after is cleanly rejected rather than run on a dying agent.
func TestIdleMonitorRejectsTurnAfterReap(t *testing.T) {
	var rf reapFlag
	m := newIdleMonitor(time.Hour, rf.fire, func() bool { return true })

	m.fire() // idle -> commit reap
	if !rf.fired() {
		t.Fatal("reaper did not commit on an idle monitor")
	}
	release, ok := m.turnStarted()
	if ok {
		t.Fatal("turnStarted accepted a turn after reap commit (would run on a dying agent)")
	}
	release() // must be a safe no-op
}

// TestIdleMonitorSecondConfirmation: even with an empty refcount, a
// confirmIdle=false vote blocks the reap and re-arms; once it agrees, the reap
// proceeds.
func TestIdleMonitorSecondConfirmation(t *testing.T) {
	var rf reapFlag
	var busy atomic.Bool
	busy.Store(true) // pool says "a turn is mid-flight" despite our empty refcount
	m := newIdleMonitor(time.Hour, rf.fire, func() bool { return !busy.Load() })

	m.fire()
	if rf.fired() {
		t.Fatal("reaper committed despite confirmIdle reporting a mid-turn session")
	}
	// Pool now agrees it's idle.
	busy.Store(false)
	m.fire()
	if !rf.fired() {
		t.Fatal("reaper did not commit after confirmIdle agreed it was idle")
	}
}

// TestIdleMonitorReleaseIdempotent: a double release (error+timeout+cancel exit
// paths all deferring release) must not corrupt the refcount / arm twice.
func TestIdleMonitorReleaseIdempotent(t *testing.T) {
	var rf reapFlag
	m := newIdleMonitor(time.Hour, rf.fire, func() bool { return true })

	r1, _ := m.turnStarted()
	r2, _ := m.turnStarted() // two concurrent turns
	r1()
	r1() // idempotent — second call is a no-op
	// One turn still in flight; fire must not reap.
	m.fire()
	if rf.fired() {
		t.Fatal("reaper committed with one turn still in flight")
	}
	r2()
	r2() // idempotent
	// Now truly idle.
	m.fire()
	if !rf.fired() {
		t.Fatal("reaper did not commit once all turns released")
	}
}

// TestIdleMonitorConcurrentTurns hammers turnStarted/release from many
// goroutines to shake out refcount races under -race; the monitor must not
// panic and must end reapable.
func TestIdleMonitorConcurrentTurns(t *testing.T) {
	var rf reapFlag
	m := newIdleMonitor(time.Hour, rf.fire, func() bool { return true })
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := m.turnStarted()
			if !ok {
				return
			}
			time.Sleep(time.Millisecond)
			release()
		}()
	}
	wg.Wait()
	m.fire()
	if !rf.fired() {
		t.Fatal("monitor not reapable after all concurrent turns drained")
	}
}
