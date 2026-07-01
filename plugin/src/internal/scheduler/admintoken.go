package scheduler

// Admin token store (specs/2026-06-08-auth-baseline.md).
//
// A single privileged token gates the control plane (/admin/agents + registry
// write). It is auto-generated to a 0600 file beside ahsir.yaml so the
// same-OS-user case is zero-friction (the CLI reads the same file), while a
// different OS user — unable to read the 0600 file — is locked out. The trust
// boundary is "can read this file" ≡ "is the same OS user". AHSIR_ADMIN_TOKEN
// overrides the file for CI / container use where writing a file is unwanted.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AdminTokenEnvVar overrides the on-disk token entirely when set non-empty.
const AdminTokenEnvVar = "AHSIR_ADMIN_TOKEN"

// adminTokenFileName is the token file, written beside the resolved ahsir.yaml.
const adminTokenFileName = "admin-token"

// Token source labels, surfaced by `ahsir doctor` and startup logs.
const (
	adminTokenSourceEnv           = "env"
	adminTokenSourceFile          = "file"
	adminTokenSourceFileGenerated = "file (generated)"
)

// AdminTokenPath returns the token file path for a given config path: a sibling
// of ahsir.yaml. Exposed so the CLI's read-only resolver and the scheduler's
// generator agree on location.
func AdminTokenPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), adminTokenFileName)
}

// LoadOrCreateAdminToken resolves the admin token for a scheduler rooted at
// configPath. Order: AHSIR_ADMIN_TOKEN env (no file touched) → existing file →
// generate a new 0600 file. A corrupt/empty file is regenerated (the token is
// rebuildable state and must never wedge startup). Returns the token and a
// human-readable source label.
func LoadOrCreateAdminToken(configPath string) (token, source string, err error) {
	if env := strings.TrimSpace(os.Getenv(AdminTokenEnvVar)); env != "" {
		return env, adminTokenSourceEnv, nil
	}

	tokenPath := AdminTokenPath(configPath)
	if data, readErr := os.ReadFile(tokenPath); readErr == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, adminTokenSourceFile, nil
		}
		// Empty/whitespace file → fall through to regenerate.
	} else if !os.IsNotExist(readErr) {
		return "", "", fmt.Errorf("read admin token %s: %w", tokenPath, readErr)
	}

	tok, genErr := newInternalToken()
	if genErr != nil {
		return "", "", fmt.Errorf("generate admin token: %w", genErr)
	}
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		return "", "", fmt.Errorf("create config dir for admin token: %w", err)
	}
	if err := os.WriteFile(tokenPath, []byte(tok+"\n"), 0o600); err != nil {
		return "", "", fmt.Errorf("write admin token %s: %w", tokenPath, err)
	}
	return tok, adminTokenSourceFileGenerated, nil
}

// loadAdminToken resolves and caches the scheduler's admin token. With
// AHSIR_ADMIN_TOKEN set it uses that; with a config path it loads-or-creates
// the 0600 file beside ahsir.yaml; with neither (a bare test scheduler) it
// leaves the token empty, which disables enforcement. Idempotent.
func (s *Scheduler) loadAdminToken() error {
	if env := strings.TrimSpace(os.Getenv(AdminTokenEnvVar)); env != "" {
		s.adminTok, s.adminTokSource = env, adminTokenSourceEnv
		return nil
	}
	if s.cfg == nil || s.cfg.path == "" {
		s.adminTok, s.adminTokSource = "", "disabled (no config path)"
		return nil
	}
	tok, source, err := LoadOrCreateAdminToken(s.cfg.path)
	if err != nil {
		return err
	}
	s.adminTok, s.adminTokSource = tok, source
	return nil
}

// adminToken returns the cached admin token ("" when auth is disabled).
func (s *Scheduler) adminToken() string { return s.adminTok }

// adminTokenSource returns a human-readable label for where the token came
// from, for `ahsir doctor` and startup logs.
func (s *Scheduler) adminTokenSource() string { return s.adminTokSource }

// ReadAdminToken is the read-only resolver: AHSIR_ADMIN_TOKEN env → existing
// file. It never generates — used by the CLI, which must not create a token the
// scheduler doesn't know about. Returns "" when neither source yields one.
func ReadAdminToken(configPath string) (token, source string) {
	if env := strings.TrimSpace(os.Getenv(AdminTokenEnvVar)); env != "" {
		return env, adminTokenSourceEnv
	}
	if data, err := os.ReadFile(AdminTokenPath(configPath)); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, adminTokenSourceFile
		}
	}
	return "", ""
}
