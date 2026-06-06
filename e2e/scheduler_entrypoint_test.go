//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
)

func TestSchedulerEntrypoint_E2E(t *testing.T) {
	fix := setupE2E(t)

	reply, err := fix.sendMessageToAgent(
		"student",
		"msg-scheduler-entrypoint",
		"scheduler-entrypoint-conv",
		"Please delegate to the teacher: what is a goroutine? Answer in one sentence.",
	)
	if err != nil {
		t.Fatalf("send through scheduler /a2a/student: %v\n--- scheduler log ---\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(strings.ToLower(reply), "goroutine") {
		t.Fatalf("reply does not mention goroutine: %q", reply)
	}

	for _, marker := range []string{
		"[student] receive",
		"[student \u2192 teacher]",
		"[teacher] receive",
		"[student \u2190 teacher] reply",
	} {
		if !strings.Contains(fix.schedulerLog(), marker) {
			t.Fatalf("scheduler entrypoint missing marker %q\n--- scheduler log ---\n%s", marker, fix.schedulerLog())
		}
	}

	rawLedger, err := os.ReadFile(fix.ledgerPath())
	if err != nil {
		t.Fatalf("read scheduler ledger: %v", err)
	}
	ledger := string(rawLedger)
	for _, marker := range []string{
		`"source":"a2a_proxy"`,
		`"agentName":"student"`,
		`"contextId":"scheduler-entrypoint-conv"`,
		`"messageId":"msg-scheduler-entrypoint"`,
		`"type":"completed"`,
	} {
		if !strings.Contains(ledger, marker) {
			t.Fatalf("ledger missing marker %q\n--- ledger ---\n%s", marker, ledger)
		}
	}
}

func TestDirectAgentPortRequiresInternalToken_E2E(t *testing.T) {
	fix := setupE2E(t)

	_, err := fix.sendDirectMessage(
		fix.studentPort,
		"msg-direct-rejected",
		"direct-rejected-conv",
		"This should not bypass the scheduler.",
	)
	if err == nil {
		t.Fatalf("direct agent port request without internal token unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("direct agent port error = %v, want 401 unauthorized", err)
	}
}
