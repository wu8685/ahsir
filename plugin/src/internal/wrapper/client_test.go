package wrapper

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
)

func TestMessageToString(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "hello world"})
	result := messageToString(msg)
	if result != "hello world" {
		t.Errorf("expected 'hello world', got '%s'", result)
	}
}

func TestMessageToStringEmpty(t *testing.T) {
	msg := a2a.NewMessage(a2a.MessageRoleAgent)
	result := messageToString(msg)
	if result != "" {
		t.Errorf("expected empty string, got '%s'", result)
	}
}

func TestTaskToString(t *testing.T) {
	task := &a2a.Task{
		Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		History: []*a2a.Message{
			a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "do something"}),
			a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "here is the result"}),
		},
	}
	result := taskToString(task)
	if result != "here is the result" {
		t.Errorf("expected 'here is the result', got '%s'", result)
	}
}

func TestTaskToStringNoMessages(t *testing.T) {
	task := &a2a.Task{
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}
	result := taskToString(task)
	if result != string(a2a.TaskStateCompleted) {
		t.Errorf("expected 'completed', got '%s'", result)
	}
}

// TestNoFieldTimeoutHTTPClient is the regression test for a bug where the
// A2A SDK's default 3-minute http.Client.Timeout field would silently kill
// long-running LLM round-trips even when the caller's context allowed more
// time. We pin the shared http.Client to Timeout=0 so the context is the
// only deadline source. If anyone "fixes" the var by adding a timeout
// back, this test must fail loudly.
func TestNoFieldTimeoutHTTPClient(t *testing.T) {
	if noFieldTimeoutHTTPClient.Timeout != 0 {
		t.Errorf("noFieldTimeoutHTTPClient must keep Timeout=0 so context drives deadlines; got %v", noFieldTimeoutHTTPClient.Timeout)
	}
}

// --- speaker attribution (specs/2026-06-08-shared-context-collaboration.md) ---

// speakerCaptureA2AServer is a minimal JSON-RPC endpoint that records the
// incoming message's metadata and contextId, replying with a fixed message.
func speakerCaptureA2AServer(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Params struct {
				Message struct {
					Metadata map[string]any `json:"metadata"`
				} `json:"message"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &rpc)
		*captured = rpc.Params.Message.Metadata

		reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "ok"})
		result, _ := json.Marshal(reply)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":` + string(result) + `,"id":"t"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func speakerTestCard(url string) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name:               "speaker-test",
		Version:            "1.0.0",
		URL:                url,
		PreferredTransport: a2a.TransportProtocolJSONRPC,
	}
}

func TestSendMessageWithSpeakerSetsMetadata(t *testing.T) {
	var captured map[string]any
	srv := speakerCaptureA2AServer(t, &captured)

	client, err := NewAgentClient(context.Background(), speakerTestCard(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessageWithSpeaker(context.Background(), "ctx-1", "alice", "hello"); err != nil {
		t.Fatal(err)
	}
	if got, _ := captured[MetadataSpeakerKey].(string); got != "alice" {
		t.Fatalf("metadata speaker = %q, want alice (metadata=%v)", got, captured)
	}
}

func TestSendMessageWithRequirementsSetsFilesystemMetadata(t *testing.T) {
	var captured map[string]any
	srv := speakerCaptureA2AServer(t, &captured)

	client, err := NewAgentClient(context.Background(), speakerTestCard(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessageWithRequirements(context.Background(), "ctx-1", "alice", "review", []string{"/repo/a", "/repo/b"}); err != nil {
		t.Fatal(err)
	}
	got, ok := captured[MetadataRequiredFilesystemPathsKey].([]any)
	if !ok || len(got) != 2 || got[0] != "/repo/a" || got[1] != "/repo/b" {
		t.Fatalf("required filesystem metadata = %#v", captured[MetadataRequiredFilesystemPathsKey])
	}
}

// TestSendMessageWithoutSpeakerOmitsMetadata pins backward compatibility:
// plain SendMessage must not grow a metadata key — absent speaker means the
// wire shape is identical to today's.
func TestSendMessageWithoutSpeakerOmitsMetadata(t *testing.T) {
	var captured map[string]any
	srv := speakerCaptureA2AServer(t, &captured)

	client, err := NewAgentClient(context.Background(), speakerTestCard(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessage(context.Background(), "ctx-1", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, present := captured[MetadataSpeakerKey]; present {
		t.Fatalf("plain SendMessage must not set speaker metadata, got %v", captured)
	}
	if _, present := captured[MetadataRequiredFilesystemPathsKey]; present {
		t.Fatalf("plain SendMessage must not set filesystem metadata, got %v", captured)
	}
}
