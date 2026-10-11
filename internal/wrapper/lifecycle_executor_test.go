package wrapper

import (
	"context"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
)

// lifecycleEmittingSession is a Session that emits one EventLifecycle before
// its reply — the shape the lifecycle controller produces.
type lifecycleEmittingSession struct{}

func (lifecycleEmittingSession) Stream(_ context.Context, _ string) (<-chan Event, error) {
	ch := make(chan Event, 4)
	ch <- EventLifecycle{Event: EvtTurnRunning, TurnID: "turn_x", Phase: PhaseRunning}
	ch <- EventTextDelta{Text: "done"}
	ch <- EventText{Text: "done"}
	ch <- EventTurnDone{}
	close(ch)
	return ch, nil
}
func (s lifecycleEmittingSession) Turn(ctx context.Context, text string) (string, error) {
	ch, err := s.Stream(ctx, text)
	if err != nil {
		return "", err
	}
	_, out, doneErr := drainEvents(ch)
	return out, doneErr
}
func (lifecycleEmittingSession) SessionID() string { return "s" }
func (lifecycleEmittingSession) IsHealthy() bool   { return true }
func (lifecycleEmittingSession) Close() error      { return nil }

// The executor's streaming path must surface lifecycle transitions as
// structured DataParts (ev=lifecycle) so the CMA facade can relay them as
// typed session.lifecycle events (issue #43).
func TestExecutor_ExecuteStream_SurfacesLifecycleEvents(t *testing.T) {
	executor := NewExecutor(ExecutorConfig{
		OpenSession: func(ctx context.Context, contextID string) (Session, error) {
			return lifecycleEmittingSession{}, nil
		},
		ListAgents: func() []*a2a.AgentCard { return nil },
		MaxDepth:   5,
		BasePrompt: "You are a helper.",
	})

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "work"})
	var lifecycleData []map[string]any
	var finalTask *a2a.Task
	for ev, err := range executor.ExecuteStream(context.Background(), msg) {
		if err != nil {
			t.Fatalf("yield err: %v", err)
		}
		switch e := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			if e.Status.Message == nil {
				continue
			}
			for _, p := range e.Status.Message.Parts {
				if dp, ok := p.(a2a.DataPart); ok {
					if dp.Data["ev"] == "lifecycle" {
						lifecycleData = append(lifecycleData, dp.Data)
					}
				}
			}
		case *a2a.Task:
			finalTask = e
		}
	}

	if finalTask == nil || finalTask.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("turn must complete, task=%+v", finalTask)
	}
	if len(lifecycleData) != 1 {
		t.Fatalf("want 1 lifecycle DataPart, got %d", len(lifecycleData))
	}
	d := lifecycleData[0]
	if d["event"] != EvtTurnRunning || d["turn_id"] != "turn_x" || d["phase"] != PhaseRunning {
		t.Fatalf("lifecycle DataPart wrong: %v", d)
	}
}
