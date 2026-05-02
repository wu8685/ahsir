package wrapper

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// FSMCPServer implements an in-process MCP HTTP server that provides
// filesystem tools (read_file, write_file, list_directory, search_files)
// scoped to a set of allowed paths.
type FSMCPServer struct {
	allowedPaths []string
	workDir      string
}

// NewFSMCPServer creates a new filesystem MCP server.
// allowedPaths are the paths the server is permitted to access.
// workDir is used to resolve relative allowed paths at construction time.
func NewFSMCPServer(allowedPaths []string, workDir string) *FSMCPServer {
	// Resolve workDir symlinks so path joins match EvalSymlinks output.
	if real, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = real
	}
	resolved := make([]string, len(allowedPaths))
	for i, p := range allowedPaths {
		if filepath.IsAbs(p) {
			resolved[i] = filepath.Clean(p)
		} else {
			resolved[i] = filepath.Join(workDir, p)
		}
		// Resolve symlinks so validation matches EvalSymlinks output.
		if real, err := filepath.EvalSymlinks(resolved[i]); err == nil {
			resolved[i] = real
		}
	}
	return &FSMCPServer{allowedPaths: resolved, workDir: workDir}
}

// ServeHTTP handles MCP JSON-RPC requests over HTTP.
func (s *FSMCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	resp, err := s.handleMessage(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

func (s *FSMCPServer) handleMessage(data []byte) ([]byte, error) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		ID      json.RawMessage `json:"id"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return s.errorResponse(nil, -32700, "Parse error")
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID)
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		return s.handleToolsCall(req.ID, req.Params)
	default:
		return s.errorResponse(req.ID, -32601, "Method not found")
	}
}

func (s *FSMCPServer) handleInitialize(id json.RawMessage) ([]byte, error) {
	result := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "ahsir-filesystem",
			"version": "1.0.0",
		},
	}
	return s.resultResponse(id, result)
}

func (s *FSMCPServer) handleToolsList(id json.RawMessage) ([]byte, error) {
	tools := []map[string]interface{}{
		{
			"name":        "read_file",
			"description": "Read the contents of a file. Returns the file content as text.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]string{"type": "string", "description": "Path to the file to read"},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "write_file",
			"description": "Write content to a file. Creates or overwrites the file.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]string{"type": "string", "description": "Path to the file to write"},
					"content": map[string]string{"type": "string", "description": "Content to write to the file"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			"name":        "list_directory",
			"description": "List the contents of a directory. Returns entry names, types, and sizes.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]string{"type": "string", "description": "Path to the directory to list"},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "search_files",
			"description": "Search for files matching a glob pattern within a directory.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]string{"type": "string", "description": "Directory to search in"},
					"pattern": map[string]string{"type": "string", "description": "Glob pattern to match (e.g., *.go, **/*.md)"},
				},
				"required": []string{"path", "pattern"},
			},
		},
	}
	return s.resultResponse(id, map[string]interface{}{"tools": tools})
}

func (s *FSMCPServer) handleToolsCall(id json.RawMessage, params json.RawMessage) ([]byte, error) {
	var call struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return s.errorResponse(id, -32602, "Invalid params")
	}

	switch call.Name {
	case "read_file":
		path, _ := call.Arguments["path"].(string)
		return s.handleReadFile(id, path)
	case "write_file":
		path, _ := call.Arguments["path"].(string)
		content, _ := call.Arguments["content"].(string)
		return s.handleWriteFile(id, path, content)
	case "list_directory":
		path, _ := call.Arguments["path"].(string)
		return s.handleListDirectory(id, path)
	case "search_files":
		path, _ := call.Arguments["path"].(string)
		pattern, _ := call.Arguments["pattern"].(string)
		return s.handleSearchFiles(id, path, pattern)
	default:
		return s.errorResponse(id, -32601, fmt.Sprintf("Unknown tool: %s", call.Name))
	}
}

// validatePath resolves the requested path and checks it against allowed paths.
// Returns the resolved absolute path or an error.
func (s *FSMCPServer) validatePath(requestedPath string) (string, error) {
	if requestedPath == "" {
		return "", fmt.Errorf("path is required")
	}

	// Resolve relative paths against workDir, keep absolute paths as-is
	resolved := requestedPath
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(s.workDir, resolved)
	}

	// Clean and resolve symlinks
	cleaned := filepath.Clean(resolved)
	real, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		// If the file doesn't exist, EvalSymlinks will fail.
		// For write operations, the file may not exist yet.
		// Fall back to cleaned path and check if parent directory exists and is allowed.
		if os.IsNotExist(err) {
			real = cleaned
		} else {
			return "", fmt.Errorf("cannot resolve path: %w", err)
		}
	}

	// Check against allowed paths
	for _, allowed := range s.allowedPaths {
		if strings.HasPrefix(real, allowed+string(filepath.Separator)) || real == allowed {
			return real, nil
		}
	}

	return "", fmt.Errorf("access denied: path %q is not within allowed paths", requestedPath)
}

func (s *FSMCPServer) handleReadFile(id json.RawMessage, path string) ([]byte, error) {
	resolved, err := s.validatePath(path)
	if err != nil {
		return s.textResult(id, fmt.Sprintf("Error: %v", err))
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return s.textResult(id, fmt.Sprintf("Error reading file: %v", err))
	}

	return s.textResult(id, string(data))
}

func (s *FSMCPServer) handleWriteFile(id json.RawMessage, path, content string) ([]byte, error) {
	resolved, err := s.validatePath(path)
	if err != nil {
		return s.textResult(id, fmt.Sprintf("Error: %v", err))
	}

	if err := os.WriteFile(resolved, []byte(content), 0644); err != nil {
		return s.textResult(id, fmt.Sprintf("Error writing file: %v", err))
	}

	return s.textResult(id, fmt.Sprintf("File written successfully: %s", resolved))
}

func (s *FSMCPServer) handleListDirectory(id json.RawMessage, path string) ([]byte, error) {
	resolved, err := s.validatePath(path)
	if err != nil {
		return s.textResult(id, fmt.Sprintf("Error: %v", err))
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return s.textResult(id, fmt.Sprintf("Error reading directory: %v", err))
	}

	type entry struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Size  int64  `json:"size"`
	}
	result := make([]entry, 0, len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		var size int64
		var entryType string
		if info != nil {
			size = info.Size()
			if info.IsDir() {
				entryType = "directory"
			} else {
				entryType = "file"
			}
		}
		result = append(result, entry{Name: e.Name(), Type: entryType, Size: size})
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return s.textResult(id, string(data))
}

func (s *FSMCPServer) handleSearchFiles(id json.RawMessage, path, pattern string) ([]byte, error) {
	resolved, err := s.validatePath(path)
	if err != nil {
		return s.textResult(id, fmt.Sprintf("Error: %v", err))
	}

	if pattern == "" {
		return s.textResult(id, "Error: pattern is required")
	}

	var matches []string
	err = filepath.WalkDir(resolved, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible entries
		}
		// Re-check path is within allowed paths (prevent symlink escapes during walk)
		real, resolveErr := filepath.EvalSymlinks(p)
		if resolveErr != nil && !os.IsNotExist(resolveErr) {
			return nil
		}
		if resolveErr == nil {
			allowed := false
			for _, a := range s.allowedPaths {
				if strings.HasPrefix(real, a+string(filepath.Separator)) || real == a {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil
			}
		}

		rel, _ := filepath.Rel(resolved, p)
		matched, _ := filepath.Match(pattern, filepath.Base(p))
		if matched {
			matches = append(matches, rel)
		}
		// Also try glob matching on the full relative path
		if !matched {
			globMatched, _ := filepath.Match(pattern, rel)
			if globMatched {
				matches = append(matches, rel)
			}
		}
		return nil
	})

	if err != nil {
		return s.textResult(id, fmt.Sprintf("Error searching files: %v", err))
	}

	data, _ := json.MarshalIndent(matches, "", "  ")
	return s.textResult(id, string(data))
}

func (s *FSMCPServer) textResult(id json.RawMessage, text string) ([]byte, error) {
	content := []map[string]interface{}{
		{"type": "text", "text": text},
	}
	return s.resultResponse(id, map[string]interface{}{"content": content})
}

func (s *FSMCPServer) resultResponse(id json.RawMessage, result interface{}) ([]byte, error) {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"result":  result,
		"id":      id,
	}
	return json.Marshal(resp)
}

func (s *FSMCPServer) errorResponse(id json.RawMessage, code int, message string) ([]byte, error) {
	resp := map[string]interface{}{
		"jsonrpc": "2.0",
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
		},
		"id": id,
	}
	return json.Marshal(resp)
}

// HandleMessage processes an incoming JSON-RPC message and returns the response.
func (s *FSMCPServer) HandleMessage(data []byte) ([]byte, error) {
	return s.handleMessage(data)
}

// ReadFile reads a file after validating it against allowed paths.
func (s *FSMCPServer) ReadFile(path string) (string, error) {
	resolved, err := s.validatePath(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteMCPConfig writes a .mcp.json to the workspace directory that configures
// a stdio-based MCP server (command + args) so `claude -p` can spawn it.
func WriteMCPConfig(workDir string, agentBinary string) error {
	// Resolve to absolute paths so the spawned subprocess can find the workspace
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}
	config := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"ahsir-filesystem": map[string]interface{}{
				"command": agentBinary,
				"args":    []string{"--fs-mcp", "--workspace", absWorkDir},
			},
		},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workDir, ".mcp.json"), data, 0644)
}

// RemoveMCPConfig removes the .mcp.json from the workspace directory.
func RemoveMCPConfig(workDir string) error {
	err := os.Remove(filepath.Join(workDir, ".mcp.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
