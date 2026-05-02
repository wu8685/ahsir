package wrapper

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFSMCPInitialize(t *testing.T) {
	srv := NewFSMCPServer([]string{"."}, "/tmp")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := mcpCall(t, ts.URL, "initialize", nil)
	result := resp["result"].(map[string]interface{})

	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion 2024-11-05, got %v", result["protocolVersion"])
	}

	serverInfo := result["serverInfo"].(map[string]interface{})
	if serverInfo["name"] != "ahsir-filesystem" {
		t.Errorf("expected server name ahsir-filesystem, got %v", serverInfo["name"])
	}
}

func TestFSMCPToolsList(t *testing.T) {
	srv := NewFSMCPServer([]string{"."}, "/tmp")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := mcpCall(t, ts.URL, "tools/list", nil)
	result := resp["result"].(map[string]interface{})
	tools := result["tools"].([]interface{})

	if len(tools) != 4 {
		t.Fatalf("expected 4 tools, got %d", len(tools))
	}

	names := make(map[string]bool)
	for _, t := range tools {
		tool := t.(map[string]interface{})
		names[tool["name"].(string)] = true
	}

	for _, n := range []string{"read_file", "write_file", "list_directory", "search_files"} {
		if !names[n] {
			t.Errorf("expected tool %q not found", n)
		}
	}
}

func TestReadFileWithinAllowedPath(t *testing.T) {
	dir := t.TempDir()
	content := "hello world"
	os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0644)

	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "read_file", "arguments": map[string]interface{}{"path": "test.txt"}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	contentArr := getContent(t, resp)
	text := contentArr[0].(map[string]interface{})["text"].(string)
	if text != content {
		t.Errorf("expected %q, got %q", content, text)
	}
}

func TestReadFileOutsideAllowedPath(t *testing.T) {
	dir := t.TempDir()
	otherDir := t.TempDir()
	os.WriteFile(filepath.Join(otherDir, "secret.txt"), []byte("secret"), 0644)

	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "read_file", "arguments": map[string]interface{}{"path": filepath.Join(otherDir, "secret.txt")}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	text := getContentText(t, resp)
	if !strings.Contains(text, "access denied") {
		t.Errorf("expected access denied, got %q", text)
	}
}

func TestReadFileTraversalPrevention(t *testing.T) {
	dir := t.TempDir()
	otherDir := t.TempDir()
	os.WriteFile(filepath.Join(otherDir, "secret.txt"), []byte("secret"), 0644)

	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "read_file", "arguments": map[string]interface{}{"path": filepath.Join("..", filepath.Base(otherDir), "secret.txt")}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	text := getContentText(t, resp)
	if !strings.Contains(text, "access denied") {
		t.Errorf("expected access denied for traversal, got %q", text)
	}
}

func TestWriteFileWithinAllowedPath(t *testing.T) {
	dir := t.TempDir()
	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "write_file", "arguments": map[string]interface{}{"path": "output.txt", "content": "generated content"}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	text := getContentText(t, resp)
	if !strings.Contains(text, "File written successfully") {
		t.Errorf("expected success message, got %q", text)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "output.txt"))
	if string(data) != "generated content" {
		t.Errorf("expected 'generated content', got %q", string(data))
	}
}

func TestWriteFileOutsideAllowedPath(t *testing.T) {
	dir := t.TempDir()
	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "write_file", "arguments": map[string]interface{}{"path": "/etc/hosts", "content": "malicious"}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	text := getContentText(t, resp)
	if !strings.Contains(text, "access denied") {
		t.Errorf("expected access denied, got %q", text)
	}
}

func TestListDirectoryWithinAllowedPath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)

	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "list_directory", "arguments": map[string]interface{}{"path": "."}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	text := getContentText(t, resp)
	if !strings.Contains(text, "a.txt") || !strings.Contains(text, "b.txt") || !strings.Contains(text, "subdir") {
		t.Errorf("expected entries not found in: %s", text)
	}
}

func TestListDirectoryOutsideAllowedPath(t *testing.T) {
	dir := t.TempDir()
	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "list_directory", "arguments": map[string]interface{}{"path": "/etc"}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	text := getContentText(t, resp)
	if !strings.Contains(text, "access denied") {
		t.Errorf("expected access denied, got %q", text)
	}
}

func TestSearchFilesWithinAllowedPath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package main"), 0644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# readme"), 0644)

	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "search_files", "arguments": map[string]interface{}{"path": ".", "pattern": "*.go"}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	text := getContentText(t, resp)
	if !strings.Contains(text, "main.go") || !strings.Contains(text, "main_test.go") {
		t.Errorf("expected .go files in: %s", text)
	}
	if strings.Contains(text, "README.md") {
		t.Errorf("unexpected README.md in go file search: %s", text)
	}
}

func TestSearchFilesOutsideAllowedPath(t *testing.T) {
	dir := t.TempDir()
	srv := NewFSMCPServer([]string{dir}, dir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "search_files", "arguments": map[string]interface{}{"path": "/etc", "pattern": "*"}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	text := getContentText(t, resp)
	if !strings.Contains(text, "access denied") {
		t.Errorf("expected access denied, got %q", text)
	}
}

func TestWriteMCPConfig(t *testing.T) {
	dir := t.TempDir()
	err := WriteMCPConfig(dir, "/usr/local/bin/ahsir-agent")
	if err != nil {
		t.Fatalf("WriteMCPConfig failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("failed to read .mcp.json: %v", err)
	}

	var cfg map[string]interface{}
	json.Unmarshal(data, &cfg)
	servers := cfg["mcpServers"].(map[string]interface{})
	fsSrv := servers["ahsir-filesystem"].(map[string]interface{})
	if fsSrv["command"] != "/usr/local/bin/ahsir-agent" {
		t.Errorf("unexpected command: %v", fsSrv["command"])
	}
	args := fsSrv["args"].([]interface{})
	if len(args) != 3 || args[0] != "--fs-mcp" || args[1] != "--workspace" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestRemoveMCPConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}"), 0644)

	err := RemoveMCPConfig(dir)
	if err != nil {
		t.Fatalf("RemoveMCPConfig failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); !os.IsNotExist(err) {
		t.Error(".mcp.json should not exist after removal")
	}
}

func TestRemoveMCPConfigNotExist(t *testing.T) {
	dir := t.TempDir()
	err := RemoveMCPConfig(dir)
	if err != nil {
		t.Errorf("expected no error for non-existent .mcp.json, got: %v", err)
	}
}

func TestPathValidationMultipleAllowedPaths(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir1, "f1.txt"), []byte("f1"), 0644)
	os.WriteFile(filepath.Join(dir2, "f2.txt"), []byte("f2"), 0644)

	srv := NewFSMCPServer([]string{dir1, dir2}, dir1)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Access file in dir1
	params := map[string]interface{}{"name": "read_file", "arguments": map[string]interface{}{"path": "f1.txt"}}
	resp := mcpCall(t, ts.URL, "tools/call", params)
	if text := getContentText(t, resp); text != "f1" {
		t.Errorf("expected f1, got %q", text)
	}

	// Access file in dir2
	params = map[string]interface{}{"name": "read_file", "arguments": map[string]interface{}{"path": filepath.Join(dir2, "f2.txt")}}
	resp = mcpCall(t, ts.URL, "tools/call", params)
	if text := getContentText(t, resp); text != "f2" {
		t.Errorf("expected f2, got %q", text)
	}
}

func TestUnknownTool(t *testing.T) {
	srv := NewFSMCPServer([]string{"."}, "/tmp")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	params := map[string]interface{}{"name": "nonexistent_tool", "arguments": map[string]interface{}{}}
	resp := mcpCall(t, ts.URL, "tools/call", params)

	if _, ok := resp["result"]; ok {
		t.Error("expected error response for unknown tool")
	}
}

// --- helpers ---

func mcpCall(t *testing.T, url string, method string, params interface{}) map[string]interface{} {
	t.Helper()

	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
		"id":      "test-1",
	}
	if params != nil {
		body["params"] = params
	}

	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("HTTP POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("JSON decode failed: %v", err)
	}
	return result
}

func getContent(t *testing.T, resp map[string]interface{}) []interface{} {
	t.Helper()
	result := resp["result"].(map[string]interface{})
	return result["content"].([]interface{})
}

func getContentText(t *testing.T, resp map[string]interface{}) string {
	t.Helper()
	content := getContent(t, resp)
	return content[0].(map[string]interface{})["text"].(string)
}
