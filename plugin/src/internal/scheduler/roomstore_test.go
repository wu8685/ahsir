package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Persist a room's meta + turns + a status change, then reload it and verify the
// full state round-trips, with recovered scheduling reset to "waiting".
func TestRoomStorePersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewRoomStore(dir)

	created := time.Date(2026, 6, 9, 10, 0, 0, 0, time.Local)
	room := &Room{
		ID:           "room-1",
		Topic:        "评审 OKR",
		Participants: []string{"strategist", "architect"},
		Organizer:    "operator",
		MaxChain:     8,
		createdAt:    created,
	}
	if err := store.WriteMeta(room); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurn(room.ID, RoomTurn{Speaker: "operator", Text: "开始", TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendTurn(room.ID, RoomTurn{Speaker: "strategist", Text: "草稿如下", Mentions: []string{"architect"}, TS: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteStatus(room.ID, RoomStopped); err != nil {
		t.Fatal(err)
	}

	rooms, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := rooms["room-1"]
	if !ok {
		t.Fatalf("room not restored: %v", rooms)
	}
	if got.Topic != "评审 OKR" || got.Organizer != "operator" || got.MaxChain != 8 {
		t.Errorf("meta mismatch: %+v", got)
	}
	if len(got.Participants) != 2 || got.Participants[0] != "strategist" {
		t.Errorf("participants mismatch: %v", got.Participants)
	}
	if len(got.transcript) != 2 {
		t.Fatalf("transcript len = %d, want 2", len(got.transcript))
	}
	if got.transcript[1].Speaker != "strategist" || got.transcript[1].Text != "草稿如下" {
		t.Errorf("turn round-trip mismatch: %+v", got.transcript[1])
	}
	if got.status != RoomStopped {
		t.Errorf("status = %q, want stopped", got.status)
	}
	if !got.createdAt.Equal(created) {
		t.Errorf("createdAt round-trip: got %v, want %v", got.createdAt, created)
	}
	// lastSeen is rebuilt to each participant's last *successful* turn (so a
	// retried speaker is re-fed what it owes a reply to): strategist spoke at
	// turn 2 → 2; architect never spoke → 0.
	if got.lastSeen["strategist"] != 2 {
		t.Errorf("strategist lastSeen = %d, want 2 (its last successful turn)", got.lastSeen["strategist"])
	}
	if got.lastSeen["architect"] != 0 {
		t.Errorf("architect lastSeen = %d, want 0 (never spoke)", got.lastSeen["architect"])
	}
}

// Compensation: a room whose last turn is a send-failure (the @-mentioned agent
// never answered) comes back active with that agent re-scheduled, so the
// interrupted turn is retried after restart.
func TestRoomRecoveryRetriesFailedTurn(t *testing.T) {
	dir := t.TempDir()
	store := NewRoomStore(dir)
	room := &Room{
		ID: "room-x", Topic: "t", Participants: []string{"a", "b"},
		Organizer: "operator", MaxChain: 8, createdAt: time.Now(),
	}
	store.WriteMeta(room)
	// operator @a, a replies @b, then the send to b fails (b unreachable).
	store.AppendTurn(room.ID, RoomTurn{Speaker: "operator", Text: "@a 开始", Mentions: []string{"a"}, TS: time.Now()})
	store.AppendTurn(room.ID, RoomTurn{Speaker: "a", Text: "我看 @b 怎么说", Mentions: []string{"b"}, TS: time.Now()})
	store.AppendTurn(room.ID, RoomTurn{Speaker: "b", Error: "Post ...: EOF", TS: time.Now()})

	rooms, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := rooms["room-x"]
	if got == nil {
		t.Fatal("room not restored")
	}
	if got.status != RoomActive {
		t.Errorf("status = %q, want active (retry pending)", got.status)
	}
	if got.next != "b" {
		t.Errorf("next = %q, want b (the agent whose turn failed)", got.next)
	}
	// b never succeeded → lastSeen 0, so the retry re-feeds it everything it owes.
	if got.lastSeen["b"] != 0 {
		t.Errorf("b lastSeen = %d, want 0 so the retry re-feeds unanswered messages", got.lastSeen["b"])
	}
	if got.lastSeen["a"] != 2 {
		t.Errorf("a lastSeen = %d, want 2 (its successful turn)", got.lastSeen["a"])
	}
}

// Compensation also covers a restart so abrupt no error turn was written: the
// last good turn still @-mentions an agent that never answered.
func TestRoomRecoveryRetriesUnansweredMention(t *testing.T) {
	dir := t.TempDir()
	store := NewRoomStore(dir)
	store.WriteMeta(&Room{ID: "room-y", Topic: "t", Participants: []string{"a", "b"}, Organizer: "operator", MaxChain: 8, createdAt: time.Now()})
	store.AppendTurn("room-y", RoomTurn{Speaker: "operator", Text: "@a", Mentions: []string{"a"}, TS: time.Now()})
	store.AppendTurn("room-y", RoomTurn{Speaker: "a", Text: "交给 @b", Mentions: []string{"b"}, TS: time.Now()})
	// b's turn never made it to disk (killed mid-flight).

	rooms, _ := store.Load()
	got := rooms["room-y"]
	if got.status != RoomActive || got.next != "b" {
		t.Errorf("want active/next=b (retry the unanswered mention), got status=%q next=%q", got.status, got.next)
	}
}

// A room created + driven on one manager comes back intact on a fresh manager
// loading the same store, parked in waiting and re-activatable.
func TestRoomPersistsAcrossManagerRestart(t *testing.T) {
	dir := t.TempDir()

	fake := &fakeTurns{reply: scriptByAgent(map[string][]string{"a": {"我的草稿"}})}
	m1 := NewRoomManager(fake.run, allAgents)
	m1.SetStore(NewRoomStore(dir), nil)
	view, err := m1.CreateRoom("话题", []string{"a", "b"}, "operator", 8, "@a 出个草稿")
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m1, view.ID, RoomWaiting)
	before, _ := m1.Get(view.ID)

	// Fresh manager restores from disk.
	restored, err := NewRoomStore(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	m2 := NewRoomManager(fake.run, allAgents)
	m2.SetStore(NewRoomStore(dir), restored)

	got, err := m2.Get(view.ID)
	if err != nil {
		t.Fatalf("restored room not found: %v", err)
	}
	if got.Status != RoomWaiting {
		t.Errorf("restored status = %q, want waiting", got.Status)
	}
	if len(got.Transcript) != len(before.Transcript) || len(got.Transcript) != 2 {
		t.Fatalf("restored transcript len = %d, want %d", len(got.Transcript), len(before.Transcript))
	}
	if got.Topic != "话题" || got.Participants[0] != "a" {
		t.Errorf("restored meta mismatch: %+v", got)
	}
	// Re-activatable: an operator message drives a new turn on the restored room.
	if _, err := m2.Say(view.ID, "@a 再来", "operator"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, m2, view.ID, RoomWaiting)
	if final, _ := m2.Get(view.ID); len(final.Transcript) != 4 {
		t.Errorf("after re-activation transcript len = %d, want 4", len(final.Transcript))
	}
}

func TestRoomStoreRetention(t *testing.T) {
	dir := t.TempDir()
	store := NewRoomStore(dir)
	if err := store.WriteMeta(&Room{ID: "fresh", createdAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteMeta(&Room{ID: "stale", createdAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "stale.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}

	n, err := store.CompactForRetention(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("removed = %d, want 1", n)
	}
	rooms, _ := store.Load()
	if _, ok := rooms["stale"]; ok {
		t.Error("stale room should be pruned")
	}
	if _, ok := rooms["fresh"]; !ok {
		t.Error("fresh room must remain")
	}
}

func TestRoomStoreNilSafe(t *testing.T) {
	var s *RoomStore // persistence disabled
	if err := s.WriteMeta(&Room{ID: "x"}); err != nil {
		t.Errorf("WriteMeta nil: %v", err)
	}
	if err := s.AppendTurn("x", RoomTurn{}); err != nil {
		t.Errorf("AppendTurn nil: %v", err)
	}
	if err := s.WriteStatus("x", RoomStopped); err != nil {
		t.Errorf("WriteStatus nil: %v", err)
	}
	if rooms, err := s.Load(); err != nil || len(rooms) != 0 {
		t.Errorf("Load nil: rooms=%v err=%v", rooms, err)
	}
	if n, err := s.CompactForRetention(time.Now()); err != nil || n != 0 {
		t.Errorf("Compact nil: n=%d err=%v", n, err)
	}
	if NewRoomStore("") != nil {
		t.Error("empty dir should yield a nil (disabled) store")
	}
}

// A torn final line (crash mid-append) must not discard the turns before it.
func TestRoomStoreTornFinalLine(t *testing.T) {
	dir := t.TempDir()
	store := NewRoomStore(dir)
	store.WriteMeta(&Room{ID: "torn", Topic: "t", Participants: []string{"a"}, createdAt: time.Now()})
	store.AppendTurn("torn", RoomTurn{Speaker: "a", Text: "good", TS: time.Now()})
	// Append a half-written record.
	f, _ := os.OpenFile(filepath.Join(dir, "torn.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"kind":"turn","turn":{"speaker":"a","tex`)
	f.Close()

	rooms, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := rooms["torn"]
	if !ok {
		t.Fatal("room with torn tail should still load")
	}
	if len(got.transcript) != 1 || got.transcript[0].Text != "good" {
		t.Errorf("torn line poisoned replay: %+v", got.transcript)
	}
}
