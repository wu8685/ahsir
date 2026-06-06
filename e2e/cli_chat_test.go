//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIChat_E2E(t *testing.T) {
	fix := setupE2E(t)

	ahsirBin := filepath.Join(fix.repoRoot, "bin", "ahsir")
	schedulerURL := fmt.Sprintf("http://127.0.0.1:%d", fix.registryPort)
	contextID := "cli-chat-conv"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, ahsirBin, "chat",
		"--scheduler", schedulerURL,
		"--context", contextID,
		"teacher",
		"In one short sentence, what is a goroutine?",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ahsir chat failed: %v\noutput:\n%s\n--- scheduler log ---\n%s", err, string(out), fix.schedulerLog())
	}
	output := strings.TrimSpace(string(out))
	if output == "" {
		t.Fatal("chat output was empty")
	}
	if !strings.Contains(strings.ToLower(output), "goroutine") {
		t.Fatalf("chat output does not mention goroutine: %q", output)
	}

	rawLedger, err := os.ReadFile(fix.ledgerPath())
	if err != nil {
		t.Fatalf("read scheduler ledger: %v", err)
	}
	ledger := string(rawLedger)
	for _, marker := range []string{
		`"source":"chat_gateway"`,
		`"agentName":"teacher"`,
		`"contextId":"cli-chat-conv"`,
		`"userText":"In one short sentence, what is a goroutine?"`,
		`"type":"completed"`,
	} {
		if !strings.Contains(ledger, marker) {
			t.Fatalf("ledger missing marker %q\n--- ledger ---\n%s", marker, ledger)
		}
	}
}
