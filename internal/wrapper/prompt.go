package wrapper

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/a2aproject/a2a-go/a2a"
)

// A2ACall represents a parsed ---A2A_CALL--- block from Claude Code output.
type A2ACall struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// ValidateA2ACall checks that an A2A call has required fields.
func ValidateA2ACall(call A2ACall) error {
	if call.Agent == "" {
		return fmt.Errorf("agent name is required in A2A_CALL")
	}
	if call.Task == "" {
		return fmt.Errorf("task description is required in A2A_CALL")
	}
	return nil
}

// BuildSystemPrompt injects available agents into the system prompt.
func BuildSystemPrompt(basePrompt string, agents []*a2a.AgentCard, maxCalls int) string {
	var sb strings.Builder
	sb.WriteString(basePrompt)

	if len(agents) > 0 {
		sb.WriteString("\n\nYou can call the following agents for help:\n")
		for _, a := range agents {
			skills := make([]string, len(a.Skills))
			for i, s := range a.Skills {
				skills[i] = s.Name
			}
			sb.WriteString(fmt.Sprintf("- name: %q, skills: %v, endpoint: %q\n",
				a.Name, skills, a.URL))
		}
		sb.WriteString("\nWhen you need another agent's help, append to your response:\n")
		sb.WriteString("---A2A_CALL---\n")
		sb.WriteString(`{"agent": "<name>", "task": "<description of what you need>"}` + "\n")
		sb.WriteString("---END---\n")
		sb.WriteString(fmt.Sprintf("\nMax chain depth: %d agent calls.\n", maxCalls))
	}

	return sb.String()
}

// ParseA2ACall extracts an ---A2A_CALL--- block from Claude Code output.
func ParseA2ACall(output string) (A2ACall, bool) {
	start := strings.Index(output, "---A2A_CALL---")
	if start == -1 {
		return A2ACall{}, false
	}

	contentStart := start + len("---A2A_CALL---")
	end := strings.Index(output[contentStart:], "---END---")
	if end == -1 {
		return A2ACall{}, false
	}

	jsonStr := strings.TrimSpace(output[contentStart : contentStart+end])
	var call A2ACall
	if err := json.Unmarshal([]byte(jsonStr), &call); err != nil {
		return A2ACall{}, false
	}

	return call, true
}

// SerializeA2ACall creates the ---A2A_CALL--- text block for a given call.
func SerializeA2ACall(call A2ACall) string {
	data, _ := json.MarshalIndent(call, "", "  ")
	return fmt.Sprintf("---A2A_CALL---\n%s\n---END---\n", string(data))
}

// BuildInjectionPrompt creates a prompt that injects another agent's result,
// including the original task context so the agent knows what to do with the response.
func BuildInjectionPrompt(agentName, originalTask, result string) string {
	return fmt.Sprintf(
		"Your original task was: %s\n\n"+
			"Agent %q returned the following response:\n%s\n\n"+
			"Please use this response to answer the original task. "+
			"If the response is sufficient, relay it to the user directly. "+
			"If not, explain what additional information is needed.",
		originalTask, agentName, result,
	)
}
