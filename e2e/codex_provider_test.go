//go:build e2e

package e2e

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestCodexProvider_E2E(t *testing.T) {
	fix := setupCodexE2E(t)

	const (
		contextID = "codex-provider-memory"
		codeword  = "codex-sapphire-42"
	)

	reply1, err := fix.sendMessageToAgent(
		"teacher",
		"msg-codex-teacher-1",
		contextID,
		fmt.Sprintf("Remember this codeword for the next turn: %s. Reply with exactly: stored %s", codeword, codeword),
	)
	if err != nil {
		t.Fatalf("teacher turn 1: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(strings.ToLower(reply1), codeword) {
		t.Fatalf("teacher turn 1 did not confirm codeword %q: %q", codeword, reply1)
	}

	reply2, err := fix.sendMessageToAgent(
		"teacher",
		"msg-codex-teacher-2",
		contextID,
		"What codeword did I ask you to remember? Reply with only the codeword.",
	)
	if err != nil {
		t.Fatalf("teacher turn 2: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(strings.ToLower(reply2), codeword) {
		t.Fatalf("teacher turn 2 did not resume Codex memory for %q: %q\n--- scheduler log ---\n%s", codeword, reply2, fix.schedulerLog())
	}

	starts := parseCodexStartLines(fix.schedulerLog())
	if len(starts) < 2 {
		t.Fatalf("expected at least two codex exec starts for teacher turns, got %d\n--- scheduler log ---\n%s", len(starts), fix.schedulerLog())
	}
	if !starts[1].hasResume {
		t.Fatalf("second codex exec should use resume with prior thread id; starts=%+v\n--- scheduler log ---\n%s", starts, fix.schedulerLog())
	}
	if starts[1].resumeID == "" {
		t.Fatalf("second codex exec resume id is empty: %+v", starts[1])
	}

	reply3, err := fix.sendMessageToAgent(
		"student",
		"msg-codex-student-1",
		"codex-provider-delegation",
		"Ask the teacher for the teacher's favorite fruit and relay the exact answer.",
	)
	if err != nil {
		t.Fatalf("student delegation: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(reply3, "papaya-5") {
		t.Fatalf("student delegation reply should contain the teacher-only answer, got %q\n--- scheduler log ---\n%s", reply3, fix.schedulerLog())
	}

	logs := fix.schedulerLog()
	for _, marker := range []string{
		"[student] receive",
		"[student → teacher] A2A_CALL",
		"[teacher] receive",
		"[student ← teacher] reply",
		"Executor wired: codex SessionPool",
		"codex session: started",
	} {
		if !strings.Contains(logs, marker) {
			t.Fatalf("scheduler log missing marker %q\n--- log ---\n%s", marker, logs)
		}
	}
}

type codexStartLine struct {
	hasResume bool
	resumeID  string
}

var codexStartRegex = regexp.MustCompile(`codex session: started pid=\d+ cmd=\S+ args=\[([^\]]*)\]`)
var codexResumeRegex = regexp.MustCompile(`\bresume\s+(\S+)`)

func parseCodexStartLines(log string) []codexStartLine {
	matches := codexStartRegex.FindAllStringSubmatch(log, -1)
	out := make([]codexStartLine, 0, len(matches))
	for _, m := range matches {
		sl := codexStartLine{}
		if rm := codexResumeRegex.FindStringSubmatch(m[1]); rm != nil {
			sl.hasResume = true
			sl.resumeID = rm[1]
		}
		out = append(out, sl)
	}
	return out
}
