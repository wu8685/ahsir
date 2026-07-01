package wrapper

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// persistFileVersion is the on-disk schema version. Bump when changing the
// PersistedRecord shape in a way that older readers can't handle.
const persistFileVersion = 1

// persistFile is the JSON envelope written to disk. Wrapping records in an
// envelope (rather than serializing the map at top level) leaves room for
// future metadata — schema version, agent identity, etc. — without another
// migration.
type persistFile struct {
	Version int                        `json:"version"`
	Entries map[string]PersistedRecord `json:"entries"`
}

// FilePersistence is a JSON-file-backed Persistence implementation suitable
// for a single ahsir-agent process. Writes are atomic (tmp + rename); a
// corrupt file is renamed to `<path>.broken-<RFC3339-timestamp>` so the
// agent can start fresh instead of refusing to come up.
//
// The path is typically `<workspace>/.a2a/sessions.json`. Parent directory
// is created with 0700 on first save; the file itself is 0600. Session IDs
// aren't cryptographic secrets but there's no reason to make them world-
// readable either.
type FilePersistence struct {
	path string
}

// NewFilePersistence returns a Persistence backed by the file at path. The
// file does not need to exist yet — first Save creates it.
func NewFilePersistence(path string) *FilePersistence {
	return &FilePersistence{path: path}
}

// Load reads the file and returns the stored records. Behaviour:
//   - file missing → empty map, nil error
//   - file unreadable → empty map, error (caller logs and continues)
//   - file present but unparseable → empty map, nil error; the corrupt file
//     is renamed to <path>.broken-<timestamp> so the operator can salvage it
func (f *FilePersistence) Load() (map[string]PersistedRecord, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]PersistedRecord{}, nil
		}
		return nil, fmt.Errorf("read persist file %s: %w", f.path, err)
	}
	var pf persistFile
	if err := json.Unmarshal(data, &pf); err != nil {
		// Corrupt file: rename, log, return empty. Always-startable beats
		// strict-correct for a session cache.
		broken := f.path + ".broken-" + time.Now().UTC().Format("20060102T150405Z")
		if renameErr := os.Rename(f.path, broken); renameErr != nil {
			log.Printf("session pool: corrupt persist file at %s (parse: %v); rename to %s also failed: %v — starting empty", f.path, err, broken, renameErr)
		} else {
			log.Printf("session pool: corrupt persist file at %s (parse: %v); moved to %s — starting empty", f.path, err, broken)
		}
		return map[string]PersistedRecord{}, nil
	}
	if pf.Entries == nil {
		return map[string]PersistedRecord{}, nil
	}
	return pf.Entries, nil
}

// Save atomically replaces the file with a fresh snapshot. Uses tmp + rename
// so a crash mid-write never leaves a truncated file — readers either see
// the previous snapshot or the new one, never something in between.
func (f *FilePersistence) Save(records map[string]PersistedRecord) error {
	if records == nil {
		records = map[string]PersistedRecord{}
	}

	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("mkdir parent of %s: %w", f.path, err)
	}

	payload, err := json.MarshalIndent(persistFile{
		Version: persistFileVersion,
		Entries: records,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal records: %w", err)
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(f.path), filepath.Base(f.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure tmp file is cleaned up on failure paths.
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(payload); err != nil {
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, f.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename %s → %s: %w", tmpPath, f.path, err)
	}
	return nil
}
