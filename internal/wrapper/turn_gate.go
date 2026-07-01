package wrapper

// Per-context FIFO turn gate (specs/2026-06-08-shared-context-collaboration.md).
//
// A contextID keys one provider session whose turn channel is physically
// serial. Pre-queue, a collision surfaced ErrTurnInFlight immediately (the
// busy/409 contract). The gate keeps that sentinel as the BACKPRESSURE signal:
// concurrent turns now wait in strict arrival order, and only a full queue —
// or queue_depth=0, the escape hatch back to fail-fast — produces the busy
// error. The wire contract is unchanged; what changed is when it fires.

import (
	"context"
	"fmt"
	"sync"
)

// defaultTurnQueueDepth is the number of waiters a context's queue holds
// before busy rejections resume. Multi-client sharing rarely stacks more than
// a few people behind one turn; a deeper queue would just convert backpressure
// into multi-minute silent waits.
const defaultTurnQueueDepth = 4

// turnGate serializes turns for one contextID. Ownership passes to waiters in
// strict FIFO order; a full queue rejects with ErrTurnInFlight.
type turnGate struct {
	mu      sync.Mutex
	busy    bool
	waiters []chan struct{}
}

func newTurnGate() *turnGate {
	return &turnGate{}
}

// acquire takes the gate or waits in line. depth bounds the number of
// concurrent waiters: 0 means never wait (legacy fail-fast). Returns
// ctx.Err() if the caller gives up while queued — the slot is surrendered
// without ever running.
func (g *turnGate) acquire(ctx context.Context, depth int) error {
	// A caller whose context is already dead must not take the gate even
	// when it is free — a cancelled queued task would otherwise slip
	// through after the gate empties.
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	if !g.busy {
		g.busy = true
		g.mu.Unlock()
		return nil
	}
	if len(g.waiters) >= depth {
		g.mu.Unlock()
		return fmt.Errorf("%w (turn queue full: %d already waiting)", ErrTurnInFlight, len(g.waiters))
	}
	ch := make(chan struct{})
	g.waiters = append(g.waiters, ch)
	g.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		for i, w := range g.waiters {
			if w == ch {
				g.waiters = append(g.waiters[:i:i], g.waiters[i+1:]...)
				g.mu.Unlock()
				return ctx.Err()
			}
		}
		g.mu.Unlock()
		// Lost the race: release() granted us the gate between ctx.Done()
		// and taking the lock. We own it but the caller is leaving — hand
		// ownership straight to the next waiter.
		g.release()
		return ctx.Err()
	}
}

// release hands the gate to the oldest waiter, or opens it when none wait.
func (g *turnGate) release() {
	g.mu.Lock()
	if len(g.waiters) > 0 {
		ch := g.waiters[0]
		g.waiters = g.waiters[1:]
		g.mu.Unlock()
		close(ch)
		return
	}
	g.busy = false
	g.mu.Unlock()
}

// gatedSession wraps a pooled Session with its entry's turn gate. Stream
// acquires the gate before opening the provider turn and releases it only
// after the event channel drains — the gate must cover the whole turn, not
// just its start, because the provider stream is the serial resource.
type gatedSession struct {
	inner Session
	gate  *turnGate
	depth func() int // live read of the pool's configured queue depth
}

func (g *gatedSession) Stream(ctx context.Context, userText string) (<-chan Event, error) {
	if err := g.gate.acquire(ctx, g.depth()); err != nil {
		return nil, err
	}
	ch, err := g.inner.Stream(ctx, userText)
	if err != nil {
		g.gate.release()
		return nil, err
	}
	out := make(chan Event)
	go func() {
		defer g.gate.release()
		defer close(out)
		for ev := range ch {
			select {
			case out <- ev:
			case <-ctx.Done():
				// Consumer stopped reading. Drain the inner channel so the
				// session's writer goroutine can finish, then release.
				for range ch {
				}
				return
			}
		}
	}()
	return out, nil
}

func (g *gatedSession) Turn(ctx context.Context, userText string) (string, error) {
	if err := g.gate.acquire(ctx, g.depth()); err != nil {
		return "", err
	}
	defer g.gate.release()
	return g.inner.Turn(ctx, userText)
}

func (g *gatedSession) SessionID() string { return g.inner.SessionID() }
func (g *gatedSession) IsHealthy() bool   { return g.inner.IsHealthy() }
func (g *gatedSession) Close() error      { return g.inner.Close() }
