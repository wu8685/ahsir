package wrapper

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var errFakeGit = errors.New("fake git failure")

func TestParseIsolationMode(t *testing.T) {
	cases := []struct {
		in      string
		want    IsolationMode
		wantErr bool
	}{
		{"", IsolationOff, false},
		{"off", IsolationOff, false},
		{"none", IsolationOff, false},
		{"  OFF  ", IsolationOff, false},
		{"scratch", IsolationScratch, false},
		{"Scratch", IsolationScratch, false},
		{"worktree", IsolationWorktree, false},
		{"WORKTREE", IsolationWorktree, false},
		{"bogus", IsolationOff, true},
	}
	for _, c := range cases {
		got, err := ParseIsolationMode(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseIsolationMode(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
		}
		if got != c.want {
			t.Errorf("ParseIsolationMode(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestIsolationModeStringAndEnabled(t *testing.T) {
	if !IsolationScratch.Enabled() || !IsolationWorktree.Enabled() {
		t.Fatal("scratch/worktree should be Enabled()")
	}
	if IsolationOff.Enabled() {
		t.Fatal("off should not be Enabled()")
	}
	if IsolationScratch.String() != "scratch" || IsolationWorktree.String() != "worktree" || IsolationOff.String() != "off" {
		t.Fatalf("unexpected String(): %s/%s/%s", IsolationOff, IsolationScratch, IsolationWorktree)
	}
	if !strings.Contains(IsolationMode(99).String(), "unknown") {
		t.Fatalf("unknown mode String() = %q", IsolationMode(99))
	}
}

func TestSessionDirNameSafeAndCollisionResistant(t *testing.T) {
	// Unsafe characters are replaced; result stays filesystem-safe.
	name := sessionDirName("ctx/../with spaces:and*stuff")
	if strings.ContainsAny(name, "/ :*.\\") {
		t.Fatalf("sanitised name still unsafe: %q", name)
	}

	// Distinct ids that sanitise to the same prefix must NOT collide, because
	// the hash of the full id is appended.
	a := sessionDirName("a/b")
	b := sessionDirName("a*b")
	if a == b {
		t.Fatalf("distinct contextIDs collided: %q == %q", a, b)
	}

	// Deterministic.
	if sessionDirName("same") != sessionDirName("same") {
		t.Fatal("sessionDirName not deterministic")
	}

	// Empty id still yields a usable name with the "ctx" placeholder prefix.
	if got := sessionDirName(""); !strings.HasPrefix(got, "ctx-") {
		t.Fatalf("empty id name = %q, want ctx- prefix", got)
	}

	// Long ids are bounded (prefix capped at 48 + "-" + 8-char hash).
	long := sessionDirName(strings.Repeat("x", 500))
	if len(long) != 48+1+8 {
		t.Fatalf("long id name length = %d want %d", len(long), 48+1+8)
	}
}

// fakeGit records calls and lets tests control isWorkTree without a real repo.
type fakeGit struct {
	workTree bool
	added    []string
	removed  []string
	addErr   error
	removeErr error
}

func (f *fakeGit) isWorkTree(dir string) bool { return f.workTree }
func (f *fakeGit) addWorktree(repoDir, path string) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, path)
	// Simulate git creating the checkout directory.
	return os.MkdirAll(path, 0o700)
}
func (f *fakeGit) removeWorktree(repoDir, path string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, path)
	return os.RemoveAll(path)
}

func TestSessionWorkspaceScratchProvisionAndRemove(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	ws := newSessionWorkspace("/nonexistent-repo", base, IsolationScratch, &fakeGit{})

	dir, err := ws.Provision("ctx-1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if filepath.Dir(dir) != base {
		t.Fatalf("dir %q not under base %q", dir, base)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("scratch dir not created: %v", err)
	}

	// Idempotent: second Provision returns same path and preserves contents.
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir2, err := ws.Provision("ctx-1")
	if err != nil || dir2 != dir {
		t.Fatalf("second Provision dir=%q err=%v (want %q)", dir2, err, dir)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("idempotent Provision wiped contents: %v", err)
	}

	// Distinct contexts get distinct dirs.
	other, err := ws.Provision("ctx-2")
	if err != nil || other == dir {
		t.Fatalf("ctx-2 dir=%q err=%v (want != %q)", other, err, dir)
	}

	// Remove deletes the dir; second Remove on a missing dir is a no-op.
	if err := ws.Remove("ctx-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still present after Remove: %v", err)
	}
	if err := ws.Remove("ctx-1"); err != nil {
		t.Fatalf("Remove of missing dir should be nil, got %v", err)
	}
}

func TestSessionWorkspaceWorktreeMode(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	fg := &fakeGit{workTree: true}
	ws := newSessionWorkspace("/repo", base, IsolationWorktree, fg)
	if ws.Mode() != IsolationWorktree {
		t.Fatalf("mode=%v want worktree", ws.Mode())
	}

	dir, err := ws.Provision("ctx-1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(fg.added) != 1 || fg.added[0] != dir {
		t.Fatalf("addWorktree not called for dir %q: %v", dir, fg.added)
	}

	if err := ws.Remove("ctx-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(fg.removed) != 1 || fg.removed[0] != dir {
		t.Fatalf("removeWorktree not called for dir %q: %v", dir, fg.removed)
	}
}

func TestSessionWorkspaceWorktreeFallsBackToScratch(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	fg := &fakeGit{workTree: false} // repoDir not a git work tree
	ws := newSessionWorkspace("/not-a-repo", base, IsolationWorktree, fg)
	if ws.Mode() != IsolationScratch {
		t.Fatalf("mode=%v want scratch fallback", ws.Mode())
	}
	dir, err := ws.Provision("ctx-1")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(fg.added) != 0 {
		t.Fatalf("worktree add should not run in scratch fallback: %v", fg.added)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("fallback scratch dir not created: %v", err)
	}
}

func TestSessionWorkspaceProvisionErrors(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	ws := newSessionWorkspace("/repo", base, IsolationScratch, &fakeGit{})

	// A path that already exists as a FILE (not a dir) is an error, not reuse.
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := ws.SessionDir("ctx-file")
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Provision("ctx-file"); err == nil {
		t.Fatal("want error when session path exists as a file")
	}

	// A failing git worktree add propagates as an error.
	wtWS := newSessionWorkspace("/repo", filepath.Join(t.TempDir(), "s2"), IsolationWorktree, &fakeGit{workTree: true, addErr: errFakeGit})
	if _, err := wtWS.Provision("ctx-add-fail"); err == nil {
		t.Fatal("want error when git worktree add fails")
	}
}

func TestSessionWorkspaceRemoveWorktreeFallback(t *testing.T) {
	base := filepath.Join(t.TempDir(), "sessions")
	// removeWorktree fails, so Remove must fall through to a plain RemoveAll and
	// still delete the directory.
	fg := &fakeGit{workTree: true, removeErr: errFakeGit}
	ws := newSessionWorkspace("/repo", base, IsolationWorktree, fg)
	dir, err := ws.Provision("ctx-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Remove("ctx-1"); err != nil {
		t.Fatalf("Remove with failing git remove should still succeed via RemoveAll: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir not removed by fallback: %v", err)
	}
}

func TestWithSessionWorkspace(t *testing.T) {
	base := SessionConfig{
		Name:    "a",
		Command: "claude",
		Args:    []string{"--strict-mcp-config"},
		WorkDir: "/orig",
	}
	got := WithSessionWorkspace(base, "/sessions/ctx-1")
	if got.WorkDir != "/sessions/ctx-1" {
		t.Fatalf("WorkDir=%q want /sessions/ctx-1", got.WorkDir)
	}
	if got.Args[len(got.Args)-1] != "--add-dir=/sessions/ctx-1" {
		t.Fatalf("missing add-dir flag: %v", got.Args)
	}
	// Original config untouched (Args slice copied, WorkDir unchanged).
	if base.WorkDir != "/orig" || len(base.Args) != 1 {
		t.Fatalf("base config mutated: %+v", base)
	}
}

// TestSessionWorkspaceRealGitWorktree exercises the real git plumbing end to
// end, so the production execGitRunner path is actually covered. Skipped when
// git is unavailable.
func TestSessionWorkspaceRealGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	runInRepo := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runInRepo("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	runInRepo("add", ".")
	runInRepo("commit", "-q", "-m", "init")

	base := filepath.Join(repo, ".a2a", "sessions")
	ws := NewSessionWorkspace(repo, base, IsolationWorktree)
	if ws.Mode() != IsolationWorktree {
		t.Fatalf("expected worktree mode on a real repo, got %v", ws.Mode())
	}

	dir, err := ws.Provision("ctx-real")
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// The worktree is a real checkout: the tracked file is present.
	if b, err := os.ReadFile(filepath.Join(dir, "tracked.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("worktree checkout missing tracked file: %q err=%v", b, err)
	}

	// Idempotent even for a real worktree.
	dir2, err := ws.Provision("ctx-real")
	if err != nil || dir2 != dir {
		t.Fatalf("second Provision dir=%q err=%v (want %q)", dir2, err, dir)
	}

	if err := ws.Remove("ctx-real"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still present after Remove: %v", err)
	}
	// git no longer lists the removed worktree.
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worktree list: %v: %s", err, out)
	}
	if strings.Contains(string(out), dir) {
		t.Fatalf("removed worktree still registered: %s", out)
	}
}
