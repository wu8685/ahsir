package wrapper

import (
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
