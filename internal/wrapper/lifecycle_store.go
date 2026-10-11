package wrapper

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Persisted Agent Session lifecycle state (issue #43).
//
// LifecycleStore is the durable record of ahsir-owned Session lifecycle work:
// the one-time initialization marker (keyed by lifecycle version), the current
// lifecycle phase, an ordered bounded event log, and the last typed terminal
// outcome per Session. It lives in the agent workspace (.a2a/lifecycle.json)
// next to sessions.json so phase, attempt identity, initialization marker, and
// outcomes survive ahsir restarts and scale-to-zero/wake cycles.
//
// Safety: events and outcomes never carry secrets, environment values, or
// authorization headers — only bounded, whitespace-normalized detail strings
// produced by the lifecycle controller itself. Provider output is stored only
// as the bounded last-message excerpt.

// Lifecycle event types. Exact names are part of the observable contract —
// asynchronous clients correlate them by session_id + turn_id.
const (
	EvtSessionInitializing         = "session.initializing"
	EvtSessionInitialized          = "session.initialized"
	EvtSessionInitializationFailed = "session.initialization_failed"
	EvtTurnRunning                 = "turn.running"
	EvtTurnSoftDeadline            = "turn.soft_deadline"
	EvtTurnFinalizing              = "turn.finalizing"
	EvtTurnFinalized               = "turn.finalized"
	EvtTurnFinalizationFailed      = "turn.finalization_failed"
	// EvtTurnOutcome is the terminal record of a turn (business or
	// lifecycle). Its Kind field is the machine-readable cause — clients
	// must never parse human-readable error strings to classify a turn.
	EvtTurnOutcome = "turn.outcome"
)

// Lifecycle phases. Persistence only ever holds PhaseIdle durably: any
// in-flight phase found on load is settled back to idle (see load).
const (
	PhaseIdle         = "idle"
	PhaseInitializing = "initializing"
	PhaseRunning      = "running"
	PhaseFinalizing   = "finalizing"
)

// Terminal outcome kinds.
const (
	KindCompleted             = "completed"
	KindRuntimeTimeout        = "runtime_timeout"
	KindFacadeTimeout         = "facade_timeout"
	KindCanceled              = "canceled"
	KindProviderError         = "provider_error"
	KindFinalizeTimeout       = "finalize_timeout"
	KindInitializationFailure = "initialization_failure"
)

// Finalization triggers.
const (
	// TriggerSoftDeadline: the turn's work_timeout fired.
	TriggerSoftDeadline = "soft_deadline"
	// TriggerGracefulStop: an explicit graceful close/stop (e.g. A2A
	// tasks/cancel) arrived while a business turn was active.
	TriggerGracefulStop = "graceful_stop"
)

// Bounds. Lifecycle state is operational metadata, not a transcript — it must
// stay small and credential-safe regardless of provider output size.
const (
	maxLifecycleEventsPerSession = 500
	maxLifecycleDetailBytes      = 512
	maxLifecycleErrBytes         = 1024
	maxLifecycleMessageBytes     = 4096
)

// LifecycleEvent is one persisted, ordered lifecycle observation.
type LifecycleEvent struct {
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id,omitempty"`
	Phase     string `json:"phase"`
	Trigger   string `json:"trigger,omitempty"`
	// Kind is set on terminal (EvtTurnOutcome) events only.
	Kind   string    `json:"kind,omitempty"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// TurnOutcome is the typed terminal result of one business or lifecycle turn.
type TurnOutcome struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	// Phase is the lifecycle phase the turn ended in (idle after settling).
	Phase string `json:"phase"`
	// Kind is the machine-readable terminal cause — one of the Kind*
	// constants above.
	Kind      string `json:"kind"`
	Finalized bool   `json:"finalized"`
	// Trigger records why finalization started, when it did.
	Trigger       string     `json:"trigger,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    time.Time  `json:"finished_at"`
	WorkDeadline  *time.Time `json:"work_deadline,omitempty"`
	GraceDeadline *time.Time `json:"grace_deadline,omitempty"`
	// Error is bounded + sanitized metadata, never a credential-bearing
	// dump. Empty on success.
	Error string `json:"error,omitempty"`
	// LastMessage is the bounded tail of the last durable agent message
	// (the finalize summary when finalization succeeded).
	LastMessage string `json:"last_message,omitempty"`
}

// SessionLifecycle is the persisted lifecycle record of one Session
// (keyed by ahsir contextID).
type SessionLifecycle struct {
	Phase            string `json:"phase"`
	LifecycleVersion string `json:"lifecycle_version,omitempty"`
	// Initialized is the completion marker: set only after the
	// initialization turn completed durably. Recovery never sets it without
	// that evidence, so a crash mid-initialize re-runs initialization
	// (idempotent by contract) and never duplicates a COMPLETED init.
	Initialized   bool       `json:"initialized,omitempty"`
	InitializedAt *time.Time `json:"initialized_at,omitempty"`
	// InitDegraded records that initialization failed under the explicit
	// degraded policy — business turns proceed and init is not retried for
	// this lifecycle version.
	InitDegraded  bool   `json:"init_degraded,omitempty"`
	CurrentTurnID string `json:"current_turn_id,omitempty"`

	Events      []LifecycleEvent `json:"events,omitempty"`
	LastOutcome *TurnOutcome     `json:"last_outcome,omitempty"`
}

// LifecycleStore persists per-Session lifecycle state as one JSON file,
// written atomically (tmp + rename) on every mutation so a crash leaves at
// worst the previous committed state. Callers are serialized by mu.
type LifecycleStore struct {
	path string

	mu       sync.Mutex
	sessions map[string]*SessionLifecycle
	seq      int64
	now      func() time.Time
}

// NewLifecycleStore loads existing state (or starts empty). A corrupt file
// is treated as empty — an agent must always boot; initialization markers
// lost this way simply re-run an idempotent init.
func NewLifecycleStore(path string) *LifecycleStore {
	s := &LifecycleStore{path: path, sessions: map[string]*SessionLifecycle{}, now: func() time.Time { return time.Now().UTC() }}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var onDisk struct {
		Seq      int64                        `json:"seq"`
		Sessions map[string]*SessionLifecycle `json:"sessions"`
	}
	if err := json.Unmarshal(data, &onDisk); err != nil {
		log.Printf("lifecycle store: %s unreadable, starting empty: %v", path, err)
		return s
	}
	if onDisk.Sessions != nil {
		s.sessions = onDisk.Sessions
	}
	s.seq = onDisk.Seq
	// Recovery settling: a phase that was in flight when the process died
	// settles deterministically to idle. No outcome is fabricated; a turn
	// interrupted by the crash simply has no terminal outcome, and a
	// completed initialization marker is preserved (never duplicated).
	for _, rec := range s.sessions {
		if rec.Phase != PhaseIdle {
			rec.Phase = PhaseIdle
			rec.CurrentTurnID = ""
		}
		if len(rec.Events) > maxLifecycleEventsPerSession {
			rec.Events = rec.Events[len(rec.Events)-maxLifecycleEventsPerSession:]
		}
	}
	return s
}

func (s *LifecycleStore) getLocked(contextID string) *SessionLifecycle {
	rec := s.sessions[contextID]
	if rec == nil {
		rec = &SessionLifecycle{Phase: PhaseIdle}
		s.sessions[contextID] = rec
	}
	return rec
}

// NeedsInit reports whether the session must run initialization for the given
// lifecycle version before its first business turn.
func (s *LifecycleStore) NeedsInit(contextID, version string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.getLocked(contextID)
	if rec.InitDegraded && rec.LifecycleVersion == version {
		return false
	}
	return !rec.Initialized || rec.LifecycleVersion != version
}

// MarkInitialized records the durable initialization completion marker.
func (s *LifecycleStore) MarkInitialized(contextID, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.getLocked(contextID)
	rec.Initialized = true
	rec.InitDegraded = false
	rec.LifecycleVersion = version
	at := s.now()
	rec.InitializedAt = &at
	s.persistLocked()
}

// MarkInitDegraded records that initialization failed under the explicit
// degraded policy: business turns proceed and init is not retried for this
// lifecycle version.
func (s *LifecycleStore) MarkInitDegraded(contextID, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.getLocked(contextID)
	rec.InitDegraded = true
	rec.LifecycleVersion = version
	s.persistLocked()
}

// SetPhase updates the session's lifecycle phase (and the in-flight turn id).
func (s *LifecycleStore) SetPhase(contextID, phase, turnID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.getLocked(contextID)
	rec.Phase = phase
	rec.CurrentTurnID = turnID
	s.persistLocked()
}

// Append adds one event to the session's ordered, bounded log and persists.
// Seq and At are assigned here so ordering is store-authoritative.
func (s *LifecycleStore) Append(contextID string, ev LifecycleEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.getLocked(contextID)
	s.seq++
	ev.Seq = s.seq
	ev.SessionID = contextID
	ev.At = s.now()
	ev.Detail = boundLifecycleString(ev.Detail, maxLifecycleDetailBytes)
	rec.Events = append(rec.Events, ev)
	if len(rec.Events) > maxLifecycleEventsPerSession {
		rec.Events = append([]LifecycleEvent(nil), rec.Events[len(rec.Events)-maxLifecycleEventsPerSession:]...)
	}
	s.persistLocked()
}

// RecordOutcome stores the typed terminal outcome of a turn and settles the
// session to idle.
func (s *LifecycleStore) RecordOutcome(contextID string, o TurnOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.getLocked(contextID)
	o.SessionID = contextID
	o.Error = boundLifecycleString(o.Error, maxLifecycleErrBytes)
	o.LastMessage = boundLifecycleString(o.LastMessage, maxLifecycleMessageBytes)
	rec.LastOutcome = &o
	rec.Phase = PhaseIdle
	rec.CurrentTurnID = ""
	s.persistLocked()
}

// Record returns a copy of the session's lifecycle record.
func (s *LifecycleStore) Record(contextID string) SessionLifecycle {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.getLocked(contextID)
	out := *src
	out.Events = append([]LifecycleEvent(nil), src.Events...)
	if src.LastOutcome != nil {
		o := *src.LastOutcome
		out.LastOutcome = &o
	}
	return out
}

// boundLifecycleString trims s to at most max bytes (reserving room for the
// ellipsis marker) and strips leading/trailing whitespace so stored metadata
// stays small and single-purpose.
func boundLifecycleString(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max-len("…")] + "…"
	}
	return s
}

func (s *LifecycleStore) persistLocked() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(struct {
		Seq      int64                        `json:"seq"`
		Sessions map[string]*SessionLifecycle `json:"sessions"`
	}{Seq: s.seq, Sessions: s.sessions})
	if err != nil {
		log.Printf("lifecycle store: marshal failed: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("lifecycle store: write %s failed: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("lifecycle store: rename to %s failed: %v", s.path, err)
	}
}

// writeFileBytes is a test helper for planting corrupt fixtures.
func writeFileBytes(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
