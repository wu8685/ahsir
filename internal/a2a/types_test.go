package a2a

import (
	"encoding/json"
	"testing"
)

func TestTaskStateConstants(t *testing.T) {
	tests := []struct {
		state TaskState
		str   string
	}{
		{TaskStateSubmitted, "TASK_STATE_SUBMITTED"},
		{TaskStateWorking, "TASK_STATE_WORKING"},
		{TaskStateInputRequired, "TASK_STATE_INPUT_REQUIRED"},
		{TaskStateCompleted, "TASK_STATE_COMPLETED"},
		{TaskStateFailed, "TASK_STATE_FAILED"},
		{TaskStateCanceled, "TASK_STATE_CANCELED"},
	}
	for _, tt := range tests {
		if string(tt.state) != tt.str {
			t.Errorf("expected %s, got %s", tt.str, tt.state)
		}
	}
}

func TestRoleConstants(t *testing.T) {
	if string(RoleUser) != "user" {
		t.Errorf("expected user, got %s", RoleUser)
	}
	if string(RoleAgent) != "agent" {
		t.Errorf("expected agent, got %s", RoleAgent)
	}
}

func TestPartTypeConstants(t *testing.T) {
	if string(PartTypeText) != "text" {
		t.Errorf("expected text, got %s", PartTypeText)
	}
	if string(PartTypeFile) != "file" {
		t.Errorf("expected file, got %s", PartTypeFile)
	}
	if string(PartTypeData) != "data" {
		t.Errorf("expected data, got %s", PartTypeData)
	}
}

func TestTextPartMarshalJSON(t *testing.T) {
	p := TextPart{Text: "hello"}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"type":"text","text":"hello"}`
	if string(data) != expected {
		t.Errorf("expected %s, got %s", expected, string(data))
	}
}

func TestTextPartUnmarshalJSON(t *testing.T) {
	data := []byte(`{"type":"text","text":"hello"}`)
	var p TextPart
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Text != "hello" {
		t.Errorf("expected hello, got %s", p.Text)
	}
}

func TestFilePartMarshalJSON(t *testing.T) {
	p := FilePart{Name: "main.go", MediaType: "text/x-go", URI: "file:///tmp/main.go"}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var result FilePart
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "main.go" || result.MediaType != "text/x-go" {
		t.Errorf("unexpected unmarshaled FilePart: %+v", result)
	}
}

func TestMessageMarshalJSON(t *testing.T) {
	msg := Message{
		Role:  RoleUser,
		Parts: []Part{TextPart{Text: "design a user API"}},
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var result Message
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Role != RoleUser {
		t.Errorf("expected user role, got %s", result.Role)
	}
	if len(result.Parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(result.Parts))
	}
}

func TestTaskMarshalJSON(t *testing.T) {
	task := Task{
		ID:     "task-1",
		Status: TaskStateWorking,
		Message: Message{
			Role:  RoleUser,
			Parts: []Part{TextPart{Text: "do something"}},
		},
	}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var result Task
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != "task-1" || result.Status != TaskStateWorking {
		t.Errorf("unexpected unmarshaled Task: %+v", result)
	}
}

func TestAgentCardMarshalJSON(t *testing.T) {
	card := AgentCard{
		Name:        "Backend Agent",
		Description: "Go backend development",
		Version:     "1.0.0",
		Provider:    &AgentProvider{Name: "ahsir", URL: "https://github.com/wu8685/ahsir"},
		Skills: []AgentSkill{
			{Name: "api-design", Description: "Design RESTful APIs"},
		},
		Endpoint: "http://127.0.0.1:9801/",
	}
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	var result AgentCard
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Name != "Backend Agent" {
		t.Errorf("expected Backend Agent, got %s", result.Name)
	}
	if result.Provider == nil || result.Provider.Name != "ahsir" {
		t.Errorf("unexpected provider: %+v", result.Provider)
	}
	if len(result.Skills) != 1 || result.Skills[0].Name != "api-design" {
		t.Errorf("unexpected skills: %+v", result.Skills)
	}
}

func TestPartUnmarshalJSON_DiscriminatesType(t *testing.T) {
	data := []byte(`{"type":"text","text":"hello"}`)
	part, err := UnmarshalPart(data)
	if err != nil {
		t.Fatal(err)
	}
	tp, ok := part.(TextPart)
	if !ok {
		t.Fatalf("expected TextPart, got %T", part)
	}
	if tp.Text != "hello" {
		t.Errorf("expected hello, got %s", tp.Text)
	}
}

func TestPartUnmarshalJSON_UnknownType(t *testing.T) {
	data := []byte(`{"type":"unknown","value":"x"}`)
	_, err := UnmarshalPart(data)
	if err == nil {
		t.Error("expected error for unknown part type")
	}
}
