package scheduler

// ahsir.yaml in-place editor. `ahsir agent new` / `ahsir agent delete`
// need to append/remove agent declarations without nuking surrounding
// comments or top-level field order. Plain yaml.Marshal would re-emit
// the file from a struct round-trip, losing both. We use yaml.Node so
// the existing document tree stays intact and we only mutate the one
// list it cares about.

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// AddAgentToConfig appends an agent block to the `agents:` list in the
// ahsir.yaml at path. Idempotent on duplicate-name: returns nil + does
// nothing if an agent with that name already exists in the file.
//
// The file is read, mutated as a yaml.Node tree, re-emitted, and atomically
// renamed into place — so a partial write never leaves a half-edited
// config behind.
func AddAgentToConfig(path, name, workspace string, port int) error {
	root, err := readYAMLNode(path)
	if err != nil {
		return err
	}

	agentsSeq, err := agentsSequence(root)
	if err != nil {
		return err
	}

	// Idempotent: caller may legitimately re-run `agent new` (e.g. after
	// a partial failure). Don't append a duplicate.
	for _, entry := range agentsSeq.Content {
		if existing := mappingFieldString(entry, "name"); existing == name {
			return nil
		}
	}

	newEntry := buildAgentMapping(name, workspace, port)
	agentsSeq.Content = append(agentsSeq.Content, newEntry)
	// Force block style — the original `agents: []` is flow style and
	// yaml.v3 preserves it across round-trips, which makes appended
	// entries unreadable on a single line. Style=0 means "default", which
	// the encoder renders as block for sequences > 0 length.
	agentsSeq.Style = 0

	return writeYAMLNodeAtomic(path, root)
}

// RemoveAgentFromConfig drops the agent entry whose `name:` matches the
// given value from ahsir.yaml at path. Idempotent on missing.
func RemoveAgentFromConfig(path, name string) error {
	root, err := readYAMLNode(path)
	if err != nil {
		return err
	}
	agentsSeq, err := agentsSequence(root)
	if err != nil {
		return err
	}

	kept := agentsSeq.Content[:0]
	for _, entry := range agentsSeq.Content {
		if mappingFieldString(entry, "name") == name {
			continue
		}
		kept = append(kept, entry)
	}
	agentsSeq.Content = kept

	return writeYAMLNodeAtomic(path, root)
}

// --- helpers ---

func readYAMLNode(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &root, nil
}

// writeYAMLNodeAtomic emits root and renames into place. Same atomic
// semantics as FilePersistence.Save in the wrapper package, kept simple
// because this file is rewritten infrequently.
func writeYAMLNodeAtomic(path string, root *yaml.Node) error {
	tmp, err := os.CreateTemp("", "ahsir-yaml-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op if rename succeeded

	enc := yaml.NewEncoder(tmp)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("close encoder: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s → %s: %w", tmpPath, path, err)
	}
	return nil
}

// agentsSequence walks the document tree to find the top-level `agents:`
// sequence node. Returns an error if the file's shape doesn't match what
// the config loader expects (caller can decide whether to bail or
// scaffold a default).
func agentsSequence(root *yaml.Node) (*yaml.Node, error) {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("yaml: empty or non-document root")
	}
	mapping := root.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml: top-level is not a mapping (got kind=%d)", mapping.Kind)
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		val := mapping.Content[i+1]
		if key.Value == "agents" {
			if val.Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("yaml: `agents` is not a sequence (got kind=%d)", val.Kind)
			}
			return val, nil
		}
	}
	return nil, fmt.Errorf("yaml: top-level `agents` key not found")
}

// mappingFieldString returns the string value of a top-level field on a
// mapping node, or "" if not present / not a scalar.
func mappingFieldString(mapping *yaml.Node, fieldName string) string {
	if mapping.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == fieldName {
			return mapping.Content[i+1].Value
		}
	}
	return ""
}

// buildAgentMapping constructs a fresh mapping node:
//
//	- name: <name>
//	  workspace: <workspace>
//	  port: <port>   # omitted when port == 0 (auto-allocate)
func buildAgentMapping(name, workspace string, port int) *yaml.Node {
	pairs := []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "name"},
		{Kind: yaml.ScalarNode, Value: name, Style: yaml.DoubleQuotedStyle},
		{Kind: yaml.ScalarNode, Value: "workspace"},
		{Kind: yaml.ScalarNode, Value: workspace, Style: yaml.DoubleQuotedStyle},
	}
	if port > 0 {
		pairs = append(pairs,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "port"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%d", port), Tag: "!!int"},
		)
	}
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Content: pairs,
	}
}
