package wrapper

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// PersistedRecord is the persistent slice of a pooledEntry — the bits that
// must survive across ahsir-agent restarts so the next session for the same
// contextID can `claude --resume` instead of starting fresh.
//
// The in-memory `*Session` (the live claude process) is intentionally absent:
// it never survives a restart, so on load every record is rehydrated as
// EVICTED — the next LookupOrCreate will go through the factory with
// resumeID=SessionID and trigger `--resume` naturally.
type PersistedRecord struct {
	SessionID string    `json:"sessionId"`
	State     string    `json:"state"` // "active" | "evicted"
	LastUsed  time.Time `json:"lastUsed,omitempty"`
	EvictedAt time.Time `json:"evictedAt,omitempty"`
}

// Persistence is the storage backend SessionPool consults to remember
// contextID → sessionID mappings across process restarts.
//
// Implementations must be safe for sequential calls from one goroutine
// (SessionPool serializes Save / Load through persistMu).
type Persistence interface {
	// Load returns the records seen on disk, or an empty map if no prior
	// state exists. A corrupt or unreadable file should be treated as
	// "empty" — the agent must always be able to start. Implementations
	// are free to side-effect (e.g. rename the broken file) but must not
	// surface the corruption as a fatal error to the caller.
	Load() (map[string]PersistedRecord, error)
	// Save atomically replaces stored state with the given snapshot.
	Save(records map[string]PersistedRecord) error
}

const (
	persistStateActive  = "active"
	persistStateEvicted = "evicted"
)

// sessionIDNotifier is an optional capability some Session backends implement
// to tell the pool when the runtime delivers its session_id. Real
// ClaudeSession can't report SessionID() at construction (it has to wait
// for the first init event), so the pool registers a callback after factory
// returns and only persists once the value arrives. Backends that know
// their sessionID synchronously (OneshotSession, test fakes) don't need to
// implement this — the pool reads SessionID() directly.
type sessionIDNotifier interface {
	OnSessionIDKnown(func(string))
}

// SessionPool keeps one long-running Session per A2A contextID. Idle entries
// are evicted by a background reaper after idleTTL; their sessionID is
// retained so the next access can resume the same conversation via the
// factory's resumeID parameter. EVICTED entries are purged from memory
// after evictedTTL has elapsed since their eviction.
type SessionPool struct {
	factory    func(ctx context.Context, contextID, resumeID string) (Session, error)
	idleTTL    time.Duration
	evictedTTL time.Duration

	mu      sync.Mutex // protects entries map + clock + stopped
	entries map[string]*pooledEntry
	clock   func() time.Time
	stopped bool

	stop       chan struct{}
	reaperDone chan struct{}

	// sessionCtx is the ctx passed to the factory — its lifetime is the
	// pool's, NOT any individual LookupOrCreate caller's. Critical because
	// the factory typically forwards this ctx to exec.CommandContext when
	// spawning claude; if we used the per-request ctx, claude would be
	// killed by SIGKILL the moment the A2A handler returns, and the next
	// request on the same contextID would hit an EVICTED session. Cancelled
	// on Stop so any orphaned subprocesses get reaped on shutdown.
	sessionCtx       context.Context
	sessionCtxCancel context.CancelFunc

	// persistMu serializes Save calls and protects persistState. Held only
	// during snapshot copy + Save invocation — never nested with mu or
	// entry.mu, so it cannot deadlock the hot path.
	persistMu    sync.Mutex
	persistState map[string]PersistedRecord
	persist      Persistence

	// Capacity controls. Set via SetCap (or left at zero values for
	// unlimited). Consulted on every LookupOrCreate that would otherwise
	// create or resume an entry — when there are already maxActive entries
	// in the ACTIVE state, the policy decides whether to reject the
	// request or evict the LRU one. EVICTED entries do NOT count toward
	// the cap (they hold only a sessionID, no live process).
	//
	// maxActive == 0 means unlimited (the historical behaviour).
	maxActive      int
	overloadPolicy OverloadPolicy
}

// OverloadPolicy selects what SessionPool does when LookupOrCreate would
// push the count of ACTIVE entries beyond maxActive.
type OverloadPolicy int

const (
	// OverloadReject returns an error to the caller and keeps existing
	// sessions untouched. Honest about the limit; the caller decides
	// whether to retry, queue, or surface the failure.
	OverloadReject OverloadPolicy = iota

	// OverloadEvictLRU transitions the least-recently-used ACTIVE entry
	// to EVICTED (kills its claude process, retains sessionID for
	// possible later --resume), freeing a slot for the new entry. Useful
	// when callers prefer "always succeed" semantics and can tolerate an
	// older conversation being unceremoniously dropped to make room.
	OverloadEvictLRU
)

func (p OverloadPolicy) String() string {
	switch p {
	case OverloadReject:
		return "reject"
	case OverloadEvictLRU:
		return "evict-lru"
	default:
		return fmt.Sprintf("unknown(%d)", int(p))
	}
}

// ParseOverloadPolicy converts the human-readable form used in agent-card.yaml
// into the typed enum. Empty string returns the default (OverloadReject) —
// makes the config field optional.
func ParseOverloadPolicy(s string) (OverloadPolicy, error) {
	switch s {
	case "", "reject":
		return OverloadReject, nil
	case "evict-lru":
		return OverloadEvictLRU, nil
	default:
		return OverloadReject, fmt.Errorf("unknown overload_policy %q (want reject or evict-lru)", s)
	}
}

type entryState int

const (
	entryActive entryState = iota
	entryEvicted
)

// pooledEntry holds one per-contextID slot. mu serializes factory calls
// and state mutations for this contextID so concurrent LookupOrCreate on
// the same contextID produce only one Session.
type pooledEntry struct {
	contextID string
	sessionID string // remembered across eviction for --resume

	mu        sync.Mutex
	state     entryState
	session   Session
	lastUsed  time.Time
	evictedAt time.Time
}

const defaultReapInterval = 1 * time.Minute

// NewSessionPool starts the background reaper. Stop must be called to
// release resources.
func NewSessionPool(factory func(ctx context.Context, contextID, resumeID string) (Session, error), idleTTL, evictedTTL time.Duration) *SessionPool {
	return NewSessionPoolWithPersistence(factory, idleTTL, evictedTTL, nil)
}

// NewSessionPoolWithPersistence is like NewSessionPool but reads prior
// contextID → sessionID state from the given backend on startup so a
// restarted agent can `claude --resume` the conversations it had before.
// Pass nil for `persist` to disable persistence.
//
// Load failures are logged but never fatal — an agent must always be able
// to start, even if its prior state file is corrupt. In that case the pool
// starts empty, as if no persistent state existed.
func NewSessionPoolWithPersistence(factory func(ctx context.Context, contextID, resumeID string) (Session, error), idleTTL, evictedTTL time.Duration, persist Persistence) *SessionPool {
	sessionCtx, cancel := context.WithCancel(context.Background())
	p := &SessionPool{
		factory:          factory,
		idleTTL:          idleTTL,
		evictedTTL:       evictedTTL,
		entries:          make(map[string]*pooledEntry),
		clock:            time.Now,
		stop:             make(chan struct{}),
		reaperDone:       make(chan struct{}),
		sessionCtx:       sessionCtx,
		sessionCtxCancel: cancel,
		persist:          persist,
		persistState:     make(map[string]PersistedRecord),
	}
	if persist != nil {
		records, err := persist.Load()
		if err != nil {
			log.Printf("session pool: persist load failed, starting empty: %v", err)
		} else {
			p.rehydrateLocked(records)
		}
	}
	go p.reaperLoop()
	return p
}

// rehydrateLocked seeds the in-memory entries map from a loaded snapshot.
// Every loaded entry is forced into the EVICTED state — the live claude
// process from the prior run is gone, but its sessionID survives so the
// next LookupOrCreate will go through `--resume`. Called only from the
// constructor (single-threaded), so no locking is needed.
func (p *SessionPool) rehydrateLocked(records map[string]PersistedRecord) {
	now := p.clock()
	for ctxID, rec := range records {
		if rec.SessionID == "" {
			// Defensive: an entry without a sessionID has no value (can't
			// resume) — drop it.
			continue
		}
		evictedAt := rec.EvictedAt
		if evictedAt.IsZero() {
			// Previously ACTIVE at shutdown — start its evictedTTL window now.
			evictedAt = now
		}
		entry := &pooledEntry{
			contextID: ctxID,
			sessionID: rec.SessionID,
			state:     entryEvicted,
			lastUsed:  rec.LastUsed,
			evictedAt: evictedAt,
		}
		p.entries[ctxID] = entry
		p.persistState[ctxID] = PersistedRecord{
			SessionID: rec.SessionID,
			State:     persistStateEvicted,
			LastUsed:  rec.LastUsed,
			EvictedAt: evictedAt,
		}
	}
}

// SetCap configures the maximum number of ACTIVE entries this pool will
// hold concurrently. max == 0 means unlimited (the default, preserving
// the historical behaviour). policy decides what to do when the cap is
// reached on a new LookupOrCreate:
//
//   - OverloadReject:    return an error to the caller; existing sessions
//     are untouched. Honest about the limit.
//   - OverloadEvictLRU:  evict the least-recently-used ACTIVE entry so the
//     new one can take its slot. The victim's sessionID
//     is preserved (the EVICTED→Resume path still works
//     if the displaced contextID comes back).
//
// Safe to call at any point after construction — the cap is consulted on
// every LookupOrCreate, not just at startup. EVICTED entries (no live
// process) do not count toward the cap.
func (p *SessionPool) SetCap(max int, policy OverloadPolicy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxActive = max
	p.overloadPolicy = policy
}

// setClock injects a fake clock for tests. Production uses time.Now.
func (p *SessionPool) setClock(fn func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clock = fn
}

// LookupOrCreate returns the Session for contextID, creating it (or
// resuming it if the entry was previously evicted) if necessary. Concurrent
// calls for the same contextID are serialized so the factory runs exactly
// once.
func (p *SessionPool) LookupOrCreate(ctx context.Context, contextID string) (Session, error) {
	started := time.Now()
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, errors.New("session pool: stopped")
	}
	entry, ok := p.entries[contextID]
	if !ok {
		entry = &pooledEntry{contextID: contextID}
		p.entries[contextID] = entry
	}
	now := p.clock()
	p.mu.Unlock()

	entry.mu.Lock()

	if entry.state == entryActive && entry.session != nil {
		if entry.session.IsHealthy() {
			entry.lastUsed = now
			s := entry.session
			sid := entry.sessionID
			entry.mu.Unlock()
			// Hot-path hit: lastUsed change is in-memory only — we don't
			// persist it because that would write the file on every request.
			// After restart all entries come back as EVICTED anyway, so the
			// missing lastUsed bump is harmless.
			log.Printf("session pool: lookup contextID=%s outcome=hit state=active sessionID=%s took=%v", contextID, truncateForLog(sid, 80), time.Since(started))
			return s, nil
		}
		// Cached session is dead (e.g. claude was kill -9'd, or `-p` stream
		// closed before we kept the process alive). Close the zombie and
		// fall through to the recreate path — entry.sessionID is preserved
		// so the factory will pass it as resumeID, letting `claude --resume`
		// pick up the conversation from its on-disk jsonl.
		_ = entry.session.Close()
		entry.session = nil
		entry.state = entryEvicted
		entry.evictedAt = now
		log.Printf("session pool: lookup contextID=%s outcome=unhealthy_evicted sessionID=%s took=%v", contextID, truncateForLog(entry.sessionID, 80), time.Since(started))
	}

	// About to create or resume an entry — this is the first chance to
	// enforce the cap, since hot-path hits above don't change the active
	// count. Skip when maxActive==0 (unlimited, the historical default).
	if p.maxActive > 0 {
		capStarted := time.Now()
		if err := p.enforceCap(contextID); err != nil {
			entry.mu.Unlock()
			log.Printf("session pool: lookup contextID=%s outcome=capacity_reject max_active=%d policy=%s took=%v cap_check=%v err=%v", contextID, p.maxActive, p.overloadPolicy, time.Since(started), time.Since(capStarted), err)
			return nil, err
		}
		log.Printf("session pool: cap_check contextID=%s max_active=%d policy=%s took=%v", contextID, p.maxActive, p.overloadPolicy, time.Since(capStarted))
	}

	// Fresh entry (sessionID=="") or EVICTED (sessionID preserved).
	resumeID := entry.sessionID
	outcome := "create"
	if resumeID != "" {
		outcome = "resume"
	}
	// Use the pool's long-lived ctx for the factory, NOT the per-request
	// ctx — see sessionCtx field doc for the underlying reason.
	factoryStarted := time.Now()
	s, err := p.factory(p.sessionCtx, contextID, resumeID)
	factoryTook := time.Since(factoryStarted)
	if err != nil {
		entry.mu.Unlock()
		log.Printf("session pool: lookup contextID=%s outcome=%s_failed resumeID=%s factory=%v took=%v err=%v", contextID, outcome, truncateForLog(resumeID, 80), factoryTook, time.Since(started), err)
		return nil, err
	}
	entry.session = s
	entry.state = entryActive
	entry.lastUsed = now

	// Two flavours of sessionID delivery, handled differently:
	//
	//   1. Notifier-backed (real ClaudeSession): SessionID is empty at
	//      construction and only arrives later via the first `init` event.
	//      Reading s.SessionID() here would give "" and freeze a useless
	//      record into the persistence file. Instead we leave entry.sessionID
	//      alone (preserving the prior resume id, if any) and wait for the
	//      callback to fill it in.
	//
	//   2. Synchronous (OneshotSession, test fakes): SessionID is final at
	//      construction, so we can read it directly.
	notifier, hasNotifier := s.(sessionIDNotifier)
	if !hasNotifier {
		entry.sessionID = s.SessionID()
	}
	sid := entry.sessionID
	lastUsed := entry.lastUsed
	entry.mu.Unlock()

	if hasNotifier {
		// Capture session identity so a late-firing callback from a session
		// that's already been replaced doesn't corrupt the new entry.
		owner := s
		notifier.OnSessionIDKnown(func(newSID string) {
			p.handleSessionIDKnown(contextID, owner, newSID)
		})
	}

	// Persist only if we already have a real sessionID:
	//   - resume path: sid == prior sessionID (non-empty) → persist now so a
	//     crash before init arrives doesn't lose the resume target
	//   - sync session: sid == s.SessionID() (non-empty) → persist now
	//   - fresh notifier session: sid == "" → don't persist; the callback
	//     above will persist when init lands
	if sid != "" {
		p.upsertPersist(contextID, PersistedRecord{
			SessionID: sid,
			State:     persistStateActive,
			LastUsed:  lastUsed,
		})
	}
	log.Printf("session pool: lookup contextID=%s outcome=%s resumeID=%s sessionID=%s notifier=%t factory=%v took=%v", contextID, outcome, truncateForLog(resumeID, 80), truncateForLog(sid, 80), hasNotifier, factoryTook, time.Since(started))
	return s, nil
}

// enforceCap is called from LookupOrCreate when a fresh/EVICTED entry is
// about to become ACTIVE — i.e. the only path that grows the count of
// running claude processes. Counts current ACTIVE entries (excluding the
// caller's own contextID, which doesn't yet hold a live session); if the
// count is already at p.maxActive, applies the configured overload
// policy:
//
//   - OverloadReject:    return an error; caller surfaces to user
//   - OverloadEvictLRU:  transition the LRU ACTIVE entry to EVICTED so a
//     slot opens up, then return nil
//
// Held without p.mu; acquires it internally for the scan + (for LRU) for
// the eviction. The caller holds the per-entry lock for `contextID` so
// concurrent LookupOrCreate on a different contextID may race with the
// counting, but worst case is a one-over-cap excursion under heavy
// concurrency, which is acceptable for this kind of soft limit.
func (p *SessionPool) enforceCap(contextID string) error {
	p.mu.Lock()
	var (
		active      int
		lruEntry    *pooledEntry
		lruLastUsed time.Time
	)
	for k, e := range p.entries {
		if k == contextID {
			continue // self — about to become active, doesn't count yet
		}
		// Read state without grabbing entry.mu: state mutations are rare
		// and any racy read either over- or under-counts by at most one,
		// which the cap can tolerate (soft limit).
		if e.state == entryActive && e.session != nil {
			active++
			if lruEntry == nil || e.lastUsed.Before(lruLastUsed) {
				lruEntry = e
				lruLastUsed = e.lastUsed
			}
		}
	}
	p.mu.Unlock()

	if active < p.maxActive {
		return nil
	}

	switch p.overloadPolicy {
	case OverloadReject:
		return fmt.Errorf("session pool at capacity (%d active); retry after a session idles out (idleTTL=%v) or raise pool.max_active", p.maxActive, p.idleTTL)
	case OverloadEvictLRU:
		if lruEntry == nil {
			// Shouldn't happen — if active >= max > 0, at least one entry exists.
			return nil
		}
		p.evictForCap(lruEntry)
		return nil
	default:
		return fmt.Errorf("unknown overload policy %v", p.overloadPolicy)
	}
}

// evictForCap transitions one ACTIVE entry to EVICTED to free a slot for
// a new ACTIVE entry. Mirrors the ACTIVE→EVICTED transition in reapOnce
// (close session, retain sessionID for later --resume, persist the new
// state) but is triggered by capacity pressure rather than idle TTL.
//
// Caller must NOT be holding p.mu or victim.mu.
func (p *SessionPool) evictForCap(victim *pooledEntry) {
	now := p.clock()
	victim.mu.Lock()
	if victim.state != entryActive || victim.session == nil {
		// Already evicted between our scan and now; nothing to do.
		victim.mu.Unlock()
		return
	}
	log.Printf("session pool: evicting LRU contextID=%s (lastUsed=%s) to free a cap slot", victim.contextID, victim.lastUsed.Format(time.RFC3339))
	_ = victim.session.Close()
	victim.session = nil
	victim.state = entryEvicted
	victim.evictedAt = now
	rec := PersistedRecord{
		SessionID: victim.sessionID,
		State:     persistStateEvicted,
		LastUsed:  victim.lastUsed,
		EvictedAt: now,
	}
	contextID := victim.contextID
	victim.mu.Unlock()
	p.upsertPersist(contextID, rec)
}

// handleSessionIDKnown is the post-init callback wired into notifier-backed
// sessions. It updates entry.sessionID and triggers a persist, BUT only if
// the live entry still owns the session we attached to — late callbacks
// from sessions that have since been evicted / replaced must not corrupt
// the current state.
func (p *SessionPool) handleSessionIDKnown(contextID string, owner Session, sessionID string) {
	if sessionID == "" {
		return
	}
	started := time.Now()
	p.mu.Lock()
	entry, ok := p.entries[contextID]
	p.mu.Unlock()
	if !ok {
		return
	}

	entry.mu.Lock()
	if entry.session != owner {
		// Session has been replaced (e.g. by a resume after re-eviction).
		// Drop this notification — it would corrupt the live entry's id.
		entry.mu.Unlock()
		return
	}
	if entry.sessionID == sessionID {
		entry.mu.Unlock()
		return
	}
	entry.sessionID = sessionID
	lastUsed := entry.lastUsed
	entry.mu.Unlock()

	p.upsertPersist(contextID, PersistedRecord{
		SessionID: sessionID,
		State:     persistStateActive,
		LastUsed:  lastUsed,
	})
	log.Printf("session pool: session_id_known contextID=%s sessionID=%s took=%v", contextID, truncateForLog(sessionID, 80), time.Since(started))
}

// upsertPersist mirrors one entry's persistent state into persistState and
// triggers an atomic file write. Must be called with NO entry locks held
// (it acquires persistMu, never entry.mu).
func (p *SessionPool) upsertPersist(contextID string, rec PersistedRecord) {
	if p.persist == nil {
		return
	}
	p.persistMu.Lock()
	p.persistState[contextID] = rec
	snap := copyPersistMap(p.persistState)
	p.persistMu.Unlock()
	if err := p.persist.Save(snap); err != nil {
		log.Printf("session pool: persist save failed for contextID=%s: %v", contextID, err)
	}
}

// removePersist forgets one contextID from the persistent map. Used on the
// 24h evictedTTL GC path.
func (p *SessionPool) removePersist(contextID string) {
	if p.persist == nil {
		return
	}
	p.persistMu.Lock()
	delete(p.persistState, contextID)
	snap := copyPersistMap(p.persistState)
	p.persistMu.Unlock()
	if err := p.persist.Save(snap); err != nil {
		log.Printf("session pool: persist save failed on delete contextID=%s: %v", contextID, err)
	}
}

func copyPersistMap(m map[string]PersistedRecord) map[string]PersistedRecord {
	out := make(map[string]PersistedRecord, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (p *SessionPool) reaperLoop() {
	defer close(p.reaperDone)
	ticker := time.NewTicker(defaultReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.reapOnce()
		}
	}
}

// reapOnce sweeps:
//   - ACTIVE entries past idleTTL → close session, transition to EVICTED
//   - EVICTED entries past evictedTTL → delete from map (forgets sessionID)
//
// Tests drive this directly; the production reaper goroutine calls it on
// its ticker.
func (p *SessionPool) reapOnce() {
	p.mu.Lock()
	now := p.clock()
	snapshot := make([]*pooledEntry, 0, len(p.entries))
	for _, e := range p.entries {
		snapshot = append(snapshot, e)
	}
	p.mu.Unlock()

	var toDelete []string
	// Track evictions so we can persist after releasing entry.mu (persist
	// path uses persistMu which must never be nested inside entry.mu).
	type evictedPersist struct {
		contextID string
		rec       PersistedRecord
	}
	var evicted []evictedPersist
	for _, e := range snapshot {
		e.mu.Lock()
		switch e.state {
		case entryActive:
			if e.session != nil && now.Sub(e.lastUsed) > p.idleTTL {
				_ = e.session.Close()
				e.session = nil
				e.state = entryEvicted
				e.evictedAt = now
				evicted = append(evicted, evictedPersist{
					contextID: e.contextID,
					rec: PersistedRecord{
						SessionID: e.sessionID,
						State:     persistStateEvicted,
						LastUsed:  e.lastUsed,
						EvictedAt: now,
					},
				})
			}
		case entryEvicted:
			if now.Sub(e.evictedAt) > p.evictedTTL {
				toDelete = append(toDelete, e.contextID)
			}
		}
		e.mu.Unlock()
	}
	for _, ev := range evicted {
		p.upsertPersist(ev.contextID, ev.rec)
	}

	if len(toDelete) > 0 {
		p.mu.Lock()
		for _, k := range toDelete {
			// Re-check under entry.mu to avoid deleting an entry that
			// got resumed between snapshot and this point.
			if e, ok := p.entries[k]; ok {
				e.mu.Lock()
				if e.state == entryEvicted {
					delete(p.entries, k)
				}
				e.mu.Unlock()
			}
		}
		p.mu.Unlock()
		for _, k := range toDelete {
			p.removePersist(k)
		}
	}
}

// Stop shuts down the reaper and closes all live sessions. Idempotent.
func (p *SessionPool) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.stop)
	p.mu.Unlock()
	<-p.reaperDone

	p.mu.Lock()
	entries := p.entries
	p.entries = make(map[string]*pooledEntry)
	p.mu.Unlock()

	for _, e := range entries {
		e.mu.Lock()
		if e.session != nil {
			_ = e.session.Close()
			e.session = nil
		}
		e.mu.Unlock()
	}

	// Cancel the factory ctx LAST: by this point every Session has had Close
	// called on it (which kills the underlying claude process via its own
	// transport.kill). The ctx cancel is just a belt-and-suspenders cleanup
	// for any process that managed to leak past Close — when ctx cancels,
	// exec.CommandContext sends SIGKILL.
	if p.sessionCtxCancel != nil {
		p.sessionCtxCancel()
	}
}
