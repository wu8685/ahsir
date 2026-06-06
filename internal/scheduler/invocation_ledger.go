package scheduler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	completedInvocationRetention  = 7 * 24 * time.Hour
	incompleteInvocationRetention = 30 * 24 * time.Hour
)

type InvocationSource string

const (
	InvocationSourceChatGateway InvocationSource = "chat_gateway"
	InvocationSourceA2AProxy    InvocationSource = "a2a_proxy"
)

type InvocationStatus string

const (
	InvocationStatusInFlight       InvocationStatus = "in_flight"
	InvocationStatusCompleted      InvocationStatus = "completed"
	InvocationStatusFailed         InvocationStatus = "failed"
	InvocationStatusRecovering     InvocationStatus = "recovering"
	InvocationStatusRecovered      InvocationStatus = "recovered"
	InvocationStatusRecoveryFailed InvocationStatus = "recovery_failed"
)

type InvocationRecord struct {
	ID         string
	Source     InvocationSource
	AgentName  string
	Method     string
	ContextID  string
	MessageID  string
	UserText   string
	Status     InvocationStatus
	StartedAt  time.Time
	FinishedAt time.Time
	Error      string
}

type InvocationMetadata struct {
	Source    InvocationSource
	AgentName string
	Method    string
	ContextID string
	MessageID string
	UserText  string
}

type InvocationLedger struct {
	mu      sync.Mutex
	nextID  uint64
	path    string
	records map[string]InvocationRecord
	order   []string
}

func NewInvocationLedger() *InvocationLedger {
	return &InvocationLedger{
		records: make(map[string]InvocationRecord),
	}
}

func NewInvocationLedgerFromFile(path string) (*InvocationLedger, error) {
	l := NewInvocationLedger()
	l.path = path
	if path == "" {
		return l, nil
	}
	exists := false
	if _, err := os.Stat(path); err == nil {
		exists = true
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := l.replay(); err != nil {
		return nil, err
	}
	if exists {
		if err := l.CompactForRetention(time.Now()); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *InvocationLedger) CompactForRetention(now time.Time) error {
	if l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.compactForRetentionLocked(now)
}

func (l *InvocationLedger) compactForRetentionLocked(now time.Time) error {
	retained := make([]InvocationRecord, 0, len(l.order))
	for _, id := range l.order {
		rec, ok := l.records[id]
		if !ok {
			continue
		}
		if retainInvocationRecord(rec, now) {
			retained = append(retained, rec)
		}
	}

	events := make([]invocationLedgerEvent, 0, len(retained)*2+1)
	events = append(events, invocationLedgerEvent{
		Type:   "checkpoint",
		NextID: l.nextID,
		TS:     now,
	})
	for _, rec := range retained {
		events = append(events, startedEventFromRecord(rec))
		switch rec.Status {
		case InvocationStatusCompleted:
			events = append(events, invocationLedgerEvent{
				Type: "completed",
				ID:   rec.ID,
				TS:   terminalTime(rec),
			})
		case InvocationStatusFailed:
			events = append(events, invocationLedgerEvent{
				Type:  "failed",
				ID:    rec.ID,
				Error: rec.Error,
				TS:    terminalTime(rec),
			})
		case InvocationStatusRecovering:
			events = append(events, invocationLedgerEvent{
				Type: "recovering",
				ID:   rec.ID,
				TS:   terminalTime(rec),
			})
		case InvocationStatusRecovered:
			events = append(events, invocationLedgerEvent{
				Type: "recovered",
				ID:   rec.ID,
				TS:   terminalTime(rec),
			})
		case InvocationStatusRecoveryFailed:
			events = append(events, invocationLedgerEvent{
				Type:  "recovery_failed",
				ID:    rec.ID,
				Error: rec.Error,
				TS:    terminalTime(rec),
			})
		}
	}
	if err := writeLedgerEvents(l.path, events); err != nil {
		return err
	}

	l.records = make(map[string]InvocationRecord, len(retained))
	l.order = l.order[:0]
	for _, rec := range retained {
		l.records[rec.ID] = rec
		l.order = append(l.order, rec.ID)
	}
	return nil
}

func (l *InvocationLedger) Begin(meta InvocationMetadata) InvocationRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	id := fmt.Sprintf("inv-%d", l.nextID)
	rec := InvocationRecord{
		ID:        id,
		Source:    meta.Source,
		AgentName: meta.AgentName,
		Method:    meta.Method,
		ContextID: meta.ContextID,
		MessageID: meta.MessageID,
		UserText:  meta.UserText,
		Status:    InvocationStatusInFlight,
		StartedAt: time.Now(),
	}
	l.records[id] = rec
	l.order = append(l.order, id)
	l.appendEventLocked(invocationLedgerEvent{
		Type:      "started",
		ID:        rec.ID,
		Source:    rec.Source,
		AgentName: rec.AgentName,
		Method:    rec.Method,
		ContextID: rec.ContextID,
		MessageID: rec.MessageID,
		UserText:  rec.UserText,
		TS:        rec.StartedAt,
	})
	return rec
}

func (l *InvocationLedger) Complete(id string) {
	l.finish(id, InvocationStatusCompleted, "")
}

func (l *InvocationLedger) Fail(id string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	l.finish(id, InvocationStatusFailed, msg)
}

func (l *InvocationLedger) FailMessage(id, msg string) {
	l.finish(id, InvocationStatusFailed, msg)
}

func (l *InvocationLedger) Recovering(id string) {
	l.updateStatus(id, InvocationStatusRecovering, "", false)
}

func (l *InvocationLedger) Recovered(id string) {
	l.updateStatus(id, InvocationStatusRecovered, "", true)
}

func (l *InvocationLedger) RecoveryFailed(id string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	l.updateStatus(id, InvocationStatusRecoveryFailed, msg, true)
}

func (l *InvocationLedger) finish(id string, status InvocationStatus, errMsg string) {
	l.updateStatus(id, status, errMsg, true)
}

func (l *InvocationLedger) updateStatus(id string, status InvocationStatus, errMsg string, finish bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.records[id]
	if !ok {
		return
	}
	rec.Status = status
	rec.Error = errMsg
	ts := time.Now()
	if finish {
		rec.FinishedAt = ts
	}
	l.records[id] = rec
	l.appendEventLocked(invocationLedgerEvent{
		Type:  string(status),
		ID:    id,
		Error: errMsg,
		TS:    ts,
	})
}

func (l *InvocationLedger) Snapshot() []InvocationRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]InvocationRecord, 0, len(l.order))
	for _, id := range l.order {
		out = append(out, l.records[id])
	}
	return out
}

func (l *InvocationLedger) RecoverableForAgent(agentName string) []InvocationRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]InvocationRecord, 0)
	for _, id := range l.order {
		rec := l.records[id]
		if rec.AgentName != agentName || rec.ContextID == "" {
			continue
		}
		switch rec.Status {
		case InvocationStatusInFlight, InvocationStatusFailed, InvocationStatusRecoveryFailed:
			out = append(out, rec)
		}
	}
	return out
}

type invocationLedgerEvent struct {
	Type      string           `json:"type"`
	ID        string           `json:"id,omitempty"`
	NextID    uint64           `json:"nextId,omitempty"`
	Source    InvocationSource `json:"source,omitempty"`
	AgentName string           `json:"agentName,omitempty"`
	Method    string           `json:"method,omitempty"`
	ContextID string           `json:"contextId,omitempty"`
	MessageID string           `json:"messageId,omitempty"`
	UserText  string           `json:"userText,omitempty"`
	Error     string           `json:"error,omitempty"`
	TS        time.Time        `json:"ts"`
}

func (l *InvocationLedger) appendEventLocked(event invocationLedgerEvent) {
	if l.path == "" {
		return
	}
	if event.TS.IsZero() {
		event.TS = time.Now()
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(event)
}

func (l *InvocationLedger) replay() error {
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event invocationLedgerEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		l.applyReplayEvent(event)
	}
	return scanner.Err()
}

func retainInvocationRecord(rec InvocationRecord, now time.Time) bool {
	if rec.Status == InvocationStatusCompleted {
		cutoff := now.Add(-completedInvocationRetention)
		ts := terminalTime(rec)
		return !ts.Before(cutoff)
	}
	cutoff := now.Add(-incompleteInvocationRetention)
	ts := rec.StartedAt
	if ts.IsZero() {
		ts = terminalTime(rec)
	}
	return !ts.Before(cutoff)
}

func startedEventFromRecord(rec InvocationRecord) invocationLedgerEvent {
	return invocationLedgerEvent{
		Type:      "started",
		ID:        rec.ID,
		Source:    rec.Source,
		AgentName: rec.AgentName,
		Method:    rec.Method,
		ContextID: rec.ContextID,
		MessageID: rec.MessageID,
		UserText:  rec.UserText,
		TS:        rec.StartedAt,
	}
}

func terminalTime(rec InvocationRecord) time.Time {
	if !rec.FinishedAt.IsZero() {
		return rec.FinishedAt
	}
	if !rec.StartedAt.IsZero() {
		return rec.StartedAt
	}
	return time.Now()
}

func writeLedgerEvents(path string, events []invocationLedgerEvent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ledger-*.jsonl")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	enc := json.NewEncoder(tmp)
	for _, event := range events {
		if event.TS.IsZero() {
			event.TS = time.Now()
		}
		if err := enc.Encode(event); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

func (l *InvocationLedger) applyReplayEvent(event invocationLedgerEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if event.Type == "checkpoint" {
		if event.NextID > l.nextID {
			l.nextID = event.NextID
		}
		return
	}
	l.bumpNextID(event.ID)
	switch event.Type {
	case "started":
		if _, exists := l.records[event.ID]; !exists {
			l.order = append(l.order, event.ID)
		}
		l.records[event.ID] = InvocationRecord{
			ID:        event.ID,
			Source:    event.Source,
			AgentName: event.AgentName,
			Method:    event.Method,
			ContextID: event.ContextID,
			MessageID: event.MessageID,
			UserText:  event.UserText,
			Status:    InvocationStatusInFlight,
			StartedAt: event.TS,
		}
	case "completed":
		rec, ok := l.records[event.ID]
		if !ok {
			return
		}
		rec.Status = InvocationStatusCompleted
		rec.FinishedAt = event.TS
		rec.Error = ""
		l.records[event.ID] = rec
	case "failed":
		rec, ok := l.records[event.ID]
		if !ok {
			return
		}
		rec.Status = InvocationStatusFailed
		rec.FinishedAt = event.TS
		rec.Error = event.Error
		l.records[event.ID] = rec
	case "recovering":
		rec, ok := l.records[event.ID]
		if !ok {
			return
		}
		rec.Status = InvocationStatusRecovering
		rec.Error = event.Error
		l.records[event.ID] = rec
	case "recovered":
		rec, ok := l.records[event.ID]
		if !ok {
			return
		}
		rec.Status = InvocationStatusRecovered
		rec.FinishedAt = event.TS
		rec.Error = ""
		l.records[event.ID] = rec
	case "recovery_failed":
		rec, ok := l.records[event.ID]
		if !ok {
			return
		}
		rec.Status = InvocationStatusRecoveryFailed
		rec.FinishedAt = event.TS
		rec.Error = event.Error
		l.records[event.ID] = rec
	}
}

func (l *InvocationLedger) bumpNextID(id string) {
	n, err := strconv.ParseUint(strings.TrimPrefix(id, "inv-"), 10, 64)
	if err != nil {
		return
	}
	if n > l.nextID {
		l.nextID = n
	}
}

func metadataFromA2AJSON(agentName string, body []byte) InvocationMetadata {
	meta := InvocationMetadata{
		Source:    InvocationSourceA2AProxy,
		AgentName: agentName,
	}
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Message struct {
				MessageID string            `json:"messageId"`
				ContextID string            `json:"contextId"`
				Parts     []json.RawMessage `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return meta
	}
	meta.Method = rpc.Method
	meta.MessageID = rpc.Params.Message.MessageID
	meta.ContextID = rpc.Params.Message.ContextID
	meta.UserText = textFromA2AParts(rpc.Params.Message.Parts)
	return meta
}

func textFromA2AParts(parts []json.RawMessage) string {
	var text string
	for _, raw := range parts {
		var part struct {
			Kind string `json:"kind"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &part); err != nil {
			continue
		}
		if part.Text == "" {
			continue
		}
		if text != "" {
			text += "\n"
		}
		text += part.Text
	}
	return text
}
