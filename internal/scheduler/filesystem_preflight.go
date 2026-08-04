package scheduler

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/wu8685/ahsir/internal/wrapper"
)

// FilesystemPreflightError is safe to return to a dispatch caller. It names
// only the selected agent and requested path; the card's allowlist stays
// private.
type FilesystemPreflightError struct {
	Code  string
	Agent string
	Path  string
	Cause error
}

func (e *FilesystemPreflightError) Error() string {
	switch e.Code {
	case "filesystem_disabled":
		return fmt.Sprintf("agent %s has filesystem access disabled; cannot access %s", e.Agent, e.Path)
	case "filesystem_path_invalid":
		return fmt.Sprintf("required filesystem path %s is invalid or unavailable", e.Path)
	case "filesystem_policy_unavailable":
		return fmt.Sprintf("filesystem policy for agent %s is unavailable; cannot verify %s", e.Agent, e.Path)
	default:
		return fmt.Sprintf("agent %s cannot access %s; select or reconfigure a project-scoped agent", e.Agent, e.Path)
	}
}

func (e *FilesystemPreflightError) Unwrap() error { return e.Cause }

// PreflightFilesystem proves that every explicitly-required path is covered by
// the selected managed agent's effective card allowlist. Empty requirements
// preserve the historical dispatch behavior.
func (s *Scheduler) PreflightFilesystem(agentName string, requiredPaths []string) error {
	if len(requiredPaths) == 0 {
		return nil
	}
	baseName := agentName
	if base, _, ok := parseInstanceName(agentName); ok {
		baseName = base
	}

	s.mu.Lock()
	cfg, managed := s.desired[baseName]
	if !managed {
		for _, configured := range s.cfg.Agents {
			if configured.Name == baseName && configured.Remote == "" {
				cfg, managed = configured, true
				break
			}
		}
	}
	s.mu.Unlock()
	if !managed || cfg.Workspace == "" {
		return newPreflightError("filesystem_policy_unavailable", baseName, requiredPaths[0], nil)
	}

	card, err := wrapper.NewAgentCardBuilder(cfg.Workspace).Load()
	if err != nil {
		return newPreflightError("filesystem_policy_unavailable", baseName, requiredPaths[0], err)
	}
	if !card.Filesystem.Enabled {
		return newPreflightError("filesystem_disabled", baseName, requiredPaths[0], nil)
	}

	workdir := cfg.Workdir
	if workdir == "" {
		workdir = cfg.Workspace
	}
	allowed := append([]string(nil), card.Filesystem.AllowedPaths...)
	allowed = append(allowed, wrapper.ResolveUploadDir())
	canonicalAllowed := make([]string, 0, len(allowed))
	for _, root := range allowed {
		canonical, err := canonicalFilesystemPath(workdir, root)
		if err != nil {
			continue
		}
		canonicalAllowed = append(canonicalAllowed, canonical)
	}
	if len(canonicalAllowed) == 0 {
		return newPreflightError("filesystem_policy_unavailable", baseName, requiredPaths[0], nil)
	}

	for _, required := range requiredPaths {
		if strings.TrimSpace(required) == "" {
			return newPreflightError("filesystem_path_invalid", baseName, required, nil)
		}
		canonical, err := canonicalFilesystemPath(workdir, required)
		if err != nil {
			return newPreflightError("filesystem_path_invalid", baseName, required, err)
		}
		permitted := false
		for _, root := range canonicalAllowed {
			if pathContainedBy(root, canonical) {
				permitted = true
				break
			}
		}
		if !permitted {
			return newPreflightError("filesystem_access_denied", baseName, required, nil)
		}
	}
	return nil
}

func newPreflightError(code, agent, path string, cause error) *FilesystemPreflightError {
	return &FilesystemPreflightError{Code: code, Agent: agent, Path: path, Cause: cause}
}

func canonicalFilesystemPath(base, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func pathContainedBy(root, required string) bool {
	rel, err := filepath.Rel(root, required)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
