//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// TestStudentDelegatesToTeacher_E2E validates the headline multi-agent
// path with a real LLM: a curl-equivalent to the student must produce
// an A2A_CALL to the teacher, the teacher must answer, and the student
// must relay that answer back. Asserts both the response content AND
// the scheduler log markers — content alone isn't enough because the
// student model might happen to know the answer and bypass the teacher.
func TestStudentDelegatesToTeacher_E2E(t *testing.T) {
	fix := setupE2E(t)

	reply, err := fix.sendMessageToAgent(
		"student",
		"msg-e2e-1",
		"e2e-conv-1",
		"What is a goroutine? Answer in one sentence.",
	)
	if err != nil {
		t.Fatalf("sendMessage to student: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	t.Logf("student reply: %q", reply)

	if !strings.Contains(strings.ToLower(reply), "goroutine") {
		t.Errorf("reply doesn't mention 'goroutine': %q", reply)
	}

	// Verify the delegation actually fired. Without these assertions, a
	// student that ignores the A2A_CALL instruction and answers directly
	// would still produce a passing reply — but the multi-agent path
	// wasn't really exercised.
	logs := fix.schedulerLog()
	for _, marker := range []string{
		"[student] receive",         // request reached student
		"[student → teacher]",       // student dispatched
		"[teacher] receive",         // teacher actually invoked
		"[student ← teacher] reply", // teacher response relayed
	} {
		if !strings.Contains(logs, marker) {
			t.Errorf("scheduler log missing marker %q — multi-agent path not exercised\n--- log ---\n%s", marker, logs)
		}
	}
}
