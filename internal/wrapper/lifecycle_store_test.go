package wrapper

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLifecycleStorePersistsInitMarker(t *testing.T) {
	path := t.TempDir() + "/lifecycle.json"
	s := NewLifecycleStore(path)
	if !s.NeedsInit("ctx1", "v1") {
		t.Fatal("fresh session must need initialization")
	}
	s.MarkInitialized("ctx1", "v1")
	if s.NeedsInit("ctx1", "v1") {
		t.Fatal("marked session must not need initialization")
	}

	// Reload (process restart / scale-to-zero + wake): the marker survives.
	s2 := NewLifecycleStore(path)
	if s2.NeedsInit("ctx1", "v1") {
		t.Fatal("init marker must survive ahsir restart")
	}
	// A lifecycle version change re-runs initialization exactly once.
	if !s2.NeedsInit("ctx1", "v2") {
		t.Fatal("lifecycle version change must require re-initialization")
	}
}

func TestLifecycleStoreDegradedMarkerSuppressesInit(t *testing.T) {
	path := t.TempDir() + "/lifecycle.json"
	s := NewLifecycleStore(path)
	s.MarkInitDegraded("ctx1", "v1")
	if s.NeedsInit("ctx1", "v1") {
		t.Fatal("degraded marker must suppress further init attempts for the same version")
	}
	if !NewLifecycleStore(path).NeedsInit("ctx1", "v2") {
		t.Fatal("degraded marker must not leak across lifecycle versions")
	}
}

func TestLifecycleStoreRecoverySettlesInFlightPhases(t *testing.T) {
	path := t.TempDir() + "/lifecycle.json"
	s := NewLifecycleStore(path)
	// Simulate a crash mid-finalize: phase persisted as finalizing.
	s.SetPhase("ctx1", PhaseRunning, "turn_1")
	s.Append("ctx1", LifecycleEvent{Type: EvtTurnSoftDeadline, TurnID: "turn_1"})
	s.SetPhase("ctx1", PhaseFinalizing, "turn_1")

	s2 := NewLifecycleStore(path)
	rec := s2.Record("ctx1")
	if rec.Phase != PhaseIdle {
		t.Fatalf("restart during finalize must settle to idle, got %q", rec.Phase)
	}
	if rec.CurrentTurnID != "" {
		t.Fatalf("settled record must not reference an in-flight turn: %q", rec.CurrentTurnID)
	}
	if rec.LastOutcome != nil {
		t.Fatal("recovery must never fabricate a completed outcome")
	}
	// Historical events remain readable after the crash.
	found := false
	for _, ev := range rec.Events {
		if ev.Type == EvtTurnSoftDeadline {
			found = true
		}
	}
	if !found {
		t.Fatal("events emitted before the crash must remain readable")
	}
}

func TestLifecycleStoreRecoveryKeepsCompletedInit(t *testing.T) {
	path := t.TempDir() + "/lifecycle.json"
	s := NewLifecycleStore(path)
	s.MarkInitialized("ctx1", "v1")
	// Crash while a later business turn was running.
	s.SetPhase("ctx1", PhaseRunning, "turn_9")

	s2 := NewLifecycleStore(path)
	if s2.NeedsInit("ctx1", "v1") {
		t.Fatal("a completed initialization must not be duplicated after restart")
	}
	if rec := s2.Record("ctx1"); rec.Phase != PhaseIdle {
		t.Fatalf("in-flight turn phase must settle to idle, got %q", rec.Phase)
	}
}

func TestLifecycleStoreEventsAreOrderedAndBounded(t *testing.T) {
	path := t.TempDir() + "/lifecycle.json"
	s := NewLifecycleStore(path)
	for i := 0; i < maxLifecycleEventsPerSession+50; i++ {
		s.Append("ctx1", LifecycleEvent{Type: EvtTurnRunning, TurnID: fmt.Sprintf("turn_%d", i)})
	}
	rec := s.Record("ctx1")
	if len(rec.Events) != maxLifecycleEventsPerSession {
		t.Fatalf("event log must be bounded at %d, got %d", maxLifecycleEventsPerSession, len(rec.Events))
	}
	for i := 1; i < len(rec.Events); i++ {
		if rec.Events[i].Seq <= rec.Events[i-1].Seq {
			t.Fatal("event seq must be strictly increasing")
		}
	}
	// Oldest events were dropped, newest retained.
	if rec.Events[len(rec.Events)-1].TurnID != fmt.Sprintf("turn_%d", maxLifecycleEventsPerSession+49) {
		t.Fatalf("bounded log must keep the newest events, last=%q", rec.Events[len(rec.Events)-1].TurnID)
	}
}

func TestLifecycleStoreOutcomeRoundTrip(t *testing.T) {
	path := t.TempDir() + "/lifecycle.json"
	s := NewLifecycleStore(path)
	start := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s.RecordOutcome("ctx1", TurnOutcome{
		SessionID: "ctx1", TurnID: "turn_1", Phase: PhaseIdle,
		Kind: KindRuntimeTimeout, Finalized: true,
		StartedAt: start, FinishedAt: start.Add(time.Minute),
		Error:       "soft deadline",
		LastMessage: "partial answer",
	})
	s2 := NewLifecycleStore(path)
	out := s2.Record("ctx1").LastOutcome
	if out == nil {
		t.Fatal("terminal outcome must survive restart")
	}
	if out.Kind != KindRuntimeTimeout || !out.Finalized || out.TurnID != "turn_1" || out.LastMessage != "partial answer" {
		t.Fatalf("outcome mismatch: %+v", out)
	}
}

func TestLifecycleStoreBoundsAndSanitizesFields(t *testing.T) {
	s := NewLifecycleStore(t.TempDir() + "/lifecycle.json")
	big := strings.Repeat("x", 10*1024)
	s.RecordOutcome("ctx1", TurnOutcome{
		SessionID: "ctx1", TurnID: "t", Phase: PhaseIdle, Kind: KindProviderError,
		Error: big, LastMessage: big,
	})
	out := s.Record("ctx1").LastOutcome
	if len(out.Error) > maxLifecycleErrBytes || len(out.LastMessage) > maxLifecycleMessageBytes {
		t.Fatalf("outcome fields must be bounded: err=%d msg=%d", len(out.Error), len(out.LastMessage))
	}
	s.Append("ctx1", LifecycleEvent{Type: EvtTurnRunning, Detail: big})
	rec := s.Record("ctx1")
	if len(rec.Events[0].Detail) > maxLifecycleDetailBytes {
		t.Fatalf("event detail must be bounded: %d", len(rec.Events[0].Detail))
	}
}

func TestLifecycleStoreCorruptFileStartsEmpty(t *testing.T) {
	path := t.TempDir() + "/lifecycle.json"
	if err := writeFileBytes(path, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	s := NewLifecycleStore(path)
	if !s.NeedsInit("ctx1", "v1") {
		t.Fatal("corrupt lifecycle file must fall back to empty state")
	}
}
