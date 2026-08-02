package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	maxLiveEventsPerContext = 256
	maxLiveEventContexts    = 128
)

// LiveEvent is the bounded, provider-neutral progress shape exposed to the UI.
// Hidden reasoning and raw provider envelopes never enter this structure.
type LiveEvent struct {
	ID           string          `json:"id"`
	InvocationID string          `json:"invocationId,omitempty"`
	ContextID    string          `json:"contextId"`
	AgentName    string          `json:"agentName,omitempty"`
	Type         string          `json:"type"`
	At           time.Time       `json:"at"`
	Name         string          `json:"name,omitempty"`
	ToolUseID    string          `json:"toolUseId,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Content      string          `json:"content,omitempty"`
	IsError      bool            `json:"isError,omitempty"`
	State        string          `json:"state,omitempty"`
}

type liveContext struct {
	events      []LiveEvent
	updatedAt   time.Time
	subscribers map[uint64]chan LiveEvent
}

type liveEventHub struct {
	mu       sync.Mutex
	seq      uint64
	subSeq   uint64
	contexts map[string]*liveContext
}

func newLiveEventHub() *liveEventHub {
	return &liveEventHub{contexts: make(map[string]*liveContext)}
}

func (h *liveEventHub) Publish(ev LiveEvent) LiveEvent {
	if ev.ContextID == "" {
		return ev
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.ID = fmt.Sprintf("live-%d", h.seq)
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	c := h.contexts[ev.ContextID]
	if c == nil {
		if len(h.contexts) >= maxLiveEventContexts {
			h.evictOldestLocked()
		}
		c = &liveContext{subscribers: make(map[uint64]chan LiveEvent)}
		h.contexts[ev.ContextID] = c
	}
	c.updatedAt = ev.At
	c.events = append(c.events, ev)
	if len(c.events) > maxLiveEventsPerContext {
		c.events = append([]LiveEvent(nil), c.events[len(c.events)-maxLiveEventsPerContext:]...)
	}
	for _, ch := range c.subscribers {
		select {
		case ch <- ev:
		default:
			// Never let an abandoned browser connection block an agent turn.
		}
	}
	return ev
}

func (h *liveEventHub) evictOldestLocked() {
	var oldest string
	var at time.Time
	for id, c := range h.contexts {
		if oldest == "" || c.updatedAt.Before(at) {
			oldest, at = id, c.updatedAt
		}
	}
	if oldest != "" {
		delete(h.contexts, oldest)
	}
}

func (h *liveEventHub) Snapshot(contextID, after string) []LiveEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return snapshotAfter(h.contexts[contextID], after)
}

func snapshotAfter(c *liveContext, after string) []LiveEvent {
	if c == nil {
		return []LiveEvent{}
	}
	start := 0
	if after != "" {
		for i, ev := range c.events {
			if ev.ID == after {
				start = i + 1
			}
		}
	}
	return append([]LiveEvent(nil), c.events[start:]...)
}

func (h *liveEventHub) Subscribe(contextID, after string) ([]LiveEvent, <-chan LiveEvent, func()) {
	h.mu.Lock()
	c := h.contexts[contextID]
	if c == nil {
		c = &liveContext{subscribers: make(map[uint64]chan LiveEvent), updatedAt: time.Now().UTC()}
		h.contexts[contextID] = c
	}
	backlog := snapshotAfter(c, after)
	h.subSeq++
	id := h.subSeq
	ch := make(chan LiveEvent, 64)
	c.subscribers[id] = ch
	h.mu.Unlock()
	return backlog, ch, func() {
		h.mu.Lock()
		if current := h.contexts[contextID]; current != nil {
			delete(current.subscribers, id)
		}
		h.mu.Unlock()
	}
}

func (g *gatewayHandler) handleContextEvents(w http.ResponseWriter, r *http.Request) {
	contextID := r.URL.Query().Get("contextId")
	if contextID == "" {
		writeJSONError(w, http.StatusBadRequest, "contextId is required")
		return
	}
	writeJSON(w, http.StatusOK, g.sch.liveEvents.Snapshot(contextID, r.URL.Query().Get("after")))
}

func (g *gatewayHandler) handleContextEventsStream(w http.ResponseWriter, r *http.Request) {
	contextID := r.URL.Query().Get("contextId")
	if contextID == "" {
		writeJSONError(w, http.StatusBadRequest, "contextId is required")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	backlog, ch, cancel := g.sch.liveEvents.Subscribe(contextID, r.URL.Query().Get("after"))
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for _, ev := range backlog {
		writeLiveSSE(w, ev)
	}
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			writeLiveSSE(w, ev)
			flusher.Flush()
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

func writeLiveSSE(w http.ResponseWriter, ev LiveEvent) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, b)
}
