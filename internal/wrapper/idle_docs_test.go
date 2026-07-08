package wrapper

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestScaleToZeroDocumented guards the issue-9 user documentation against drift.
//
// The scale-to-zero feature (runtime.idle_timeout, issue #6) originally shipped
// as code only, with zero user-visible docs. This test fails the build if that
// regresses: each user-facing doc must mention the field, the documented default
// must track DefaultIdleTimeout (so bumping the constant forces a doc update),
// and the docs that disambiguate idle_ttl vs idle_timeout must keep doing so.
func TestScaleToZeroDocumented(t *testing.T) {
	// Human-readable default the docs quote, derived from the code constant.
	// If DefaultIdleTimeout stops being a whole number of minutes, this guard
	// needs revisiting — assert that assumption explicitly.
	if DefaultIdleTimeout%60 != 0 {
		t.Fatalf("DefaultIdleTimeout %v is no longer a whole number of minutes; update this doc guard", DefaultIdleTimeout)
	}
	defaultStr := fmt.Sprintf("%dm", int(DefaultIdleTimeout.Minutes())) // e.g. "10m"

	cases := []struct {
		path string
		must []string
	}{
		{
			// README: agent-card runtime example + value blurb.
			path: "../../README.md",
			must: []string{"idle_timeout", defaultStr},
		},
		{
			// features.md: lifecycle subsection + idle_ttl vs idle_timeout table
			// + idle-stopped semantics.
			path: "../../docs/features.md",
			must: []string{"idle_timeout", "idle_ttl", "idle-stopped", defaultStr},
		},
		{
			// example config reference: one row for the new knob.
			path: "../../example/multi-agent/README.md",
			must: []string{"idle_timeout"},
		},
	}

	for _, c := range cases {
		b, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		doc := string(b)
		for _, sub := range c.must {
			if !strings.Contains(doc, sub) {
				t.Errorf("%s: missing required scale-to-zero doc string %q (issue #9)", c.path, sub)
			}
		}
	}
}
