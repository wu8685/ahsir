// Package version exposes the ahsir build version.
//
// Version is the single source of truth for the semantic version and is stamped
// at build time from plugin/.claude-plugin/plugin.json via -ldflags (see the
// Makefile). A plain `go build` with no ldflags leaves it as "dev"; in that case
// String() falls back to the git revision/time the Go toolchain embeds
// automatically (buildvcs), so even unstamped builds are identifiable.
package version

import (
	"runtime/debug"
	"time"
)

// Version is the semantic version, overridden at build time with
//
//	-ldflags "-X github.com/wu8685/ahsir/internal/version.Version=<v>"
//
// It stays "dev" for un-stamped builds.
var Version = "dev"

// String returns a human-readable build identifier, e.g.
//
//	ahsir 0.6.0 (a1b2c3d, 2026-06-11)
//	ahsir dev (a1b2c3d+dirty, 2026-06-11)   // unstamped build
//	ahsir 0.6.0                              // no VCS info available
func String() string {
	out := "ahsir " + Version
	if rev, dirty, when := vcsInfo(); rev != "" {
		suffix := rev
		if dirty {
			suffix += "+dirty"
		}
		if when != "" {
			suffix += ", " + when
		}
		out += " (" + suffix + ")"
	}
	return out
}

// vcsInfo pulls the git revision (short), dirty flag, and date from the build
// info the Go toolchain stamps automatically (buildvcs=on by default). Returns
// empty strings when built without VCS info (e.g. from a tarball).
func vcsInfo() (rev string, dirty bool, date string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false, ""
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				rev = s.Value[:7]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				date = t.Format("2006-01-02")
			}
		}
	}
	return rev, dirty, date
}
