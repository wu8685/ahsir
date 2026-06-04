package wrapper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFilePersistence_RoundTrip verifies that Save → Load returns the same
// records, including all timestamp fields.
func TestFilePersistence_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	fp := NewFilePersistence(path)

	now := time.Date(2026, 6, 4, 22, 31, 0, 0, time.UTC)
	in := map[string]PersistedRecord{
		"ctx-1": {SessionID: "sid-1", State: persistStateEvicted, LastUsed: now, EvictedAt: now.Add(5 * time.Minute)},
		"ctx-2": {SessionID: "sid-2", State: persistStateActive, LastUsed: now.Add(time.Hour)},
	}
	if err := fp.Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := fp.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("expected %d records, got %d", len(in), len(out))
	}
	for k, want := range in {
		got, ok := out[k]
		if !ok {
			t.Fatalf("missing key %q after round-trip", k)
		}
		if got.SessionID != want.SessionID || got.State != want.State {
			t.Errorf("%s: got %+v, want %+v", k, got, want)
		}
		if !got.LastUsed.Equal(want.LastUsed) {
			t.Errorf("%s lastUsed: got %v, want %v", k, got.LastUsed, want.LastUsed)
		}
		if !got.EvictedAt.Equal(want.EvictedAt) {
			t.Errorf("%s evictedAt: got %v, want %v", k, got.EvictedAt, want.EvictedAt)
		}
	}
}

// TestFilePersistence_LoadMissingFile is the cold-start case — no file yet.
// Must return an empty map, no error, so the agent boots normally.
func TestFilePersistence_LoadMissingFile(t *testing.T) {
	fp := NewFilePersistence(filepath.Join(t.TempDir(), "does-not-exist.json"))
	out, err := fp.Load()
	if err != nil {
		t.Fatalf("Load on missing file should be nil err, got %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map, got %v", out)
	}
}

// TestFilePersistence_LoadCorruptFile verifies the "always-startable"
// invariant: a corrupted JSON file must NOT block startup. It should be
// moved aside (so the operator can salvage) and Load should return empty.
func TestFilePersistence_LoadCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := NewFilePersistence(path)

	out, err := fp.Load()
	if err != nil {
		t.Fatalf("Load on corrupt file should be nil err, got %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map from corrupt file, got %v", out)
	}

	// The original file should be moved aside (not deleted) so the operator
	// can inspect it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var foundBroken bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sessions.json.broken-") {
			foundBroken = true
		}
	}
	if !foundBroken {
		t.Errorf("expected a sessions.json.broken-* file in %s, got entries: %v", dir, entries)
	}
}

// TestFilePersistence_SaveIsAtomic verifies the tmp-then-rename strategy
// leaves no temp files behind on success and never leaves a half-written
// file at the canonical path.
func TestFilePersistence_SaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	fp := NewFilePersistence(path)

	for i := 0; i < 5; i++ {
		if err := fp.Save(map[string]PersistedRecord{
			"ctx-x": {SessionID: "sid-x", State: persistStateActive},
		}); err != nil {
			t.Fatalf("Save iter %d: %v", i, err)
		}
	}

	// After several saves, exactly one file should remain — no leftover
	// .tmp-* siblings.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "sessions.json" {
			t.Errorf("unexpected leftover file %q in dir after Save (only sessions.json should remain)", e.Name())
		}
	}
}

// TestFilePersistence_SaveCreatesParentDir verifies that Save can populate
// a fresh directory tree — caller doesn't have to mkdir first. Important
// because <workspace>/.a2a/ may not exist yet at first Save.
func TestFilePersistence_SaveCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "subdir", "sessions.json")
	fp := NewFilePersistence(path)
	if err := fp.Save(map[string]PersistedRecord{"ctx-1": {SessionID: "sid-1"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created at %s: %v", path, err)
	}
}

// TestFilePersistence_SaveEmptyMap covers the "all entries GC'd" case —
// the file should be left with a valid empty-entries JSON, not removed.
func TestFilePersistence_SaveEmptyMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	fp := NewFilePersistence(path)
	if err := fp.Save(map[string]PersistedRecord{}); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	out, err := fp.Load()
	if err != nil {
		t.Fatalf("Load after empty save: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map, got %v", out)
	}
}
