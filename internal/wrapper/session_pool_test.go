package wrapper

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakePoolSession is a minimal Session impl for pool tests. It records
// whether Close was called so eviction tests can verify the pool released
// the underlying resource.
type fakePoolSession struct {
	sessionID string

	mu     sync.Mutex
	closed bool
}

func (f *fakePoolSession) Stream(ctx context.Context, userText string) (<-chan Event, error) {
	ch := make(chan Event, 1)
	ch <- EventTurnDone{}
	close(ch)
	return ch, nil
}
func (f *fakePoolSession) Turn(ctx context.Context, userText string) (string, error) {
	return "", nil
}
func (f *fakePoolSession) SessionID() string { return f.sessionID }
func (f *fakePoolSession) IsHealthy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.closed
}
func (f *fakePoolSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}
func (f *fakePoolSession) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// recordedFactoryCall captures (contextID, resumeID) per factory invocation
// so tests can assert the resume protocol.
type recordedFactoryCall struct {
	contextID string
	resumeID  string
}

// newRecordingFactory returns a factory that produces fresh fakePoolSession
// instances and records every call. The returned slice is mutated as the
// pool calls the factory, so test goroutines must synchronize before reading.
func newRecordingFactory() (func(ctx context.Context, contextID, resumeID string) (Session, error), *[]recordedFactoryCall, *[]*fakePoolSession, *sync.Mutex) {
	var mu sync.Mutex
	var calls []recordedFactoryCall
	var sessions []*fakePoolSession
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		s := &fakePoolSession{sessionID: "sess-" + contextID}
		mu.Lock()
		calls = append(calls, recordedFactoryCall{contextID, resumeID})
		sessions = append(sessions, s)
		mu.Unlock()
		return s, nil
	}
	return factory, &calls, &sessions, &mu
}

func TestSessionPool_LookupOrCreate_New(t *testing.T) {
	factory, calls, _, mu := newRecordingFactory()
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	ctx := context.Background()
	s, err := p.LookupOrCreate(ctx, "ctx-1")
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("want non-nil session")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("want 1 factory call, got %d", len(*calls))
	}
	if (*calls)[0].contextID != "ctx-1" {
		t.Errorf("want contextID 'ctx-1', got %q", (*calls)[0].contextID)
	}
	if (*calls)[0].resumeID != "" {
		t.Errorf("want empty resumeID on first lookup, got %q", (*calls)[0].resumeID)
	}
}

// TestSessionPool_LookupOrCreate_ReuseRefreshesTTL verifies that a second
// lookup of the same contextID returns the same Session AND advances the
// entry's lastUsed timestamp so the reaper doesn't evict it prematurely.
func TestSessionPool_LookupOrCreate_ReuseRefreshesTTL(t *testing.T) {
	factory, calls, _, mu := newRecordingFactory()
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	now := time.Now()
	p.setClock(func() time.Time { return now })

	ctx := context.Background()
	s1, err := p.LookupOrCreate(ctx, "ctx-1")
	if err != nil {
		t.Fatal(err)
	}

	// Advance clock 10 minutes (under idleTTL=30min)
	now = now.Add(10 * time.Minute)

	s2, err := p.LookupOrCreate(ctx, "ctx-1")
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Error("want same session reused, got different")
	}
	mu.Lock()
	if len(*calls) != 1 {
		t.Errorf("want 1 factory call, got %d", len(*calls))
	}
	mu.Unlock()

	// Advance another 25 min — total 35min since first lookup, but only 25min
	// since the second lookup refreshed lastUsed. With sliding TTL, must NOT
	// be evicted.
	now = now.Add(25 * time.Minute)
	p.reapOnce()

	s3, err := p.LookupOrCreate(ctx, "ctx-1")
	if err != nil {
		t.Fatal(err)
	}
	if s3 != s1 {
		t.Error("want session preserved across reapOnce (sliding TTL refresh failed)")
	}
}

func TestSessionPool_IdleEviction(t *testing.T) {
	factory, _, sessions, mu := newRecordingFactory()
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	now := time.Now()
	p.setClock(func() time.Time { return now })

	ctx := context.Background()
	if _, err := p.LookupOrCreate(ctx, "ctx-1"); err != nil {
		t.Fatal(err)
	}

	// Advance past idleTTL.
	now = now.Add(31 * time.Minute)
	p.reapOnce()

	mu.Lock()
	s0 := (*sessions)[0]
	mu.Unlock()
	if !s0.isClosed() {
		t.Error("want session Close() called on idle eviction")
	}
}

// TestSessionPool_ResumeAfterEvict: after an entry is evicted, the next
// LookupOrCreate must call the factory with resumeID equal to the original
// sessionID so claude resumes the same conversation.
func TestSessionPool_ResumeAfterEvict(t *testing.T) {
	factory, calls, _, mu := newRecordingFactory()
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	now := time.Now()
	p.setClock(func() time.Time { return now })

	ctx := context.Background()
	if _, err := p.LookupOrCreate(ctx, "ctx-resume"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(31 * time.Minute)
	p.reapOnce()

	if _, err := p.LookupOrCreate(ctx, "ctx-resume"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 2 {
		t.Fatalf("want 2 factory calls (initial + resume), got %d", len(*calls))
	}
	if (*calls)[1].resumeID != "sess-ctx-resume" {
		t.Errorf("want resumeID 'sess-ctx-resume' on second call, got %q", (*calls)[1].resumeID)
	}
}

// TestSessionPool_EvictedSecondaryGC: after evictedTTL passes since
// eviction, the entry is purged so the next lookup is a fresh conversation
// (no --resume).
func TestSessionPool_EvictedSecondaryGC(t *testing.T) {
	factory, calls, _, mu := newRecordingFactory()
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	now := time.Now()
	p.setClock(func() time.Time { return now })

	ctx := context.Background()
	if _, err := p.LookupOrCreate(ctx, "ctx-gc"); err != nil {
		t.Fatal(err)
	}

	// First reap evicts.
	now = now.Add(31 * time.Minute)
	p.reapOnce()

	// 24h+ later, secondary GC purges the entry entirely.
	now = now.Add(25 * time.Hour)
	p.reapOnce()

	// Next lookup must be treated as brand new — resumeID empty.
	if _, err := p.LookupOrCreate(ctx, "ctx-gc"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 2 {
		t.Fatalf("want 2 factory calls, got %d", len(*calls))
	}
	if (*calls)[1].resumeID != "" {
		t.Errorf("want empty resumeID after secondary GC, got %q", (*calls)[1].resumeID)
	}
}

// TestSessionPool_ConcurrentLookupSameContextID verifies the singleflight
// property: N goroutines calling LookupOrCreate for the same contextID
// must result in exactly one factory invocation (otherwise we'd start
// multiple claude processes for the same conversation).
func TestSessionPool_ConcurrentLookupSameContextID(t *testing.T) {
	var factoryCount int32
	var ready sync.WaitGroup
	ready.Add(1)
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		atomic.AddInt32(&factoryCount, 1)
		// Block briefly so concurrent callers pile up on the entry mutex.
		ready.Wait()
		return &fakePoolSession{sessionID: "sess-" + contextID}, nil
	}
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	results := make([]Session, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			s, err := p.LookupOrCreate(context.Background(), "ctx-conc")
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			results[i] = s
		}()
	}
	// Let all goroutines reach the entry, then release factory.
	time.Sleep(50 * time.Millisecond)
	ready.Done()
	wg.Wait()

	if got := atomic.LoadInt32(&factoryCount); got != 1 {
		t.Errorf("want exactly 1 factory call for concurrent same-contextID lookups, got %d", got)
	}
	// All returned Sessions must be the same instance.
	for i := 1; i < N; i++ {
		if results[i] != results[0] {
			t.Errorf("goroutine %d got different Session than goroutine 0", i)
			return
		}
	}
}

// TestSessionPool_Stop verifies that Stop closes all live sessions and
// shuts down the reaper without leaks.
func TestSessionPool_Stop(t *testing.T) {
	factory, _, sessions, mu := newRecordingFactory()
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)

	ctx := context.Background()
	if _, err := p.LookupOrCreate(ctx, "ctx-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.LookupOrCreate(ctx, "ctx-b"); err != nil {
		t.Fatal(err)
	}

	p.Stop()

	mu.Lock()
	defer mu.Unlock()
	for i, s := range *sessions {
		if !s.isClosed() {
			t.Errorf("session %d not closed after Pool.Stop", i)
		}
	}
}

// memPersist is an in-memory Persistence used by SessionPool tests so we can
// observe what the pool writes without touching the filesystem. Captures the
// last successful Save so tests can assert state at quiesce.
type memPersist struct {
	mu      sync.Mutex
	stored  map[string]PersistedRecord
	saves   int
	loadErr error
	loadVal map[string]PersistedRecord
	saveErr error
}

func newMemPersist() *memPersist {
	return &memPersist{stored: map[string]PersistedRecord{}}
}

func (m *memPersist) Load() (map[string]PersistedRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	if m.loadVal != nil {
		out := make(map[string]PersistedRecord, len(m.loadVal))
		for k, v := range m.loadVal {
			out[k] = v
		}
		return out, nil
	}
	return map[string]PersistedRecord{}, nil
}

func (m *memPersist) Save(records map[string]PersistedRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.stored = make(map[string]PersistedRecord, len(records))
	for k, v := range records {
		m.stored[k] = v
	}
	m.saves++
	return nil
}

func (m *memPersist) snapshot() map[string]PersistedRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]PersistedRecord, len(m.stored))
	for k, v := range m.stored {
		out[k] = v
	}
	return out
}

func (m *memPersist) saveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saves
}

// TestSessionPool_PersistsOnCreate verifies the pool writes the new
// (contextID, sessionID) pair to the backend the moment the factory returns.
func TestSessionPool_PersistsOnCreate(t *testing.T) {
	factory, _, _, _ := newRecordingFactory()
	mp := newMemPersist()
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	if _, err := p.LookupOrCreate(context.Background(), "ctx-1"); err != nil {
		t.Fatal(err)
	}
	snap := mp.snapshot()
	got, ok := snap["ctx-1"]
	if !ok {
		t.Fatalf("expected ctx-1 in persisted snapshot, got %v", snap)
	}
	if got.SessionID != "sess-ctx-1" {
		t.Errorf("sessionID: got %q want sess-ctx-1", got.SessionID)
	}
	if got.State != persistStateActive {
		t.Errorf("state: got %q want active", got.State)
	}
}

// TestSessionPool_HotHitDoesNotRewrite verifies the fast path (ACTIVE entry
// reuse) skips persistence — otherwise every request would write the file.
func TestSessionPool_HotHitDoesNotRewrite(t *testing.T) {
	factory, _, _, _ := newRecordingFactory()
	mp := newMemPersist()
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	if _, err := p.LookupOrCreate(context.Background(), "ctx-1"); err != nil {
		t.Fatal(err)
	}
	saves := mp.saveCount()

	for i := 0; i < 5; i++ {
		if _, err := p.LookupOrCreate(context.Background(), "ctx-1"); err != nil {
			t.Fatal(err)
		}
	}
	if mp.saveCount() != saves {
		t.Errorf("hot hits should not trigger persist, but saves went from %d to %d", saves, mp.saveCount())
	}
}

// TestSessionPool_PersistsOnEvict verifies that the reaper's ACTIVE→EVICTED
// transition is mirrored to the persistence layer with state=evicted plus
// the evictedAt timestamp (so the 24h secondary-GC window is recoverable
// after restart).
func TestSessionPool_PersistsOnEvict(t *testing.T) {
	factory, _, _, _ := newRecordingFactory()
	mp := newMemPersist()
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	now := time.Now()
	p.setClock(func() time.Time { return now })

	if _, err := p.LookupOrCreate(context.Background(), "ctx-1"); err != nil {
		t.Fatal(err)
	}
	// Advance past idleTTL and reap.
	now = now.Add(31 * time.Minute)
	p.reapOnce()

	got := mp.snapshot()["ctx-1"]
	if got.State != persistStateEvicted {
		t.Errorf("state after evict: got %q want evicted", got.State)
	}
	if got.SessionID != "sess-ctx-1" {
		t.Errorf("sessionID lost across evict: got %q", got.SessionID)
	}
	if !got.EvictedAt.Equal(now) {
		t.Errorf("evictedAt: got %v want %v", got.EvictedAt, now)
	}
}

// TestSessionPool_RemovesPersistOnGC verifies that secondary GC (24h after
// eviction) also removes the entry from the persistent backend — so a
// restart after a long idle period doesn't try to resume a session that's
// no longer valid.
func TestSessionPool_RemovesPersistOnGC(t *testing.T) {
	factory, _, _, _ := newRecordingFactory()
	mp := newMemPersist()
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	now := time.Now()
	p.setClock(func() time.Time { return now })

	if _, err := p.LookupOrCreate(context.Background(), "ctx-1"); err != nil {
		t.Fatal(err)
	}

	// Evict (30min idle), then secondary-GC (24h after eviction).
	now = now.Add(31 * time.Minute)
	p.reapOnce()
	now = now.Add(25 * time.Hour)
	p.reapOnce()

	if _, ok := mp.snapshot()["ctx-1"]; ok {
		t.Errorf("expected ctx-1 to be deleted from persistence after secondary GC")
	}
}

// TestSessionPool_RehydratesAsEvicted verifies the cold-start path: records
// loaded from disk are placed into the EVICTED state in memory, so the
// next LookupOrCreate of those contextIDs triggers a `--resume` via the
// factory (with the loaded sessionID).
func TestSessionPool_RehydratesAsEvicted(t *testing.T) {
	mp := newMemPersist()
	priorEvicted := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	mp.loadVal = map[string]PersistedRecord{
		"ctx-1": {SessionID: "old-sess-1", State: persistStateActive, LastUsed: priorEvicted},
		"ctx-2": {SessionID: "old-sess-2", State: persistStateEvicted, LastUsed: priorEvicted, EvictedAt: priorEvicted},
	}

	factory, calls, _, mu := newRecordingFactory()
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	// Trigger a resume for ctx-1 — factory should be called with resumeID=old-sess-1.
	if _, err := p.LookupOrCreate(context.Background(), "ctx-1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 1 {
		t.Fatalf("want 1 factory call, got %d", len(*calls))
	}
	if (*calls)[0].resumeID != "old-sess-1" {
		t.Errorf("want resumeID=old-sess-1 (rehydrated), got %q", (*calls)[0].resumeID)
	}
}

// TestSessionPool_RehydratePreservesEvictedAt verifies that the 24h
// secondary-GC clock is preserved across restart — so if the user shut
// down with 23h of idle, then started up, the next reap (within an hour)
// will GC the entry. Otherwise we'd silently extend session validity past
// what claude itself might still resume.
func TestSessionPool_RehydratePreservesEvictedAt(t *testing.T) {
	mp := newMemPersist()
	priorEvictedAt := time.Date(2026, 6, 3, 23, 0, 0, 0, time.UTC) // 25h before "now"
	mp.loadVal = map[string]PersistedRecord{
		"ctx-old": {SessionID: "sid-old", State: persistStateEvicted, EvictedAt: priorEvictedAt},
	}

	factory, calls, _, mu := newRecordingFactory()
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	now := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	p.setClock(func() time.Time { return now })
	p.reapOnce()

	// ctx-old should have been GC'd — next lookup builds a fresh session
	// with empty resumeID.
	if _, err := p.LookupOrCreate(context.Background(), "ctx-old"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if (*calls)[0].resumeID != "" {
		t.Errorf("expected fresh session after evictedTTL exceeded, got resumeID=%q", (*calls)[0].resumeID)
	}
}

// TestSessionPool_LoadErrorStartsEmpty: if Load returns an error (e.g. I/O
// failure), the pool must still come up empty rather than failing to
// construct. The agent is more useful with a forgotten session history
// than with no agent at all.
func TestSessionPool_LoadErrorStartsEmpty(t *testing.T) {
	mp := newMemPersist()
	mp.loadErr = errLoadFailed
	factory, _, _, _ := newRecordingFactory()
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	// Pool came up; LookupOrCreate works as a fresh contextID (no resume).
	if _, err := p.LookupOrCreate(context.Background(), "ctx-1"); err != nil {
		t.Fatalf("LookupOrCreate after Load error: %v", err)
	}
}

// unhealthyableSession is a fakePoolSession variant whose health can be
// flipped to simulate a dead claude process (e.g. SIGKILL'd by an operator).
type unhealthyableSession struct {
	fakePoolSession
	healthMu sync.Mutex
	healthy  bool
}

func (u *unhealthyableSession) IsHealthy() bool {
	u.healthMu.Lock()
	defer u.healthMu.Unlock()
	if u.fakePoolSession.isClosed() {
		return false
	}
	return u.healthy
}

func (u *unhealthyableSession) setHealthy(b bool) {
	u.healthMu.Lock()
	defer u.healthMu.Unlock()
	u.healthy = b
}

// TestSessionPool_RecreatesOnUnhealthy is the HA regression: when a cached
// session reports IsHealthy()==false (e.g. underlying claude was kill -9'd),
// the pool must transparently close it, recreate via the factory with the
// preserved sessionID as resumeID, and return the fresh session to the
// caller. The next user request thus survives a dead subprocess without
// losing conversation state.
func TestSessionPool_RecreatesOnUnhealthy(t *testing.T) {
	var (
		mu          sync.Mutex
		factoryHits []string // captures resumeID per call
		instances   []*unhealthyableSession
	)
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		s := &unhealthyableSession{
			fakePoolSession: fakePoolSession{sessionID: "sid-stable"},
			healthy:         true,
		}
		mu.Lock()
		factoryHits = append(factoryHits, resumeID)
		instances = append(instances, s)
		mu.Unlock()
		return s, nil
	}
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	ctx := context.Background()
	s1, err := p.LookupOrCreate(ctx, "ctx-ha")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the underlying claude process being killed externally.
	instances[0].setHealthy(false)

	// Same contextID, same scheduler — pool must detect, recreate, resume.
	s2, err := p.LookupOrCreate(ctx, "ctx-ha")
	if err != nil {
		t.Fatalf("recreate after unhealthy: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(factoryHits) != 2 {
		t.Fatalf("want 2 factory calls (initial + recreate), got %d", len(factoryHits))
	}
	if factoryHits[1] != "sid-stable" {
		t.Errorf("recreate should pass resumeID=sid-stable, got %q", factoryHits[1])
	}
	if s2 == s1 {
		t.Error("recreate should return a NEW Session, not the dead one")
	}
	if !instances[0].fakePoolSession.isClosed() {
		t.Error("dead session should have been Close()'d before recreate")
	}
}

// TestSessionPool_HealthySessionStillReused is the companion: when the
// cached session is healthy, the pool MUST NOT call the factory again —
// reuse stays the fast path.
func TestSessionPool_HealthySessionStillReused(t *testing.T) {
	var (
		mu       sync.Mutex
		hits     int
	)
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		mu.Lock()
		hits++
		mu.Unlock()
		return &unhealthyableSession{
			fakePoolSession: fakePoolSession{sessionID: "sid-reuse"},
			healthy:         true,
		}, nil
	}
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	ctx := context.Background()
	s1, _ := p.LookupOrCreate(ctx, "ctx-reuse")
	s2, _ := p.LookupOrCreate(ctx, "ctx-reuse")
	s3, _ := p.LookupOrCreate(ctx, "ctx-reuse")

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("want 1 factory call across 3 lookups on healthy session, got %d", hits)
	}
	if s1 != s2 || s2 != s3 {
		t.Errorf("expected identical sessions across reuses")
	}
}

var errLoadFailed = errSentinel("load failed")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// delayedSessionIDSession models the real ClaudeSession behaviour: at
// construction time SessionID() is empty, and only becomes non-empty later
// when the underlying runtime delivers an `init` event. Tests use
// deliverSessionID to simulate that asynchronous event.
type delayedSessionIDSession struct {
	contextID string

	mu        sync.Mutex
	sessionID string
	callback  func(string)
	closed    bool
}

func (d *delayedSessionIDSession) Stream(ctx context.Context, userText string) (<-chan Event, error) {
	ch := make(chan Event, 1)
	ch <- EventTurnDone{}
	close(ch)
	return ch, nil
}
func (d *delayedSessionIDSession) Turn(ctx context.Context, userText string) (string, error) {
	return "", nil
}
func (d *delayedSessionIDSession) SessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessionID
}
func (d *delayedSessionIDSession) IsHealthy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.closed
}
func (d *delayedSessionIDSession) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

// OnSessionIDKnown is the interface the pool uses to learn about sessionID
// arrival on async-sessionID-delivery backends (ClaudeSession, here).
func (d *delayedSessionIDSession) OnSessionIDKnown(fn func(string)) {
	d.mu.Lock()
	sid := d.sessionID
	d.callback = fn
	d.mu.Unlock()
	// If the sessionID happens to already be known, fire immediately so the
	// pool doesn't have to special-case it.
	if sid != "" && fn != nil {
		fn(sid)
	}
}

// deliverSessionID simulates claude's init event arriving: sets sessionID
// and invokes the registered callback. Idempotent on subsequent calls.
func (d *delayedSessionIDSession) deliverSessionID(sid string) {
	d.mu.Lock()
	if d.sessionID != "" {
		d.mu.Unlock()
		return
	}
	d.sessionID = sid
	cb := d.callback
	d.mu.Unlock()
	if cb != nil {
		cb(sid)
	}
}

// TestSessionPool_PersistsAfterSessionIDArrives is the regression test for
// the "empty sessionId on disk" bug — Real ClaudeSession reports SessionID()
// as "" until claude's init event arrives. The pool must NOT freeze that
// empty value into the persistence file at LookupOrCreate; it must wait
// for OnSessionIDKnown and only then persist.
//
// Original bug: scheduler created a session, persisted contextID with
// sessionId="", then init arrived and updated only in-memory state — leaving
// the file with an unusable empty record across restarts.
func TestSessionPool_PersistsAfterSessionIDArrives(t *testing.T) {
	mp := newMemPersist()
	var mu sync.Mutex
	var created []*delayedSessionIDSession
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		s := &delayedSessionIDSession{contextID: contextID}
		mu.Lock()
		created = append(created, s)
		mu.Unlock()
		return s, nil
	}
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	if _, err := p.LookupOrCreate(context.Background(), "ctx-async"); err != nil {
		t.Fatal(err)
	}

	// Pre-condition: factory returned a session with sessionID="" — pool
	// must NOT have persisted an empty value.
	if rec, ok := mp.snapshot()["ctx-async"]; ok && rec.SessionID == "" {
		t.Errorf("pool persisted empty sessionID for ctx-async (record=%+v) — should wait for init", rec)
	}

	// Simulate the init event arriving asynchronously.
	mu.Lock()
	s := created[0]
	mu.Unlock()
	s.deliverSessionID("real-sid-async")

	// Now persistence must reflect the real sessionID.
	rec, ok := mp.snapshot()["ctx-async"]
	if !ok {
		t.Fatalf("expected ctx-async persisted after sessionID delivered, snapshot: %v", mp.snapshot())
	}
	if rec.SessionID != "real-sid-async" {
		t.Errorf("persisted sessionID after delivery: got %q want real-sid-async", rec.SessionID)
	}
	if rec.State != persistStateActive {
		t.Errorf("state after delivery: got %q want active", rec.State)
	}
}

// TestSessionPool_FactoryGetsLongLivedCtx is the regression test for the
// "session reuse fails within one process" bug. Root cause: the pool used
// to pass the *per-request* ctx through to the factory, which propagated to
// exec.CommandContext, which killed claude the moment the A2A response was
// sent. Subsequent same-contextID requests then hit an EVICTED session.
//
// The contract: factory receives a ctx whose lifetime is the SessionPool's,
// not any individual LookupOrCreate caller's. Cancelling the request ctx
// must NOT cancel the factory ctx — the resulting claude process needs to
// outlive a single HTTP turn so the next request on the same contextID can
// reuse it.
func TestSessionPool_FactoryGetsLongLivedCtx(t *testing.T) {
	var (
		mu          sync.Mutex
		capturedCtx context.Context
	)
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		mu.Lock()
		capturedCtx = ctx
		mu.Unlock()
		return &fakePoolSession{sessionID: "sess-" + contextID}, nil
	}
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)
	defer p.Stop()

	reqCtx, reqCancel := context.WithCancel(context.Background())
	if _, err := p.LookupOrCreate(reqCtx, "ctx-survive"); err != nil {
		t.Fatal(err)
	}
	// Simulate the A2A handler returning: per-request ctx ends.
	reqCancel()

	mu.Lock()
	got := capturedCtx
	mu.Unlock()
	if got == nil {
		t.Fatal("factory was not called or did not receive a ctx")
	}
	select {
	case <-got.Done():
		t.Errorf("factory ctx cancelled when request ctx was cancelled: %v — sessions must outlive requests", got.Err())
	default:
		// Good — ctx still alive.
	}
}

// TestSessionPool_FactoryCtxCancelledOnStop verifies the other end of the
// contract: when the pool is stopped (e.g. agent shutting down), the
// long-lived factory ctx must be cancelled so any leaked claude processes
// get reaped via exec.CommandContext's SIGKILL.
func TestSessionPool_FactoryCtxCancelledOnStop(t *testing.T) {
	var (
		mu          sync.Mutex
		capturedCtx context.Context
	)
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		mu.Lock()
		capturedCtx = ctx
		mu.Unlock()
		return &fakePoolSession{sessionID: "sess-" + contextID}, nil
	}
	p := NewSessionPool(factory, 30*time.Minute, 24*time.Hour)

	if _, err := p.LookupOrCreate(context.Background(), "ctx-1"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := capturedCtx
	mu.Unlock()

	p.Stop()

	select {
	case <-got.Done():
		// Good — factory ctx now cancelled.
	case <-time.After(2 * time.Second):
		t.Errorf("factory ctx not cancelled within 2s after pool.Stop")
	}
}

// TestSessionPool_ResumeKeepsPriorSessionIDDuringInit verifies the resume
// case: rehydrate gives entry.sessionID="X", factory creates a fresh session
// (sessionID="" until --resume init confirms). Pool must NOT overwrite
// entry.sessionID with empty during the window between LookupOrCreate and
// init arrival — otherwise a crash in that window would lose the resume id.
func TestSessionPool_ResumeKeepsPriorSessionIDDuringInit(t *testing.T) {
	mp := newMemPersist()
	priorTS := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	mp.loadVal = map[string]PersistedRecord{
		"ctx-resume": {SessionID: "old-sid", State: persistStateEvicted, EvictedAt: priorTS},
	}

	var mu sync.Mutex
	var created []*delayedSessionIDSession
	var seenResumeID string
	factory := func(ctx context.Context, contextID, resumeID string) (Session, error) {
		s := &delayedSessionIDSession{contextID: contextID}
		mu.Lock()
		created = append(created, s)
		seenResumeID = resumeID
		mu.Unlock()
		return s, nil
	}
	p := NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, mp)
	defer p.Stop()

	if _, err := p.LookupOrCreate(context.Background(), "ctx-resume"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	gotResume := seenResumeID
	mu.Unlock()
	if gotResume != "old-sid" {
		t.Errorf("factory resumeID: got %q want old-sid", gotResume)
	}

	rec, ok := mp.snapshot()["ctx-resume"]
	if !ok {
		t.Fatalf("expected ctx-resume in persisted snapshot during resume window")
	}
	if rec.SessionID != "old-sid" {
		t.Errorf("persisted sessionID during resume: got %q want old-sid (must NOT be overwritten with empty)", rec.SessionID)
	}
}
