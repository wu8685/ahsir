package ui

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestParticipantSelection runs the browser-side regression suite against the
// exact app.js embedded by this package. Node is only a test runner here; the
// suite supplies a tiny DOM and mocked API responses, so it needs no browser or
// third-party JavaScript packages.
func TestParticipantSelection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping browser-side app.js regression test")
	}

	cmd := exec.Command(node, "app_participant_test.js", filepath.Join("assets", "app.js"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("participant app.js regression test failed: %v\n%s", err, out)
	}
}
