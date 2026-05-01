package a2a

import (
	"encoding/json"
	"fmt"
)

// TaskState represents the state of an A2A task.
type TaskState string

const (
	TaskStateSubmitted      TaskState = "TASK_STATE_SUBMITTED"
	TaskStateWorking        TaskState = "TASK_STATE_WORKING"
	TaskStateInputRequired  TaskState = "TASK_STATE_INPUT_REQUIRED"
	TaskStateCompleted      TaskState = "TASK_STATE_COMPLETED"
	TaskStateFailed         TaskState = "TASK_STATE_FAILED"
	TaskStateCanceled       TaskState = "TASK_STATE_CANCELED"
	TaskStateAuthRequired   TaskState = "TASK_STATE_AUTH_REQUIRED"
)

// Role represents the role of a message sender.
type Role string

const (
	RoleUser  Role = "user"
	RoleAgent Role = "agent"
)

// PartType identifies the type of a Part.
type PartType string

const (
	PartTypeText PartType = "text"
	PartTypeFile PartType = "file"
	PartTypeData PartType = "data"
)

// Part is an interface for message parts.
type Part interface {
	partMarker()
}

// TextPart represents a text content part.
type TextPart struct {
	Type PartType `json:"type"`
	Text string   `json:"text"`
}

func (TextPart) partMarker() {}

// MarshalJSON ensures the type field is always set for TextPart.
func (tp TextPart) MarshalJSON() ([]byte, error) {
	type alias TextPart
	return json.Marshal(struct {
		Type PartType `json:"type"`
		alias
	}{
		Type:  PartTypeText,
		alias: alias(tp),
	})
}

// FilePart represents a file content part.
type FilePart struct {
	Type      PartType `json:"type"`
	Name      string   `json:"name"`
	MediaType string   `json:"mediaType,omitempty"`
	URI       string   `json:"uri,omitempty"`
	Content   string   `json:"content,omitempty"`
}

func (FilePart) partMarker() {}

// MarshalJSON ensures the type field is always set for FilePart.
func (fp FilePart) MarshalJSON() ([]byte, error) {
	type alias FilePart
	return json.Marshal(struct {
		Type PartType `json:"type"`
		alias
	}{
		Type:  PartTypeFile,
		alias: alias(fp),
	})
}

// DataPart represents a structured data part.
type DataPart struct {
	Type     PartType        `json:"type"`
	Name     string          `json:"name,omitempty"`
	MimeType string          `json:"mimeType,omitempty"`
	Data     json.RawMessage `json:"data"`
}

func (DataPart) partMarker() {}

// MarshalJSON ensures the type field is always set for DataPart.
func (dp DataPart) MarshalJSON() ([]byte, error) {
	type alias DataPart
	return json.Marshal(struct {
		Type PartType `json:"type"`
		alias
	}{
		Type:  PartTypeData,
		alias: alias(dp),
	})
}

// partAlias is used to avoid infinite recursion during unmarshaling.
type partAlias struct {
	Type PartType `json:"type"`
}

// UnmarshalPart unmarshals a Part from JSON, dispatching on the type field.
func UnmarshalPart(data []byte) (Part, error) {
	var alias partAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return nil, fmt.Errorf("unmarshal part type: %w", err)
	}
	switch alias.Type {
	case PartTypeText:
		var tp TextPart
		if err := json.Unmarshal(data, &tp); err != nil {
			return nil, err
		}
		return tp, nil
	case PartTypeFile:
		var fp FilePart
		if err := json.Unmarshal(data, &fp); err != nil {
			return nil, err
		}
		return fp, nil
	case PartTypeData:
		var dp DataPart
		if err := json.Unmarshal(data, &dp); err != nil {
			return nil, err
		}
		return dp, nil
	default:
		return nil, fmt.Errorf("unknown part type: %s", alias.Type)
	}
}

// Message represents an A2A message.
type Message struct {
	Role     Role                   `json:"role"`
	Parts    []Part                 `json:"parts"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// messageJSON is the JSON representation used for custom (un)marshaling.
type messageJSON struct {
	Role     Role                   `json:"role"`
	Parts    []json.RawMessage      `json:"parts"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// MarshalJSON customizes JSON marshaling for Message.
func (m Message) MarshalJSON() ([]byte, error) {
	rawParts := make([]json.RawMessage, len(m.Parts))
	for i, p := range m.Parts {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("marshal part %d: %w", i, err)
		}
		rawParts[i] = b
	}
	return json.Marshal(messageJSON{
		Role:     m.Role,
		Parts:    rawParts,
		Metadata: m.Metadata,
	})
}

// UnmarshalJSON customizes JSON unmarshaling for Message.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw messageJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.Metadata = raw.Metadata
	m.Parts = make([]Part, len(raw.Parts))
	for i, rp := range raw.Parts {
		part, err := UnmarshalPart(rp)
		if err != nil {
			return fmt.Errorf("unmarshal part %d: %w", i, err)
		}
		m.Parts[i] = part
	}
	return nil
}

// Task represents an A2A task.
type Task struct {
	ID        string                 `json:"id"`
	ContextID string                 `json:"contextId,omitempty"`
	Status    TaskState              `json:"status"`
	Message   Message                `json:"message"`
	Artifacts []Artifact             `json:"artifacts,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// Artifact represents an output artifact from a task.
type Artifact struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parts       []Part `json:"parts"`
}

// artifactJSON is used for custom (un)marshaling of Artifact.
type artifactJSON struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Parts       []json.RawMessage `json:"parts"`
}

// MarshalJSON customizes JSON marshaling for Artifact.
func (a Artifact) MarshalJSON() ([]byte, error) {
	rawParts := make([]json.RawMessage, len(a.Parts))
	for i, p := range a.Parts {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		rawParts[i] = b
	}
	return json.Marshal(artifactJSON{
		Name:        a.Name,
		Description: a.Description,
		Parts:       rawParts,
	})
}

// UnmarshalJSON customizes JSON unmarshaling for Artifact.
func (a *Artifact) UnmarshalJSON(data []byte) error {
	var raw artifactJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	a.Name = raw.Name
	a.Description = raw.Description
	a.Parts = make([]Part, len(raw.Parts))
	for i, rp := range raw.Parts {
		part, err := UnmarshalPart(rp)
		if err != nil {
			return err
		}
		a.Parts[i] = part
	}
	return nil
}

// AgentSkill describes a skill an agent can perform.
type AgentSkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// AgentProvider describes the provider of an agent.
type AgentProvider struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// AgentCard describes an agent's capabilities and endpoint.
type AgentCard struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Version      string                 `json:"version"`
	Provider     *AgentProvider         `json:"provider,omitempty"`
	Skills       []AgentSkill           `json:"skills"`
	Endpoint     string                 `json:"endpoint"`
	Capabilities map[string]interface{} `json:"capabilities,omitempty"`
	Status       string                 `json:"status,omitempty"`
}
