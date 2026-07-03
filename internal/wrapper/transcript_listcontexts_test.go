package wrapper

import (
	"testing"
	"time"
)

func TestTranscriptExpired(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		lastTurn time.Time
		want     bool
	}{
		{"fresh", now.Add(-time.Hour), false},
		{"just inside window", now.Add(-transcriptRetention + time.Minute), false},
		{"exactly at cutoff", now.Add(-transcriptRetention), false}, // Before is strict
		{"just past window", now.Add(-transcriptRetention - time.Minute), true},
		{"ancient", now.Add(-365 * 24 * time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TranscriptExpired(tt.lastTurn, now); got != tt.want {
				t.Fatalf("TranscriptExpired(%v) = %v, want %v", tt.lastTurn, got, tt.want)
			}
		})
	}
}

// seedTurn appends a single completed turn stamped at ts so retention/ordering
// can be asserted deterministically.
func seedTurn(t *testing.T, store *TranscriptStore, contextID, user, reply, status string, ts time.Time) {
	t.Helper()
	if err := store.Append(contextID, TranscriptTurn{
		TS:       ts,
		UserText: user,
		Reply:    reply,
		Status:   status,
	}); err != nil {
		t.Fatalf("append %s: %v", contextID, err)
	}
}

func TestListContexts(t *testing.T) {
	store, _ := newTestTranscriptStore(t)
	now := time.Now()

	// ctx-old: newest turn well past retention → must be omitted.
	seedTurn(t, store, "ctx-old", "old question", "old answer", "completed", now.Add(-40*24*time.Hour))
	// ctx-recent: two turns, the first carries the title, newest is recent.
	seedTurn(t, store, "ctx-recent", "recent question", "a1", "completed", now.Add(-2*time.Hour))
	seedTurn(t, store, "ctx-recent", "follow up", "a2", "failed", now.Add(-1*time.Hour))
	// ctx-mid: single recent turn, older than ctx-recent's last activity.
	seedTurn(t, store, "ctx-mid", "mid question", "am", "completed", now.Add(-5*time.Hour))

	got, err := store.ListContexts(now)
	if err != nil {
		t.Fatalf("ListContexts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 in-retention contexts, got %d: %+v", len(got), got)
	}
	// Newest last-activity first: ctx-recent (-1h) then ctx-mid (-5h).
	if got[0].ContextID != "ctx-recent" || got[1].ContextID != "ctx-mid" {
		t.Fatalf("wrong order: %s, %s", got[0].ContextID, got[1].ContextID)
	}
	rec := got[0]
	if rec.Turns != 2 {
		t.Errorf("ctx-recent turns = %d, want 2", rec.Turns)
	}
	if rec.Title != "recent question" {
		t.Errorf("ctx-recent title = %q, want first userText", rec.Title)
	}
	if rec.LastStatus != "failed" {
		t.Errorf("ctx-recent lastStatus = %q, want failed (last turn)", rec.LastStatus)
	}
	if rec.LastActivity.IsZero() {
		t.Error("ctx-recent lastActivity should be set")
	}
}

func TestListContextsEmpty(t *testing.T) {
	store, _ := newTestTranscriptStore(t)
	got, err := store.ListContexts(time.Now())
	if err != nil {
		t.Fatalf("ListContexts on empty store: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty store should yield no contexts, got %d", len(got))
	}
}

func TestFirstUserText(t *testing.T) {
	// Skips leading empty userText (e.g. a system-seeded turn) to the first real
	// message.
	turns := []TranscriptTurn{
		{UserText: "", Reply: "seed"},
		{UserText: "the real first question", Reply: "answer"},
	}
	if got := firstUserText(turns); got != "the real first question" {
		t.Fatalf("firstUserText = %q", got)
	}
	if got := firstUserText(nil); got != "" {
		t.Fatalf("firstUserText(nil) = %q, want empty", got)
	}
}
