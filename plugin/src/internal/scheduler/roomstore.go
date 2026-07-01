package scheduler

// Roundtable room persistence — the third persisted store, mirroring the
// per-agent TranscriptStore and the invocation ledger so a scheduler restart no
// longer loses group-chat history.
//
// Layout: one append-only JSONL file per room under <config>/.ahsir/rooms/,
// named <roomID>.jsonl. The roomID is a UUIDv7, already a safe filename, so no
// hashing/index is needed (unlike the transcript store, whose key is an
// arbitrary contextId). The first line is the "meta" record; subsequent lines
// are "turn" appends and "status" changes, replayed in order to reconstruct the
// Room. Owner-only modes (0700 dir / 0600 files), like the transcript store.
//
// Retention mirrors the transcript store: a room whose file mtime (= its last
// turn) is older than roomRetention is pruned at scheduler startup.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// roomRetention bounds how long an inactive room's log is kept. Matches the
// per-agent transcript retention so the two stores age out together.
const roomRetention = 30 * 24 * time.Hour

// roomRecord is one append-only line in a room's log. Kind selects which fields
// are populated: "meta" (room created), "turn" (a transcript entry), "status"
// (a lifecycle change, e.g. stopped).
type roomRecord struct {
	Kind         string     `json:"kind"`
	Topic        string     `json:"topic,omitempty"`
	Participants []string   `json:"participants,omitempty"`
	Organizer    string     `json:"organizer,omitempty"`
	MaxChain     int        `json:"maxChain,omitempty"`
	Mode         RoomMode   `json:"mode,omitempty"`      // "" = relay (back-compat with pre-mode logs)
	Moderator    string     `json:"moderator,omitempty"` // roundtable mode
	Budget       int        `json:"budget,omitempty"`    // roundtable mode
	CreatedAt    *time.Time `json:"createdAt,omitempty"` // pointer: time.Time zero value ignores omitempty
	Turn         *RoomTurn  `json:"turn,omitempty"`
	Status       RoomStatus `json:"status,omitempty"`
}

// RoomStore persists rooms under dir. All methods are nil-safe: a nil *RoomStore
// is a no-op store (persistence disabled), so callers never have to branch.
type RoomStore struct {
	mu  sync.Mutex
	dir string
}

// NewRoomStore returns a store rooted at dir, or nil when dir is empty (in-memory
// config). A nil store disables persistence; its methods are no-ops.
func NewRoomStore(dir string) *RoomStore {
	if dir == "" {
		return nil
	}
	return &RoomStore{dir: dir}
}

func (s *RoomStore) path(roomID string) string {
	return filepath.Join(s.dir, roomID+".jsonl")
}

// appendLocked writes one record to a room's log. Caller holds s.mu.
func (s *RoomStore) appendLocked(roomID string, rec roomRecord) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create rooms dir: %w", err)
	}
	f, err := os.OpenFile(s.path(roomID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(rec); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// WriteMeta records a room's metadata as the first line of its log.
func (s *RoomStore) WriteMeta(room *Room) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	created := room.createdAt
	return s.appendLocked(room.ID, roomRecord{
		Kind:         "meta",
		Topic:        room.Topic,
		Participants: room.Participants,
		Organizer:    room.Organizer,
		MaxChain:     room.MaxChain,
		Mode:         room.Mode,
		Moderator:    room.Moderator,
		Budget:       room.Budget,
		CreatedAt:    &created,
	})
}

// AppendTurn appends one transcript turn to a room's log.
func (s *RoomStore) AppendTurn(roomID string, turn RoomTurn) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := turn
	return s.appendLocked(roomID, roomRecord{Kind: "turn", Turn: &t})
}

// WriteStatus records a lifecycle change (e.g. stopped). Transient active/waiting
// transitions aren't persisted — recovered rooms always come back as waiting.
func (s *RoomStore) WriteStatus(roomID string, status RoomStatus) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(roomID, roomRecord{Kind: "status", Status: status})
}

// Load reconstructs every persisted room. Files without a usable meta line are
// skipped; a torn final line (crash mid-append) is dropped without poisoning the
// rest. Recovered rooms have no live drive loop, so non-terminal ones come back
// as waiting with in-flight scheduling reset.
func (s *RoomStore) Load() (map[string]*Room, error) {
	if s == nil {
		return map[string]*Room{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return map[string]*Room{}, nil
	}
	if err != nil {
		return nil, err
	}
	rooms := map[string]*Room{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		roomID := strings.TrimSuffix(e.Name(), ".jsonl")
		room := s.loadRoomLocked(roomID)
		if room != nil {
			rooms[roomID] = room
		}
	}
	return rooms, nil
}

func (s *RoomStore) loadRoomLocked(roomID string) *Room {
	f, err := os.Open(s.path(roomID))
	if err != nil {
		return nil
	}
	defer f.Close()

	var room *Room
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec roomRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // torn line — skip, don't poison the rest
		}
		switch rec.Kind {
		case "meta":
			created := time.Time{}
			if rec.CreatedAt != nil {
				created = *rec.CreatedAt
			}
			mode := rec.Mode
			if mode == "" {
				mode = RoomModeRelay // pre-mode logs are relay rooms
			}
			room = &Room{
				ID:           roomID,
				Topic:        rec.Topic,
				Participants: rec.Participants,
				Organizer:    rec.Organizer,
				MaxChain:     rec.MaxChain,
				Mode:         mode,
				Moderator:    rec.Moderator,
				Budget:       rec.Budget,
				status:       RoomWaiting,
				lastSeen:     make(map[string]int),
				createdAt:    created,
			}
		case "turn":
			if room != nil && rec.Turn != nil {
				room.transcript = append(room.transcript, *rec.Turn)
			}
		case "status":
			if room != nil {
				room.status = rec.Status
			}
		}
	}
	if room == nil {
		return nil // no meta line — unusable
	}
	// Rebuild turn-taking so an interrupted turn is retried after restart
	// (compensation): sets next/status/lastSeen from the transcript tail. The
	// actual retry is kicked off by RoomManager.ResumePending once agents are up.
	room.recoverScheduleLocked()
	return room
}

// CompactForRetention removes room logs whose last activity (file mtime) is
// older than roomRetention, at startup. Returns the number removed. Mirrors
// TranscriptStore.CompactForRetention.
func (s *RoomStore) CompactForRetention(now time.Time) (int, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-roomRetention)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && !os.IsNotExist(err) {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}
