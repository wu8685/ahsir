package scheduler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestLiveEventHubSnapshotAfterCursor(t *testing.T) {
	h := newLiveEventHub()
	first := h.Publish(LiveEvent{ContextID: "ctx", Type: "status", State: "running"})
	second := h.Publish(LiveEvent{ContextID: "ctx", Type: "tool_use", Name: "command_execution"})
	h.Publish(LiveEvent{ContextID: "other", Type: "status"})

	got := h.Snapshot("ctx", first.ID)
	if len(got) != 1 || got[0].ID != second.ID {
		t.Fatalf("snapshot after cursor = %+v", got)
	}
}

func TestGatewayA2AProxyPublishesToolEventBeforeTerminal(t *testing.T) {
	release := make(chan struct{})
	toolSent := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message\n")
		_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":"rpc","result":{"kind":"status-update","taskId":"task-1","status":{"state":"working","message":{"role":"agent","parts":[{"kind":"data","data":{"ev":"tool_use","id":"cmd-1","name":"command_execution","input":{"command":"go test ./..."}}}]}}}}`+"\n\n")
		flusher.Flush()
		close(toolSent)
		<-release
		_, _ = io.WriteString(w, "event: message\n")
		_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":"rpc","result":{"kind":"task","id":"task-1","status":{"state":"completed"}}}`+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	sch, gatewayURL := newTestScheduler(t)
	sch.Registry().Register(&a2a.AgentCard{Name: "coder", URL: upstream.URL, PreferredTransport: a2a.TransportProtocolJSONRPC})
	body := []byte(`{"jsonrpc":"2.0","method":"message/stream","id":"rpc","params":{"message":{"messageId":"m1","role":"user","contextId":"ctx-live","parts":[{"kind":"text","text":"build it"}]}}}`)
	done := make(chan error, 1)
	go func() {
		resp, err := http.Post(gatewayURL+"/a2a/coder", "application/json", bytes.NewReader(body))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()

	select {
	case <-toolSent:
	case <-time.After(time.Second):
		t.Fatal("upstream did not send tool event")
	}

	deadline := time.Now().Add(time.Second)
	var events []LiveEvent
	for time.Now().Before(deadline) {
		resp, err := http.Get(gatewayURL + "/context-events?contextId=ctx-live")
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(resp.Body).Decode(&events)
		_ = resp.Body.Close()
		if err == nil && len(events) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(events) != 1 || events[0].Type != "tool_use" || events[0].Name != "command_execution" {
		t.Fatalf("live events before terminal = %+v", events)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
