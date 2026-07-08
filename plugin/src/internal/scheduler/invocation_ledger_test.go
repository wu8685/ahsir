package scheduler

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInvocationLedgerLifecycle(t *testing.T) {
	ledger := NewInvocationLedger()
	rec := ledger.Begin(InvocationMetadata{
		Source:    InvocationSourceChatGateway,
		AgentName: "teacher",
		Method:    "message/send",
		ContextID: "ctx-1",
		UserText:  "hello",
	})
	if rec.Status != InvocationStatusInFlight {
		t.Fatalf("status = %q", rec.Status)
	}

	ledger.Complete(rec.ID)
	snapshot := ledger.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot len = %d", len(snapshot))
	}
	if snapshot[0].Status != InvocationStatusCompleted {
		t.Fatalf("status = %q", snapshot[0].Status)
	}
	if snapshot[0].FinishedAt.IsZero() {
		t.Fatal("expected FinishedAt to be set")
	}
}

func TestInvocationLedgerRecordsFailure(t *testing.T) {
	ledger := NewInvocationLedger()
	rec := ledger.Begin(InvocationMetadata{AgentName: "teacher"})
	ledger.Fail(rec.ID, errors.New("boom"))

	snapshot := ledger.Snapshot()
	if snapshot[0].Status != InvocationStatusFailed {
		t.Fatalf("status = %q", snapshot[0].Status)
	}
	if snapshot[0].Error != "boom" {
		t.Fatalf("error = %q", snapshot[0].Error)
	}
}

func TestInvocationLedgerRecordsRecoveryLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	ledger, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}

	recovered := ledger.Begin(InvocationMetadata{AgentName: "teacher", ContextID: "ctx-recovered"})
	ledger.Recovering(recovered.ID)
	ledger.Recovered(recovered.ID)

	failed := ledger.Begin(InvocationMetadata{AgentName: "teacher", ContextID: "ctx-recovery-failed"})
	ledger.Recovering(failed.ID)
	ledger.RecoveryFailed(failed.ID, errors.New("resume failed"))

	replayed, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := replayed.Snapshot()
	assertLedgerStatus(t, snapshot, recovered.ID, InvocationStatusRecovered, "")
	assertLedgerStatus(t, snapshot, failed.ID, InvocationStatusRecoveryFailed, "resume failed")
}

func TestMetadataFromA2AJSON(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-1","contextId":"ctx-1","role":"user","parts":[{"kind":"text","text":"first"},{"kind":"text","text":"second"}]}},"id":1}`)
	meta := metadataFromA2AJSON("teacher", body)

	if meta.Source != InvocationSourceA2AProxy {
		t.Fatalf("source = %q", meta.Source)
	}
	if meta.AgentName != "teacher" {
		t.Fatalf("agent = %q", meta.AgentName)
	}
	if meta.Method != "message/send" {
		t.Fatalf("method = %q", meta.Method)
	}
	if meta.ContextID != "ctx-1" {
		t.Fatalf("contextID = %q", meta.ContextID)
	}
	if meta.MessageID != "msg-1" {
		t.Fatalf("messageID = %q", meta.MessageID)
	}
	if meta.UserText != "first\nsecond" {
		t.Fatalf("userText = %q", meta.UserText)
	}
}

func TestInvocationLedgerPersistsJSONLEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ahsir", "ledger.jsonl")
	ledger, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}

	rec := ledger.Begin(InvocationMetadata{
		Source:    InvocationSourceA2AProxy,
		AgentName: "teacher",
		Method:    "message/send",
		ContextID: "ctx-jsonl",
		MessageID: "msg-jsonl",
		UserText:  "persist me",
	})
	ledger.Complete(rec.ID)
	failed := ledger.Begin(InvocationMetadata{Source: InvocationSourceChatGateway, AgentName: "student"})
	ledger.Fail(failed.ID, errors.New("network down"))

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 JSONL events, got %d: %s", len(lines), data)
	}
	for _, want := range []string{`"type":"started"`, `"type":"completed"`, `"type":"failed"`, `"contextId":"ctx-jsonl"`, `"error":"network down"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("ledger file missing %s: %s", want, data)
		}
	}
}

func TestInvocationLedgerReplaysJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	original, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	completed := original.Begin(InvocationMetadata{Source: InvocationSourceA2AProxy, AgentName: "teacher", ContextID: "ctx-done"})
	original.Complete(completed.ID)
	failed := original.Begin(InvocationMetadata{Source: InvocationSourceChatGateway, AgentName: "student", ContextID: "ctx-fail"})
	original.Fail(failed.ID, errors.New("boom"))
	inFlight := original.Begin(InvocationMetadata{Source: InvocationSourceA2AProxy, AgentName: "reviewer", ContextID: "ctx-open"})

	replayed, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := replayed.Snapshot()
	if len(snapshot) != 3 {
		t.Fatalf("snapshot len = %d, want 3: %+v", len(snapshot), snapshot)
	}
	assertLedgerStatus(t, snapshot, completed.ID, InvocationStatusCompleted, "")
	assertLedgerStatus(t, snapshot, failed.ID, InvocationStatusFailed, "boom")
	assertLedgerStatus(t, snapshot, inFlight.ID, InvocationStatusInFlight, "")

	next := replayed.Begin(InvocationMetadata{AgentName: "next"})
	if next.ID == completed.ID || next.ID == failed.ID || next.ID == inFlight.ID {
		t.Fatalf("replayed ledger reused ID %q", next.ID)
	}
}

func TestInvocationLedgerReplayIgnoresBadJSONLLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	// Use recent, relative timestamps so the completed record survives
	// retention compaction on replay — a hardcoded absolute date would age
	// past completedInvocationRetention and be dropped, making this test a
	// time bomb that fails once the clock moves past the retention window.
	started := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	completed := time.Now().UTC().Format(time.RFC3339)
	content := strings.Join([]string{
		`{"type":"started","id":"inv-1","source":"a2a_proxy","agentName":"teacher","contextId":"ctx-1","ts":"` + started + `"}`,
		`{bad json`,
		`{"type":"completed","id":"inv-1","ts":"` + completed + `"}`,
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	ledger, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ledger.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot len = %d", len(snapshot))
	}
	if snapshot[0].Status != InvocationStatusCompleted {
		t.Fatalf("status = %q", snapshot[0].Status)
	}
}

func TestInvocationLedgerCompactsExpiredRecordsOnReplay(t *testing.T) {
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	events := []invocationLedgerEvent{
		startedEvent("inv-1", "old-completed", now.Add(-8*24*time.Hour)),
		completedEvent("inv-1", now.Add(-8*24*time.Hour)),
		startedEvent("inv-2", "new-completed", now.Add(-6*24*time.Hour)),
		completedEvent("inv-2", now.Add(-6*24*time.Hour)),
		startedEvent("inv-3", "old-in-flight", now.Add(-31*24*time.Hour)),
		startedEvent("inv-4", "new-in-flight", now.Add(-29*24*time.Hour)),
		startedEvent("inv-5", "old-failed", now.Add(-31*24*time.Hour)),
		failedEvent("inv-5", "old failure", now.Add(-31*24*time.Hour)),
		startedEvent("inv-6", "new-failed", now.Add(-29*24*time.Hour)),
		failedEvent("inv-6", "new failure", now.Add(-29*24*time.Hour)),
	}
	writeLedgerEventsForTest(t, path, events)

	ledger, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}

	snapshot := ledger.Snapshot()
	if len(snapshot) != 3 {
		t.Fatalf("snapshot len = %d, want 3: %+v", len(snapshot), snapshot)
	}
	assertLedgerContextPresent(t, snapshot, "new-completed")
	assertLedgerContextPresent(t, snapshot, "new-in-flight")
	assertLedgerContextPresent(t, snapshot, "new-failed")
	assertLedgerContextAbsent(t, snapshot, "old-completed")
	assertLedgerContextAbsent(t, snapshot, "old-in-flight")
	assertLedgerContextAbsent(t, snapshot, "old-failed")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"old-completed", "old-in-flight", "old-failed"} {
		if strings.Contains(string(data), gone) {
			t.Fatalf("compacted ledger still contains %q: %s", gone, data)
		}
	}
	for _, kept := range []string{"new-completed", "new-in-flight", "new-failed", `"type":"checkpoint"`} {
		if !strings.Contains(string(data), kept) {
			t.Fatalf("compacted ledger missing %q: %s", kept, data)
		}
	}

	next := ledger.Begin(InvocationMetadata{AgentName: "next"})
	if next.ID == "inv-1" || next.ID == "inv-2" || next.ID == "inv-3" || next.ID == "inv-4" || next.ID == "inv-5" || next.ID == "inv-6" {
		t.Fatalf("next ID reused compacted history ID %q", next.ID)
	}
}

func TestSchedulerNewReplaysInvocationLedgerFromConfigPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "ahsir.yaml")
	if err := os.WriteFile(configPath, []byte("agents: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(dir, ".ahsir", "ledger.jsonl")
	seed, err := NewInvocationLedgerFromFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	rec := seed.Begin(InvocationMetadata{
		Source:    InvocationSourceA2AProxy,
		AgentName: "teacher",
		ContextID: "ctx-scheduler-replay",
	})

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	sch := New(cfg)
	snapshot := sch.Invocations().Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot len = %d", len(snapshot))
	}
	if snapshot[0].ID != rec.ID || snapshot[0].ContextID != "ctx-scheduler-replay" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func startedEvent(id, contextID string, ts time.Time) invocationLedgerEvent {
	return invocationLedgerEvent{
		Type:      "started",
		ID:        id,
		Source:    InvocationSourceA2AProxy,
		AgentName: "teacher",
		Method:    "message/send",
		ContextID: contextID,
		MessageID: "msg-" + contextID,
		UserText:  contextID,
		TS:        ts,
	}
}

func completedEvent(id string, ts time.Time) invocationLedgerEvent {
	return invocationLedgerEvent{Type: "completed", ID: id, TS: ts}
}

func failedEvent(id, errMsg string, ts time.Time) invocationLedgerEvent {
	return invocationLedgerEvent{Type: "failed", ID: id, Error: errMsg, TS: ts}
}

func writeLedgerEventsForTest(t *testing.T, path string, events []invocationLedgerEvent) {
	t.Helper()
	var b strings.Builder
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertLedgerContextPresent(t *testing.T, snapshot []InvocationRecord, contextID string) {
	t.Helper()
	for _, rec := range snapshot {
		if rec.ContextID == contextID {
			return
		}
	}
	t.Fatalf("context %q not found in snapshot %+v", contextID, snapshot)
}

func assertLedgerContextAbsent(t *testing.T, snapshot []InvocationRecord, contextID string) {
	t.Helper()
	for _, rec := range snapshot {
		if rec.ContextID == contextID {
			t.Fatalf("context %q unexpectedly found in snapshot %+v", contextID, snapshot)
		}
	}
}

func assertLedgerStatus(t *testing.T, snapshot []InvocationRecord, id string, status InvocationStatus, errSub string) {
	t.Helper()
	for _, rec := range snapshot {
		if rec.ID != id {
			continue
		}
		if rec.Status != status {
			t.Fatalf("%s status = %q, want %q", id, rec.Status, status)
		}
		if errSub != "" && !strings.Contains(rec.Error, errSub) {
			t.Fatalf("%s error = %q, want substring %q", id, rec.Error, errSub)
		}
		return
	}
	t.Fatalf("record %s not found in snapshot %+v", id, snapshot)
}

// --- speaker attribution (specs/2026-06-08-shared-context-collaboration.md) ---

// TestInvocationLedgerPersistsSpeaker pins that the self-claimed speaker
// travels into the record AND survives the jsonl replay path — trace must be
// able to show who said what after a scheduler restart.
func TestInvocationLedgerPersistsSpeaker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	ledger, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}

	rec := ledger.Begin(InvocationMetadata{
		Source:    InvocationSourceChatGateway,
		AgentName: "teacher",
		ContextID: "ctx-speaker",
		UserText:  "hello",
		Speaker:   "alice",
	})
	if rec.Speaker != "alice" {
		t.Fatalf("record speaker = %q, want alice", rec.Speaker)
	}
	ledger.Complete(rec.ID)

	replayed, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := replayed.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot len = %d", len(snapshot))
	}
	if snapshot[0].Speaker != "alice" {
		t.Fatalf("replayed speaker = %q, want alice", snapshot[0].Speaker)
	}
}

// TestMetadataFromA2AJSONExtractsSpeaker pins that native A2A proxy calls get
// attribution too: the speaker rides Message.Metadata["speaker"].
func TestMetadataFromA2AJSONExtractsSpeaker(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-1","contextId":"ctx-1","role":"user","metadata":{"speaker":"bob"},"parts":[{"kind":"text","text":"hi"}]}},"id":1}`)
	meta := metadataFromA2AJSON("teacher", body)
	if meta.Speaker != "bob" {
		t.Fatalf("speaker = %q, want bob", meta.Speaker)
	}

	// Absent metadata key → empty speaker, not a parse failure.
	body = []byte(`{"jsonrpc":"2.0","method":"message/send","params":{"message":{"messageId":"msg-2","contextId":"ctx-1","role":"user","parts":[{"kind":"text","text":"hi"}]}},"id":1}`)
	meta = metadataFromA2AJSON("teacher", body)
	if meta.Speaker != "" {
		t.Fatalf("speaker = %q, want empty", meta.Speaker)
	}
}

// --- queued state (specs/2026-06-08-shared-context-collaboration.md) ---

// TestInvocationLedgerQueuedLifecycle pins the async-chat timeline:
// started → queued → terminal, surviving file replay at each stage.
func TestInvocationLedgerQueuedLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	ledger, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}

	rec := ledger.Begin(InvocationMetadata{AgentName: "teacher", ContextID: "ctx-q", Speaker: "alice"})
	ledger.Queued(rec.ID)

	snapshot := ledger.Snapshot()
	if snapshot[0].Status != InvocationStatusQueued {
		t.Fatalf("status after Queued = %q, want queued", snapshot[0].Status)
	}
	if !snapshot[0].FinishedAt.IsZero() {
		t.Fatal("queued is not a terminal state — FinishedAt must stay zero")
	}

	// Replay mid-flight: a queued invocation must come back queued.
	replayed, err := NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := replayed.Snapshot()[0].Status; got != InvocationStatusQueued {
		t.Fatalf("replayed mid-flight status = %q, want queued", got)
	}

	ledger.Complete(rec.ID)
	replayed, err = NewInvocationLedgerFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := replayed.Snapshot()[0].Status; got != InvocationStatusCompleted {
		t.Fatalf("replayed terminal status = %q, want completed", got)
	}
}
