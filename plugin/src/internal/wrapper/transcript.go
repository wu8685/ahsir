package wrapper

// Per-context transcript store (specs/2026-06-08-shared-context-collaboration.md).
//
// The transcript records FULL turn content — speaker, user text, reply — so a
// person joining a shared context can replay what happened before their first
// turn. This deliberately reverses the scheduler ledger's 512-byte-preview
// decision for this one file class: replay is the point. The protection
// posture compensates: owner-only modes (0700 dir / 0600 files), storage
// scoped to the agent workspace, and the wrapper's internal-token-protected
// /history endpoint as the sanctioned access path (where ACLs attach when
// ingress auth lands).
//
// contextId is caller-supplied and must never reach the filesystem raw — the
// file name is hex(sha256(contextId))[:16], and index.json maps contextId →
// file name for listing.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// transcriptRetention bounds how long an inactive context's transcript is kept.
// A context whose most recent turn is older than this is considered dead, and
// its transcript file + index entry are pruned at agent startup (see
// CompactForRetention) — mirroring the scheduler ledger's retention policy.
const transcriptRetention = 30 * 24 * time.Hour

// TranscriptExpired reports whether a context whose most recent turn is at
// lastTurn has aged past the retention window as of now — i.e. it would be
// pruned at the next agent startup (see CompactForRetention). Read-only
// consumers (the archived/offline history views) use this to avoid resurrecting
// transcripts the retention policy has effectively dropped, without mutating the
// workspace of an agent that is no longer running.
func TranscriptExpired(lastTurn, now time.Time) bool {
	return lastTurn.Before(now.Add(-transcriptRetention))
}

// TranscriptTurn is one completed (or failed) turn in a context's transcript.
type TranscriptTurn struct {
	TS         time.Time `json:"ts"`
	Turn       int       `json:"turn"`
	TaskID     string    `json:"taskId,omitempty"`
	Speaker    string    `json:"speaker,omitempty"`
	UserText   string    `json:"userText"`
	Reply      string    `json:"reply,omitempty"`
	Status     string    `json:"status"` // "completed" | "failed"
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"durationMs,omitempty"`
}

// TranscriptStore appends and reads per-context transcripts under
// <workspace>/.a2a/transcripts/. Appends are serialized by a single mutex —
// turns are seconds-long LLM round-trips, so contention is negligible.
type TranscriptStore struct {
	mu  sync.Mutex
	dir string
	// counters caches the next turn number per contextID. Lazily seeded from
	// the existing file so numbering resumes across process restarts.
	counters map[string]int
}

// NewTranscriptStore returns a store rooted at the workspace's
// .a2a/transcripts directory. The directory is created on first append, not
// here — a read-only consumer must not mutate the workspace.
func NewTranscriptStore(workspace string) *TranscriptStore {
	return &TranscriptStore{
		dir:      filepath.Join(workspace, ".a2a", "transcripts"),
		counters: make(map[string]int),
	}
}

// transcriptFileName derives the on-disk name for a contextID. Hashing (not
// sanitizing) guarantees no caller-supplied byte ever reaches the filesystem,
// killing path traversal by construction.
func transcriptFileName(contextID string) string {
	sum := sha256.Sum256([]byte(contextID))
	return hex.EncodeToString(sum[:])[:16] + ".jsonl"
}

// Append writes one turn record to the context's transcript file and updates
// index.json. The turn number is assigned here (1-based, resuming across
// restarts); any value in turn.Turn is overwritten.
func (s *TranscriptStore) Append(contextID string, turn TranscriptTurn) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create transcripts dir: %w", err)
	}

	next, ok := s.counters[contextID]
	if !ok {
		existing, err := s.readLocked(contextID)
		if err != nil {
			return err
		}
		next = len(existing)
	}
	turn.Turn = next + 1
	if turn.TS.IsZero() {
		turn.TS = time.Now()
	}

	path := filepath.Join(s.dir, transcriptFileName(contextID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	if err := json.NewEncoder(f).Encode(turn); err != nil {
		_ = f.Close()
		return fmt.Errorf("append transcript turn: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.counters[contextID] = turn.Turn

	return s.updateIndexLocked(contextID)
}

// Read returns all turns recorded for contextID in append order. Unknown
// contexts yield an empty slice, not an error — "no prior history" is a
// normal state for a fresh conversation.
func (s *TranscriptStore) Read(contextID string) ([]TranscriptTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(contextID)
}

func (s *TranscriptStore) readLocked(contextID string) ([]TranscriptTurn, error) {
	path := filepath.Join(s.dir, transcriptFileName(contextID))
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var turns []TranscriptTurn
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var turn TranscriptTurn
		if err := json.Unmarshal(line, &turn); err != nil {
			// A torn final line (crash mid-append) must not poison replay
			// of the turns before it.
			continue
		}
		turns = append(turns, turn)
	}
	return turns, scanner.Err()
}

// Contexts returns the contextID → file name mapping from index.json. Empty
// map when no transcript has ever been written.
func (s *TranscriptStore) Contexts() (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readIndexLocked()
}

// ContextSummary describes one on-disk context transcript for read-only
// listing (the archived/offline agent view). It carries just enough to render a
// left-rail row and open the full transcript via Read.
type ContextSummary struct {
	ContextID    string
	Title        string // first non-empty userText — the conversation's anchor
	Turns        int
	LastActivity time.Time
	LastStatus   string
}

// ListContexts summarises every context transcript still within the retention
// window (most recent turn newer than the cutoff as of now), newest first.
// Contexts past retention are omitted — they would be pruned at the next agent
// startup (see CompactForRetention), so surfacing them would defeat the policy.
// Read-only: it never mutates the workspace, so a deleted agent's history stays
// viewable without resurrecting the process.
func (s *TranscriptStore) ListContexts(now time.Time) ([]ContextSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.readIndexLocked()
	if err != nil {
		return nil, err
	}
	out := make([]ContextSummary, 0, len(index))
	for contextID := range index {
		turns, err := s.readLocked(contextID)
		if err != nil {
			return nil, err
		}
		if len(turns) == 0 {
			continue
		}
		last := turns[len(turns)-1]
		if TranscriptExpired(last.TS, now) {
			continue
		}
		out = append(out, ContextSummary{
			ContextID:    contextID,
			Title:        firstUserText(turns),
			Turns:        len(turns),
			LastActivity: last.TS,
			LastStatus:   last.Status,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastActivity.After(out[j].LastActivity)
	})
	return out, nil
}

// firstUserText returns the first non-empty user message in a transcript — used
// as a human-readable title, mirroring the console's context-title rule.
func firstUserText(turns []TranscriptTurn) string {
	for _, t := range turns {
		if t.UserText != "" {
			return t.UserText
		}
	}
	return ""
}

func (s *TranscriptStore) indexPath() string {
	return filepath.Join(s.dir, "index.json")
}

func (s *TranscriptStore) readIndexLocked() (map[string]string, error) {
	raw, err := os.ReadFile(s.indexPath())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	index := map[string]string{}
	if err := json.Unmarshal(raw, &index); err != nil {
		// A corrupt index is rebuildable state, not data — start fresh
		// rather than wedging every future append.
		return map[string]string{}, nil
	}
	return index, nil
}

// updateIndexLocked records contextID → file name. No-op when already present.
func (s *TranscriptStore) updateIndexLocked(contextID string) error {
	index, err := s.readIndexLocked()
	if err != nil {
		return err
	}
	file := transcriptFileName(contextID)
	if index[contextID] == file {
		return nil
	}
	index[contextID] = file
	return s.writeIndexLocked(index)
}

// writeIndexLocked persists the full index map with a same-dir temp + rename so
// readers never observe a torn index. Caller holds s.mu.
func (s *TranscriptStore) writeIndexLocked(index map[string]string) error {
	tmp, err := os.CreateTemp(s.dir, ".index-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := json.NewEncoder(tmp).Encode(index); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.indexPath())
}

// CompactForRetention removes the transcript of every context whose most recent
// turn is older than transcriptRetention, and prunes its index.json entry. The
// last-turn time is read from the file's mtime: transcripts are append-only, so
// mtime is exactly when the last turn was written. Index entries whose file has
// already vanished are pruned too. Returns the number of contexts removed.
//
// Called once at agent startup (see cmd/ahsir-agent), mirroring the scheduler
// ledger's startup compaction. Running agents never prune mid-session.
func (s *TranscriptStore) CompactForRetention(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	index, err := s.readIndexLocked()
	if err != nil {
		return 0, err
	}
	if len(index) == 0 {
		return 0, nil
	}

	cutoff := now.Add(-transcriptRetention)
	removed := 0
	changed := false
	for ctx, file := range index {
		info, statErr := os.Stat(filepath.Join(s.dir, file))
		if os.IsNotExist(statErr) {
			// Dangling index entry — the file is already gone. Drop it.
			delete(index, ctx)
			delete(s.counters, ctx)
			changed = true
			continue
		}
		if statErr != nil {
			return removed, statErr
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(s.dir, file)); err != nil && !os.IsNotExist(err) {
				return removed, err
			}
			delete(index, ctx)
			delete(s.counters, ctx)
			removed++
			changed = true
		}
	}

	if changed {
		if err := s.writeIndexLocked(index); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
