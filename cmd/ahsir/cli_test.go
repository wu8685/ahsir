package main

import (
	"flag"
	"os/user"
	"reflect"
	"testing"
)

// --- speaker attribution (specs/2026-06-08-shared-context-collaboration.md) ---

// TestResolveSpeakerExplicitWins: a user-provided --as value is used verbatim.
func TestResolveSpeakerExplicitWins(t *testing.T) {
	if got := resolveSpeaker("alice"); got != "alice" {
		t.Fatalf("resolveSpeaker(alice) = %q, want alice", got)
	}
}

// TestResolveSpeakerDefaultsToOSUser: empty --as falls back to the OS
// username so attribution is on by default with zero configuration.
func TestResolveSpeakerDefaultsToOSUser(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skipf("user.Current unavailable: %v", err)
	}
	if got := resolveSpeaker(""); got != u.Username {
		t.Fatalf("resolveSpeaker(\"\") = %q, want OS username %q", got, u.Username)
	}
}

func TestRequiredPathFlagIsRepeatable(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var paths pathListFlag
	fs.Var(&paths, "require-path", "")
	positionals := parsePositionals(fs, []string{"reviewer", "review", "--require-path", "/repo/a", "--require-path", "/repo/b"})
	if !reflect.DeepEqual([]string(paths), []string{"/repo/a", "/repo/b"}) {
		t.Fatalf("paths = %v", paths)
	}
	if !reflect.DeepEqual(positionals, []string{"reviewer", "review"}) {
		t.Fatalf("positionals = %v", positionals)
	}
}
