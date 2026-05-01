package wrapper

import (
	"strings"
	"testing"

	"github.com/wu8685/ahsir/internal/a2a"
)

func TestBuildSystemPrompt(t *testing.T) {
	agents := []a2a.AgentCard{
		{Name: "backend", Skills: []a2a.AgentSkill{{Name: "api-design"}}, Endpoint: "http://127.0.0.1:9801/"},
		{Name: "data", Skills: []a2a.AgentSkill{{Name: "sql"}}, Endpoint: "http://127.0.0.1:9802/"},
	}
	basePrompt := "You are a Go developer."

	prompt := BuildSystemPrompt(basePrompt, agents, 5)
	if !strings.Contains(prompt, "backend") {
		t.Error("expected prompt to mention backend agent")
	}
	if !strings.Contains(prompt, "data") {
		t.Error("expected prompt to mention data agent")
	}
	if !strings.Contains(prompt, "---A2A_CALL---") {
		t.Error("expected prompt to contain A2A_CALL format instructions")
	}
	if !strings.Contains(prompt, "api-design") {
		t.Error("expected prompt to mention api-design skill")
	}
}

func TestBuildSystemPromptNoAgents(t *testing.T) {
	prompt := BuildSystemPrompt("You are a Go developer.", nil, 5)
	if !strings.Contains(prompt, "You are a Go developer.") {
		t.Error("expected base prompt to be included")
	}
}

func TestParseA2ACall(t *testing.T) {
	output := `Some text before
---A2A_CALL---
{"agent": "backend", "task": "design a user API"}
---END---
Some text after`

	call, ok := ParseA2ACall(output)
	if !ok {
		t.Fatal("expected to parse A2A_CALL")
	}
	if call.Agent != "backend" {
		t.Errorf("expected backend, got %s", call.Agent)
	}
	if call.Task != "design a user API" {
		t.Errorf("unexpected task: %s", call.Task)
	}
}

func TestParseA2ACallNoCall(t *testing.T) {
	output := "Just normal output, no A2A call here."
	_, ok := ParseA2ACall(output)
	if ok {
		t.Error("expected no A2A_CALL found")
	}
}

func TestParseA2ACallInvalidJSON(t *testing.T) {
	output := "---A2A_CALL---\n{invalid json\n---END---"
	_, ok := ParseA2ACall(output)
	if ok {
		t.Error("expected parse failure for invalid JSON")
	}
}

func TestSerializeA2ACall(t *testing.T) {
	call := A2ACall{Agent: "data", Task: "run a SQL query"}
	serialized := SerializeA2ACall(call)
	if !strings.Contains(serialized, "---A2A_CALL---") {
		t.Error("expected ---A2A_CALL--- marker")
	}
	if !strings.Contains(serialized, "---END---") {
		t.Error("expected ---END--- marker")
	}
	if !strings.Contains(serialized, "data") {
		t.Error("expected agent name in output")
	}
}

func TestBuildInjectionPrompt(t *testing.T) {
	result := BuildInjectionPrompt("data", "the query result")
	if !strings.Contains(result, "data") {
		t.Error("expected agent name in injection prompt")
	}
	if !strings.Contains(result, "the query result") {
		t.Error("expected result in injection prompt")
	}
}

func TestValidateA2ACall(t *testing.T) {
	tests := []struct {
		call    A2ACall
		wantErr bool
	}{
		{A2ACall{Agent: "backend", Task: "do something"}, false},
		{A2ACall{Agent: "", Task: "do something"}, true},
		{A2ACall{Agent: "backend", Task: ""}, true},
		{A2ACall{Agent: "", Task: ""}, true},
	}
	for _, tt := range tests {
		err := ValidateA2ACall(tt.call)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateA2ACall(%+v) error = %v, wantErr = %v", tt.call, err, tt.wantErr)
		}
	}
}
