package wrapper

// Transcript store tests (specs/2026-06-08-shared-context-collaboration.md).
//
// The transcript deliberately stores FULL turn content — userText and reply —
// unlike the scheduler ledger's 512-byte preview. Its protection is owner-only
// file modes and the wrapper API as the sole sanctioned access path, so the
// permission assertions here are part of the contract, not nice-to-haves.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestTranscriptStore(t *testing.T) (*TranscriptStore, string) {
	t.Helper()
	workspace := t.TempDir()
	return NewTranscriptStore(workspace), workspace
}

func TestTranscriptAppendAndRead(t *testing.T) {
	store, _ := newTestTranscriptStore(t)

	if err := store.Append("ctx-1", TranscriptTurn{
		TS:         time.Now(),
		TaskID:     "task-1",
		Speaker:    "alice",
		UserText:   "my favorite color is red",
		Reply:      "Noted.",
		Status:     "completed",
		DurationMS: 1200,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("ctx-1", TranscriptTurn{
		TS:       time.Now(),
		TaskID:   "task-2",
		Speaker:  "bob",
		UserText: "what is my favorite color?",
		Status:   "failed",
		Error:    "session turn: boom",
	}); err != nil {
		t.Fatal(err)
	}

	turns, err := store.Read("ctx-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns))
	}
	if turns[0].Turn != 1 || turns[1].Turn != 2 {
		t.Fatalf("turn numbers = %d,%d, want 1,2", turns[0].Turn, turns[1].Turn)
	}
	if turns[0].Speaker != "alice" || turns[0].Reply != "Noted." || turns[0].Status != "completed" {
		t.Fatalf("turn 1 roundtrip mismatch: %+v", turns[0])
	}
	if turns[1].Status != "failed" || turns[1].Error != "session turn: boom" {
		t.Fatalf("failed turn must persist its error: %+v", turns[1])
	}
}

func TestTranscriptTurnNumbersResumeAcrossReopen(t *testing.T) {
	store, workspace := newTestTranscriptStore(t)
	if err := store.Append("ctx-resume", TranscriptTurn{UserText: "first", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	// A fresh store over the same workspace (agent restart) must continue
	// numbering, not restart at 1.
	reopened := NewTranscriptStore(workspace)
	if err := reopened.Append("ctx-resume", TranscriptTurn{UserText: "second", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	turns, err := reopened.Read("ctx-resume")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[1].Turn != 2 {
		t.Fatalf("after reopen want turn=2 as second record, got %+v", turns)
	}
}

func TestTranscriptFileModes(t *testing.T) {
	store, workspace := newTestTranscriptStore(t)
	if err := store.Append("ctx-modes", TranscriptTurn{UserText: "hi", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(workspace, ".a2a", "transcripts")
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("transcripts dir mode = %o, want 700", di.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", e.Name(), fi.Mode().Perm())
		}
	}
}

// TestTranscriptFilenameSafety pins the path-traversal defence: contextId is
// caller-supplied and must never reach the filesystem raw.
func TestTranscriptFilenameSafety(t *testing.T) {
	store, workspace := newTestTranscriptStore(t)
	evil := "../../../tmp/evil"
	if err := store.Append(evil, TranscriptTurn{UserText: "attack", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(workspace, ".a2a", "transcripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no transcript files written inside the transcripts dir")
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") || strings.Contains(e.Name(), "/") {
			t.Errorf("unsafe transcript file name %q", e.Name())
		}
		if e.Name() == "index.json" {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".jsonl")
		if len(base) != 16 {
			t.Errorf("transcript file %q: want 16-hex-char base name", e.Name())
		}
	}
	// Nothing may escape the transcripts dir.
	if _, err := os.Stat(filepath.Join(workspace, "..", "..", "..", "tmp", "evil.jsonl")); err == nil {
		t.Error("transcript escaped the transcripts dir via contextId traversal")
	}
	// And the turn must still be readable under the original (raw) id.
	turns, err := store.Read(evil)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 || turns[0].UserText != "attack" {
		t.Fatalf("evil-id roundtrip failed: %+v", turns)
	}
}

func TestTranscriptReadUnknownContext(t *testing.T) {
	store, _ := newTestTranscriptStore(t)
	turns, err := store.Read("never-seen")
	if err != nil {
		t.Fatalf("unknown context must not error, got %v", err)
	}
	if len(turns) != 0 {
		t.Fatalf("unknown context turns = %d, want 0", len(turns))
	}
}

func TestTranscriptIndexMapsContexts(t *testing.T) {
	store, workspace := newTestTranscriptStore(t)
	if err := store.Append("ctx-a", TranscriptTurn{UserText: "a", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append("ctx-b", TranscriptTurn{UserText: "b", Status: "completed"}); err != nil {
		t.Fatal(err)
	}

	contexts, err := store.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	if len(contexts) != 2 {
		t.Fatalf("contexts = %d, want 2", len(contexts))
	}
	for _, ctxID := range []string{"ctx-a", "ctx-b"} {
		file, ok := contexts[ctxID]
		if !ok {
			t.Fatalf("index missing %s: %v", ctxID, contexts)
		}
		if _, err := os.Stat(filepath.Join(workspace, ".a2a", "transcripts", file)); err != nil {
			t.Errorf("index points at missing file %q: %v", file, err)
		}
	}
}

func TestTranscriptCompactForRetention(t *testing.T) {
	store, _ := newTestTranscriptStore(t)
	for _, ctx := range []string{"stale-1", "stale-2", "fresh"} {
		if err := store.Append(ctx, TranscriptTurn{TS: time.Now(), UserText: "hi", Status: "completed"}); err != nil {
			t.Fatal(err)
		}
	}

	// Age the two stale contexts' files past the 30d window via mtime; the
	// retention check reads mtime, so this simulates "last turn >30d ago".
	old := time.Now().Add(-35 * 24 * time.Hour)
	for _, ctx := range []string{"stale-1", "stale-2"} {
		p := filepath.Join(store.dir, transcriptFileName(ctx))
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	n, err := store.CompactForRetention(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("removed = %d, want 2", n)
	}

	// Stale transcripts gone (file + index + reads empty); fresh one intact.
	for _, ctx := range []string{"stale-1", "stale-2"} {
		if _, err := os.Stat(filepath.Join(store.dir, transcriptFileName(ctx))); !os.IsNotExist(err) {
			t.Errorf("%s file should be removed, stat err=%v", ctx, err)
		}
		if turns, _ := store.Read(ctx); len(turns) != 0 {
			t.Errorf("%s should read empty after prune, got %d turns", ctx, len(turns))
		}
	}
	idx, err := store.Contexts()
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []string{"stale-1", "stale-2"} {
		if _, ok := idx[ctx]; ok {
			t.Errorf("%s should be pruned from index", ctx)
		}
	}
	if _, ok := idx["fresh"]; !ok {
		t.Error("fresh context must remain in index")
	}
	if turns, err := store.Read("fresh"); err != nil || len(turns) != 1 {
		t.Fatalf("fresh turns = %d (err %v), want 1", len(turns), err)
	}
}

func TestTranscriptCompactEmptyStore(t *testing.T) {
	store, _ := newTestTranscriptStore(t)
	n, err := store.CompactForRetention(time.Now())
	if err != nil {
		t.Fatalf("compact on empty store: %v", err)
	}
	if n != 0 {
		t.Fatalf("removed = %d, want 0", n)
	}
}

func TestTranscriptCompactPrunesDanglingIndex(t *testing.T) {
	store, _ := newTestTranscriptStore(t)
	if err := store.Append("gone", TranscriptTurn{TS: time.Now(), UserText: "hi", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	// Delete the file out from under the index (external cleanup / crash).
	if err := os.Remove(filepath.Join(store.dir, transcriptFileName("gone"))); err != nil {
		t.Fatal(err)
	}
	n, err := store.CompactForRetention(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A dangling entry is pruned but not counted as a retention removal.
	if n != 0 {
		t.Fatalf("removed = %d, want 0 (dangling prune isn't a retention removal)", n)
	}
	if idx, _ := store.Contexts(); len(idx) != 0 {
		t.Errorf("dangling index entry should be pruned, got %v", idx)
	}
}
