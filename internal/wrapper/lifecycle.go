package wrapper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Agent Session lifecycle controller (issue #43).
//
// LifecycleController wraps the pool's session factory and drives the
// ahsir-owned Session lifecycle:
//
//	session created
//	  -> initializing (optional, once per Session + lifecycle version)
//	  -> running (business turns)
//	  -> finalizing (after the work_timeout soft deadline or an explicit
//	     graceful stop while a turn is active)
//	  -> idle/completed
//	  -> hard terminate only when finalization cannot finish within grace
//
// It is runtime- and transport-level infrastructure: it never inspects
// business payloads and knows nothing about DingTalk, dws, GitHub, or any
// source protocol. Lifecycle prompts are operator-supplied plain text run
// inside the Agent's existing sandbox — they cannot widen filesystem, shell,
// network, or credential capabilities beyond the card.
//
// Serialization contract: all lifecycle work for one Session is serialized by
// a per-Session mutex, acquired BEFORE the pool's turn gate, so initialize /
// business / finalize turns can never interleave with concurrent user turns.
// A user turn arriving while initialization or finalization is in flight
// QUEUES (it waits on the mutex, bounded by its own ctx) rather than
// interleaving or being rejected — that is the documented deterministic
// policy for input arriving during lifecycle work.
//
// Timeout precedence (deterministic): when finalize is enabled, the wrapper
// enforces work_timeout (soft) then grace_timeout (finalize bound). The
// provider-level runtime.timeout is raised to work+grace as a hard backstop
// by the agent bootstrap, so it can only fire if the wrapper's own
// enforcement fails. The CMA facade's turn timeout remains the outermost
// bound and records kind=facade_timeout when it wins. When lifecycle is
// disabled, the historical one-step provider timeout is the only deadline.
//
// Terminal-event and deadline truth (issue #47): a turn settles as completed
// ONLY on an explicit terminal event (EventTurnDone with Err == nil) observed
// while no wrapper-owned deadline or caller cancellation has fired. Channel
// EOF alone is never completion — a stream that closes without EventTurnDone
// is truncated (ErrStreamTruncated). When completion races a fired
// deadline/cancellation, the deadline wins deterministically: the wrapper
// cannot distinguish "provider finished just before the deadline" from
// "provider was killed and reported nil" (observed with Codex exiting 0
// without turn.completed), so it runs the bounded finalize turn to settle
// the turn with real output instead of trusting a nil provider error.

// Typed lifecycle errors surfaced as EventTurnDone.Err so callers can react
// without parsing message strings.
var (
	// ErrInitializationFailed terminates a turn whose Session initialization
	// failed (or timed out) under the default fail-closed policy.
	ErrInitializationFailed = errors.New("session initialization failed")
	// ErrFinalizeTimeout terminates a turn whose finalize turn could not
	// complete within grace_timeout (hard-terminate boundary).
	ErrFinalizeTimeout = errors.New("finalize turn exceeded grace timeout")
	// ErrFinalizationFailed terminates a turn whose finalize turn failed at
	// the provider level.
	ErrFinalizationFailed = errors.New("finalize turn failed")
	// ErrStreamTruncated terminates a turn whose provider stream closed
	// without delivering EventTurnDone. Channel EOF alone is not a terminal
	// signal: only an explicit terminal event settles a turn (issue #47).
	ErrStreamTruncated = errors.New("provider stream closed without a terminal event")
)

// LifecycleController owns per-Session lifecycle state for one agent.
type LifecycleController struct {
	cfg   ResolvedLifecycle
	store *LifecycleStore
	open  func(ctx context.Context, contextID string) (Session, error)

	mu       sync.Mutex
	sessions map[string]*lifecycleSessionState
}

type lifecycleSessionState struct {
	sem chan struct{} // cap 1 — serializes the whole lifecycle composite
}

// NewLifecycleController builds a controller for a resolved lifecycle config.
// open is typically SessionPool.LookupOrCreate, so reopening a Session after
// its provider process died transparently resumes the conversation.
func NewLifecycleController(cfg ResolvedLifecycle, store *LifecycleStore, open func(ctx context.Context, contextID string) (Session, error)) *LifecycleController {
	return &LifecycleController{cfg: cfg, store: store, open: open, sessions: map[string]*lifecycleSessionState{}}
}

// WrapOpenSession installs the lifecycle controller around open when the
// resolved lifecycle config enables any block; otherwise it returns open
// unchanged (byte-for-byte legacy behaviour for cards without a lifecycle
// block).
func WrapOpenSession(cfg ResolvedLifecycle, store *LifecycleStore, open func(ctx context.Context, contextID string) (Session, error)) func(ctx context.Context, contextID string) (Session, error) {
	if !cfg.Enabled() {
		return open
	}
	return NewLifecycleController(cfg, store, open).OpenSession
}

// OpenSession returns the lifecycle-managed Session for one contextID.
func (c *LifecycleController) OpenSession(_ context.Context, contextID string) (Session, error) {
	c.mu.Lock()
	st := c.sessions[contextID]
	if st == nil {
		st = &lifecycleSessionState{sem: make(chan struct{}, 1)}
		c.sessions[contextID] = st
	}
	c.mu.Unlock()
	return &lifecycleSession{ctrl: c, contextID: contextID, st: st}, nil
}

func newLifecycleTurnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "turn_" + hex.EncodeToString(b[:])
}

// lifecycleSession implements Session by composing initialize / business /
// finalize turns over the pool-managed provider sessions.
type lifecycleSession struct {
	ctrl      *LifecycleController
	contextID string
	st        *lifecycleSessionState

	mu       sync.Mutex
	lastOpen Session // last provider session opened, for SessionID/IsHealthy/Close delegation
}

func (s *lifecycleSession) Stream(ctx context.Context, userText string) (<-chan Event, error) {
	// Queue policy: wait for the per-Session lifecycle mutex, bounded by the
	// caller's ctx. This is what serializes user turns behind an in-flight
	// initialize/finalize instead of interleaving with them.
	select {
	case s.st.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make(chan Event, 16)
	go func() {
		defer func() { <-s.st.sem }()
		defer close(out)
		s.run(ctx, userText, out)
	}()
	return out, nil
}

func (s *lifecycleSession) Turn(ctx context.Context, userText string) (string, error) {
	ch, err := s.Stream(ctx, userText)
	if err != nil {
		return "", err
	}
	_, text, doneErr := drainEvents(ch)
	return text, doneErr
}

func (s *lifecycleSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastOpen == nil {
		return ""
	}
	return s.lastOpen.SessionID()
}

func (s *lifecycleSession) IsHealthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastOpen == nil {
		return true // nothing opened yet — OpenSession will create fresh
	}
	return s.lastOpen.IsHealthy()
}

func (s *lifecycleSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastOpen == nil {
		return nil
	}
	return s.lastOpen.Close()
}

// openInner opens (or resumes) the provider session for this contextID and
// remembers it for delegation.
func (s *lifecycleSession) openInner(ctx context.Context) (Session, error) {
	inner, err := s.ctrl.open(ctx, s.contextID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.lastOpen = inner
	s.mu.Unlock()
	return inner, nil
}

// emit records a lifecycle event durably and surfaces it on the turn stream.
func (s *lifecycleSession) emit(out chan<- Event, ev EventLifecycle) {
	s.ctrl.store.Append(s.contextID, LifecycleEvent{
		Type:    ev.Event,
		TurnID:  ev.TurnID,
		Phase:   ev.Phase,
		Trigger: ev.Trigger,
		Kind:    ev.Kind,
		Detail:  ev.Detail,
	})
	out <- ev
}

// run drives the composite: initialize (once) -> business turn -> optional
// finalize. Runs on the Stream goroutine with the per-Session mutex held.
func (s *lifecycleSession) run(ctx context.Context, userText string, out chan<- Event) {
	cfg := s.ctrl.cfg
	if cfg.Initialize.Enabled && s.ctrl.store.NeedsInit(s.contextID, cfg.Version) {
		if !s.initialize(ctx, out) {
			return // fail closed: typed outcome recorded, business turn not started
		}
	}
	s.runBusiness(ctx, userText, out)
}

// initialize runs the one-time initialization turn. Returns false when the
// business turn must not start (fail-closed policy).
func (s *lifecycleSession) initialize(ctx context.Context, out chan<- Event) bool {
	cfg := s.ctrl.cfg
	turnID := newLifecycleTurnID()
	started := time.Now().UTC()
	deadline := started.Add(cfg.Initialize.Timeout)

	s.ctrl.store.SetPhase(s.contextID, PhaseInitializing, turnID)
	s.emit(out, EventLifecycle{Event: EvtSessionInitializing, TurnID: turnID, Phase: PhaseInitializing})

	fail := func(cause error) bool {
		s.ctrl.store.SetPhase(s.contextID, PhaseIdle, "")
		s.emit(out, EventLifecycle{Event: EvtSessionInitializationFailed, TurnID: turnID, Phase: PhaseIdle, Detail: cause.Error()})
		outcome := TurnOutcome{
			TurnID: turnID, Phase: PhaseIdle, Kind: KindInitializationFailure,
			StartedAt: started, FinishedAt: time.Now().UTC(), Error: cause.Error(),
		}
		s.ctrl.store.RecordOutcome(s.contextID, outcome)
		s.emit(out, EventLifecycle{Event: EvtTurnOutcome, TurnID: turnID, Phase: PhaseIdle, Kind: KindInitializationFailure, Detail: cause.Error()})
		if cfg.Initialize.OnFailure == InitDegraded {
			s.ctrl.store.MarkInitDegraded(s.contextID, cfg.Version)
			return true // explicit degraded mode: proceed without initialization
		}
		out <- EventTurnDone{Err: fmt.Errorf("%w: %v", ErrInitializationFailed, cause)}
		return false
	}

	inner, err := s.openInner(ctx)
	if err != nil {
		return fail(fmt.Errorf("open session: %w", err))
	}
	initCtx, cancel := context.WithTimeout(ctx, cfg.Initialize.Timeout)
	defer cancel()
	ch, err := inner.Stream(initCtx, cfg.Initialize.Prompt)
	if err != nil {
		return fail(fmt.Errorf("start initialization turn: %w", err))
	}
	var turnErr error
	var gotTerminal bool
	for ev := range ch {
		// Initialization output is system-owned lifecycle work: it is
		// drained for completion but NOT forwarded as business reply text.
		if done, ok := ev.(EventTurnDone); ok {
			turnErr = done.Err
			gotTerminal = true
		}
	}
	switch {
	case initCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
		return fail(fmt.Errorf("initialization exceeded timeout %v (deadline %s)", cfg.Initialize.Timeout, deadline.Format(time.RFC3339)))
	case ctx.Err() != nil:
		return fail(fmt.Errorf("initialization interrupted: %w", ErrTurnCanceled))
	case turnErr == nil && gotTerminal:
		s.ctrl.store.MarkInitialized(s.contextID, cfg.Version)
		s.ctrl.store.SetPhase(s.contextID, PhaseIdle, "")
		s.emit(out, EventLifecycle{Event: EvtSessionInitialized, TurnID: turnID, Phase: PhaseIdle})
		log.Printf("lifecycle: session %s initialized (turn=%s, took=%v)", s.contextID, turnID, time.Since(started))
		return true
	case turnErr == nil:
		return fail(fmt.Errorf("initialization turn: %w", ErrStreamTruncated))
	default:
		return fail(fmt.Errorf("initialization turn: %w", turnErr))
	}
}

// runBusiness drives the business turn and, when enabled, the two-stage
// deadline: work_timeout soft deadline -> grace-bounded finalize turn.
func (s *lifecycleSession) runBusiness(ctx context.Context, userText string, out chan<- Event) {
	cfg := s.ctrl.cfg
	turnID := newLifecycleTurnID()
	started := time.Now().UTC()

	// The soft (work) deadline exists only when finalization is enabled;
	// otherwise the provider's own runtime.timeout remains the sole per-turn
	// deadline, preserving legacy behaviour.
	workCtx := ctx
	var workCancel context.CancelFunc
	var workDeadline *time.Time
	if cfg.Finalize.Enabled {
		d := started.Add(cfg.Finalize.WorkTimeout)
		workDeadline = &d
		workCtx, workCancel = context.WithTimeout(ctx, cfg.Finalize.WorkTimeout)
		defer workCancel()
	}

	s.ctrl.store.SetPhase(s.contextID, PhaseRunning, turnID)
	s.emit(out, EventLifecycle{Event: EvtTurnRunning, TurnID: turnID, Phase: PhaseRunning})

	inner, err := s.openInner(workCtx)
	if err != nil {
		s.finishTurn(out, TurnOutcome{
			TurnID: turnID, Phase: PhaseIdle, Kind: KindProviderError,
			StartedAt: started, FinishedAt: time.Now().UTC(), WorkDeadline: workDeadline,
			Error: "open session: " + err.Error(),
		}, nil, fmt.Errorf("open session: %w", err))
		return
	}
	ch, err := inner.Stream(workCtx, userText)
	if err != nil {
		s.finishTurn(out, TurnOutcome{
			TurnID: turnID, Phase: PhaseIdle, Kind: KindProviderError,
			StartedAt: started, FinishedAt: time.Now().UTC(), WorkDeadline: workDeadline,
			Error: "start turn: " + err.Error(),
		}, nil, fmt.Errorf("start turn: %w", err))
		return
	}

	// Drain the business turn: every event (partial text, tool evidence) is
	// forwarded as-is, so partial work emitted before any cancellation stays
	// readable. EventTurnDone is captured, not yet forwarded — finalization
	// may still settle the turn cleanly.
	var lastText string
	var done EventTurnDone
	var gotTerminal bool
	for ev := range ch {
		if d, ok := ev.(EventTurnDone); ok {
			done = d
			gotTerminal = true
			continue
		}
		if t, ok := ev.(EventText); ok {
			lastText = t.Text
		}
		out <- ev
	}

	// Decision order is deterministic (issue #47): wrapper-owned deadline and
	// cancellation truth is evaluated BEFORE the provider's reported error.
	// A nil provider error observed after the work deadline fired is not
	// trusted — Codex can exit 0 without a terminal protocol event when
	// killed — and the turn settles through the bounded finalize turn
	// instead of recording an empty "completed".
	softDeadline := cfg.Finalize.Enabled && workCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil
	callerGone := cfg.Finalize.Enabled && ctx.Err() != nil
	switch {
	case softDeadline:
		// Soft deadline fired while the caller is still attached.
		s.emit(out, EventLifecycle{Event: EvtTurnSoftDeadline, TurnID: turnID, Phase: PhaseRunning})
		s.finalize(out, turnID, TriggerSoftDeadline, KindRuntimeTimeout, started, workDeadline, lastText)
		return
	case callerGone:
		// Explicit graceful stop (tasks/cancel, client shutdown) while the
		// turn was active. Finalization runs on a detached context.
		s.finalize(out, turnID, TriggerGracefulStop, KindCanceled, started, workDeadline, lastText)
		return
	case done.Err == nil && gotTerminal:
		s.finishTurn(out, TurnOutcome{
			TurnID: turnID, Phase: PhaseIdle, Kind: KindCompleted,
			StartedAt: started, FinishedAt: time.Now().UTC(), WorkDeadline: workDeadline,
			LastMessage: lastText,
		}, &done, nil)
		return
	case done.Err == nil:
		// EOF without EventTurnDone: the stream was truncated mid-turn.
		// Partial evidence stays visible, but this is never a completion.
		err := fmt.Errorf("business turn: %w", ErrStreamTruncated)
		s.finishTurn(out, TurnOutcome{
			TurnID: turnID, Phase: PhaseIdle, Kind: KindProviderError,
			StartedAt: started, FinishedAt: time.Now().UTC(), WorkDeadline: workDeadline,
			Error: err.Error(), LastMessage: lastText,
		}, nil, err)
		return
	default:
		// Provider failure (or legacy one-step timeout with finalize
		// disabled): finalization may be impossible — record a distinct
		// terminal result and never fabricate final output.
		kind := KindProviderError
		if errors.Is(done.Err, ErrTurnCanceled) {
			kind = KindCanceled
		}
		s.finishTurn(out, TurnOutcome{
			TurnID: turnID, Phase: PhaseIdle, Kind: kind,
			StartedAt: started, FinishedAt: time.Now().UTC(), WorkDeadline: workDeadline,
			Error: done.Err.Error(), LastMessage: lastText,
		}, &done, done.Err)
		return
	}
}

// finalize runs the grace-bounded finalize turn in the same logical Session.
// The business turn has already drained (cancelled); its resume identity is
// preserved by the pool, so reopening here transparently resumes.
func (s *lifecycleSession) finalize(out chan<- Event, turnID, trigger, triggerKind string, started time.Time, workDeadline *time.Time, partialText string) {
	cfg := s.ctrl.cfg
	s.ctrl.store.SetPhase(s.contextID, PhaseFinalizing, turnID)
	s.emit(out, EventLifecycle{Event: EvtTurnFinalizing, TurnID: turnID, Phase: PhaseFinalizing, Trigger: trigger})

	graceStarted := time.Now().UTC()
	graceDeadline := graceStarted.Add(cfg.Finalize.GraceTimeout)
	// Detached from the caller ctx: finalization must complete even when the
	// client cancelled/disconnected — that is the entire point of the grace
	// turn. The grace deadline remains authoritative.
	graceCtx, cancel := context.WithTimeout(context.Background(), cfg.Finalize.GraceTimeout)
	defer cancel()

	inner, err := s.openInner(graceCtx)
	if err != nil {
		s.finishFinalize(out, turnID, trigger, started, workDeadline, &graceDeadline, partialText,
			KindProviderError, fmt.Errorf("finalize: reopen session: %w", err))
		return
	}
	ch, err := inner.Stream(graceCtx, cfg.Finalize.Prompt)
	if err != nil {
		s.finishFinalize(out, turnID, trigger, started, workDeadline, &graceDeadline, partialText,
			KindProviderError, fmt.Errorf("finalize: start turn: %w", err))
		return
	}

	var summary string
	var done EventTurnDone
	var gotTerminal bool
	for ev := range ch {
		if d, ok := ev.(EventTurnDone); ok {
			done = d
			gotTerminal = true
			continue
		}
		// The finalize summary IS the turn's final delivery — forward it.
		if t, ok := ev.(EventText); ok {
			summary = t.Text
		}
		out <- ev
	}

	switch {
	case graceCtx.Err() == context.DeadlineExceeded:
		// Hard-terminate boundary: the finalize turn itself exceeded grace.
		// The grace deadline is evaluated before the provider's error for the
		// same reason as the work deadline: a nil provider error after the
		// grace kill is not a finalization (issue #47).
		s.finishFinalize(out, turnID, trigger, started, workDeadline, &graceDeadline, partialText,
			KindFinalizeTimeout, fmt.Errorf("%w (%v)", ErrFinalizeTimeout, cfg.Finalize.GraceTimeout))
	case done.Err == nil && gotTerminal:
		s.emit(out, EventLifecycle{Event: EvtTurnFinalized, TurnID: turnID, Phase: PhaseIdle, Trigger: trigger})
		s.finishTurn(out, TurnOutcome{
			TurnID: turnID, Phase: PhaseIdle, Kind: triggerKind, Finalized: true, Trigger: trigger,
			StartedAt: started, FinishedAt: time.Now().UTC(),
			WorkDeadline: workDeadline, GraceDeadline: &graceDeadline,
			LastMessage: summary,
		}, &EventTurnDone{Stats: done.Stats}, nil)
	case done.Err == nil:
		// Finalize stream truncated (EOF without EventTurnDone).
		s.finishFinalize(out, turnID, trigger, started, workDeadline, &graceDeadline, partialText,
			KindProviderError, fmt.Errorf("%w: %w", ErrFinalizationFailed, ErrStreamTruncated))
	default:
		s.finishFinalize(out, turnID, trigger, started, workDeadline, &graceDeadline, partialText,
			KindProviderError, fmt.Errorf("%w: %v", ErrFinalizationFailed, done.Err))
	}
}

// finishFinalize records a failed finalization: finalization_failed event,
// typed terminal outcome, and a terminal stream error.
func (s *lifecycleSession) finishFinalize(out chan<- Event, turnID, trigger string, started time.Time, workDeadline, graceDeadline *time.Time, partialText, kind string, err error) {
	s.emit(out, EventLifecycle{Event: EvtTurnFinalizationFailed, TurnID: turnID, Phase: PhaseIdle, Trigger: trigger, Kind: kind, Detail: err.Error()})
	s.finishTurn(out, TurnOutcome{
		TurnID: turnID, Phase: PhaseIdle, Kind: kind, Trigger: trigger,
		StartedAt: started, FinishedAt: time.Now().UTC(),
		WorkDeadline: workDeadline, GraceDeadline: graceDeadline,
		Error: err.Error(), LastMessage: partialText,
	}, nil, err)
}

// finishTurn persists the terminal outcome, emits the terminal lifecycle
// event, and closes the stream with the given terminal EventTurnDone / error.
func (s *lifecycleSession) finishTurn(out chan<- Event, outcome TurnOutcome, done *EventTurnDone, turnErr error) {
	s.ctrl.store.RecordOutcome(s.contextID, outcome)
	s.emit(out, EventLifecycle{Event: EvtTurnOutcome, TurnID: outcome.TurnID, Phase: outcome.Phase, Kind: outcome.Kind, Trigger: outcome.Trigger, Detail: outcome.Error})
	if done != nil {
		out <- *done
		return
	}
	out <- EventTurnDone{Err: turnErr}
}

// drainEvents aggregates a stream's text; used by Turn.
func drainEvents(ch <-chan Event) (events []Event, text string, doneErr error) {
	var sb []byte
	for ev := range ch {
		events = append(events, ev)
		switch e := ev.(type) {
		case EventText:
			sb = append(sb, e.Text...)
		case EventTurnDone:
			doneErr = e.Err
		}
	}
	return events, string(sb), doneErr
}
