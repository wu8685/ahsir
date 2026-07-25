package wrapper

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsolationMode selects how a SessionPool isolates the filesystem working tree
// of one A2A contextID (one session) from its siblings within the SAME
// ahsir-agent process. Sessions already get separate provider subprocesses; the
// mode here governs whether they ALSO get separate scratch directories so a
// `git checkout` / build / generated file in one session cannot race a sibling.
type IsolationMode int

const (
	// IsolationOff keeps the historical behaviour: every session shares the one
	// agent workdir. No per-session scratch is created.
	IsolationOff IsolationMode = iota

	// IsolationScratch gives each session an empty private directory under
	// <workspace>/.a2a/sessions/<id>/ as its cwd. Cheap and dependency-free —
	// useful for agents that only generate files and don't need the repo tree.
	IsolationScratch

	// IsolationWorktree gives each session its own `git worktree` checkout of
	// the agent workdir (detached at HEAD), so file mutations (checkout, build
	// output) stay isolated while all worktrees share one object store. Falls
	// back to IsolationScratch when the workdir is not a git working tree.
	IsolationWorktree
)

// ParseIsolationMode converts the agent-card.yaml `pool.session_isolation`
// value into the typed enum. Empty string returns IsolationOff so the field is
// optional and defaults to the historical shared-workdir behaviour.
func ParseIsolationMode(s string) (IsolationMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "none":
		return IsolationOff, nil
	case "scratch":
		return IsolationScratch, nil
	case "worktree":
		return IsolationWorktree, nil
	default:
		return IsolationOff, fmt.Errorf("unknown session_isolation %q (want off, scratch, or worktree)", s)
	}
}

func (m IsolationMode) String() string {
	switch m {
	case IsolationOff:
		return "off"
	case IsolationScratch:
		return "scratch"
	case IsolationWorktree:
		return "worktree"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// Enabled reports whether the mode provisions a per-session directory at all.
func (m IsolationMode) Enabled() bool { return m != IsolationOff }

// gitRunner abstracts the git plumbing SessionWorkspace needs, so tests can
// exercise the provisioning logic without a real repository (and one
// integration test can run the real thing).
type gitRunner interface {
	// isWorkTree reports whether dir is inside a git working tree.
	isWorkTree(dir string) bool
	// addWorktree creates a detached worktree at path rooted at repoDir's HEAD.
	addWorktree(repoDir, path string) error
	// removeWorktree unregisters and deletes the worktree at path.
	removeWorktree(repoDir, path string) error
}

// execGitRunner is the production gitRunner: it shells out to the `git` binary.
type execGitRunner struct{}

func (execGitRunner) isWorkTree(dir string) bool {
	out, err := runGit(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func (execGitRunner) addWorktree(repoDir, path string) error {
	// Clear any stale registration left by a previously rm'd worktree so the
	// add below doesn't fail with "already registered".
	_, _ = runGit(repoDir, "worktree", "prune")
	if out, err := runGit(repoDir, "worktree", "add", "--detach", path, "HEAD"); err != nil {
		return fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (execGitRunner) removeWorktree(repoDir, path string) error {
	if out, err := runGit(repoDir, "worktree", "remove", "--force", path); err != nil {
		// Best-effort: prune the registration so a later add at the same path
		// isn't blocked even if remove failed (e.g. path already gone).
		_, _ = runGit(repoDir, "worktree", "prune")
		return fmt.Errorf("git worktree remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runGit(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// SessionWorkspace hands each A2A contextID a private working directory,
// isolating one session's file mutations from its siblings inside a single
// agent process (issue #19). Directories are stable and deterministic per
// contextID: baseDir/<sanitised-id>-<hash>. Provisioning is idempotent, so a
// session that is evicted and later resumed reuses the same directory (and its
// uncommitted contents) rather than resetting.
type SessionWorkspace struct {
	repoDir       string        // git working tree root (the agent CLI cwd)
	baseDir       string        // parent of all per-session dirs
	effectiveMode IsolationMode // requested mode, downgraded to scratch if repoDir isn't git
	git           gitRunner
}

// NewSessionWorkspace builds a provisioner. repoDir is the agent's CLI cwd
// (the git working tree, when worktree mode is used); baseDir is where
// per-session directories are created (typically <workspace>/.a2a/sessions).
// When mode is IsolationWorktree but repoDir is not a git working tree, the
// mode is downgraded to IsolationScratch and a note is logged — a session still
// gets an isolated (if empty) directory rather than silently sharing the
// workdir.
func NewSessionWorkspace(repoDir, baseDir string, mode IsolationMode) *SessionWorkspace {
	return newSessionWorkspace(repoDir, baseDir, mode, execGitRunner{})
}

func newSessionWorkspace(repoDir, baseDir string, mode IsolationMode, git gitRunner) *SessionWorkspace {
	effective := mode
	if mode == IsolationWorktree && !git.isWorkTree(repoDir) {
		log.Printf("session workspace: session_isolation=worktree but %q is not a git working tree; falling back to scratch dirs", repoDir)
		effective = IsolationScratch
	}
	return &SessionWorkspace{
		repoDir:       repoDir,
		baseDir:       baseDir,
		effectiveMode: effective,
		git:           git,
	}
}

// Mode reports the effective isolation mode after any git-availability
// downgrade.
func (w *SessionWorkspace) Mode() IsolationMode { return w.effectiveMode }

// sessionDirName derives a filesystem-safe, collision-resistant directory name
// for a contextID. The human-readable prefix aids debugging; the appended hash
// of the FULL id guarantees two distinct contextIDs never collide even if their
// sanitised prefixes are identical or truncated.
func sessionDirName(contextID string) string {
	sum := sha1.Sum([]byte(contextID))
	hash := hex.EncodeToString(sum[:])[:8]

	var b strings.Builder
	for _, r := range contextID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	safe := b.String()
	if len(safe) > 48 {
		safe = safe[:48]
	}
	if safe == "" {
		safe = "ctx"
	}
	return safe + "-" + hash
}

// SessionDir returns the (deterministic) directory path for a contextID without
// creating anything.
func (w *SessionWorkspace) SessionDir(contextID string) string {
	return filepath.Join(w.baseDir, sessionDirName(contextID))
}

// Provision returns the private directory for contextID, creating it if
// necessary. It is idempotent: an existing directory is reused as-is so an
// evicted-then-resumed session keeps its working tree. In worktree mode a
// `git worktree` checkout is created; in scratch mode an empty directory.
func (w *SessionWorkspace) Provision(contextID string) (string, error) {
	dir := w.SessionDir(contextID)
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("session workspace %q exists but is not a directory", dir)
		}
		return dir, nil
	}
	if err := os.MkdirAll(w.baseDir, 0o700); err != nil {
		return "", fmt.Errorf("create session workspace base %q: %w", w.baseDir, err)
	}
	switch w.effectiveMode {
	case IsolationWorktree:
		if err := w.git.addWorktree(w.repoDir, dir); err != nil {
			return "", fmt.Errorf("provision session worktree for %q: %w", contextID, err)
		}
	default: // IsolationScratch
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("provision session scratch for %q: %w", contextID, err)
		}
	}
	return dir, nil
}

// Remove deletes the directory for contextID. In worktree mode it unregisters
// the git worktree first. Missing directories are not an error. Intended to be
// wired to the pool's "context permanently forgotten" signal so worktrees don't
// leak past the resumable lifetime of their session.
func (w *SessionWorkspace) Remove(contextID string) error {
	dir := w.SessionDir(contextID)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	if w.effectiveMode == IsolationWorktree {
		if err := w.git.removeWorktree(w.repoDir, dir); err == nil {
			return nil
		}
		// Fall through to a plain remove so a failed git remove still frees disk.
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove session workspace %q: %w", dir, err)
	}
	return nil
}

// WithSessionWorkspace returns a copy of cfg pointed at a per-session directory:
// its WorkDir becomes dir and an `--add-dir=<dir>` flag is appended so the CLI
// can also read/write there. cfg is not mutated (its Args slice is copied).
func WithSessionWorkspace(cfg SessionConfig, dir string) SessionConfig {
	out := cfg
	out.WorkDir = dir
	out.Args = append(append([]string(nil), cfg.Args...), "--add-dir="+dir)
	return out
}
