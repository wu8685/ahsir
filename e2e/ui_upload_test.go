//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests cover the console's file-drop pipeline (added for the composer
// drag-to-attach feature). The browser hides a dragged file's local path, so
// the SPA uploads the bytes; the console copies them into an agent-readable
// upload dir and returns the on-disk path to inject into the prompt.
//
//   - TestUIConsole_Upload_E2E pins the /api/upload contract through the real
//     `ahsir ui` binary — no LLM, so it runs whenever the e2e tag is set.
//   - TestUIConsole_UploadThenAgentReads_E2E pins the cross-process wiring: a
//     file dropped in the console is actually readable by a (separately
//     spawned) fs-enabled agent, because the agent auto-allow-lists the same
//     upload dir. This needs a real model and is gated like the other claude
//     e2e tests.
//
// NOTE: the pure-frontend interactions shipped alongside this — the roundtable
// bubble "直接回复" button and dragging an agent card to insert "@name" — are
// client-side JS with no server contract, so they're out of scope here; they'd
// need a browser-automation harness the repo doesn't have.

type uploadFile struct {
	name    string
	content string
}

type uploadResp struct {
	Files []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"files"`
}

// startConsole boots a standalone `ahsir ui` against schedulerURL, waits for it
// to listen, registers teardown, and returns its base URL. uploadDir, when
// non-empty, is passed as --upload-dir; empty keeps the console's default
// (which resolves AHSIR_UPLOAD_DIR). The scheduler need not be reachable for
// routes the console serves itself (e.g. /api/upload).
func startConsole(t *testing.T, schedulerURL, uploadDir string) string {
	t.Helper()
	repoRoot := findRepoRoot(t)
	uiBin := filepath.Join(repoRoot, "bin", "ahsir")
	if _, err := os.Stat(uiBin); err != nil {
		t.Skipf("missing %s — from repo root run: go build -o bin/ahsir ./cmd/ahsir", uiBin)
	}

	consolePort, _, _ := allocateFreePorts(t, 3)
	args := []string{"ui",
		"--addr", fmt.Sprintf("127.0.0.1:%d", consolePort),
		"--scheduler", schedulerURL,
	}
	if uploadDir != "" {
		args = append(args, "--upload-dir", uploadDir)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, uiBin, args...)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logBuf := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = logBuf, logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start console: %v", err)
	}
	t.Cleanup(func() { cleanupSchedulerProcess(cmd, cancel, nil, t.Logf) })
	if err := waitForPort(consolePort, 15*time.Second); err != nil {
		t.Fatalf("console not ready: %v\nconsole log:\n%s", err, logBuf.String())
	}
	return fmt.Sprintf("http://127.0.0.1:%d", consolePort)
}

// doUpload POSTs files as multipart/form-data (field "file", as the SPA does)
// to the console's /api/upload and returns the parsed response.
func doUpload(t *testing.T, consoleURL string, files []uploadFile) uploadResp {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		w, err := mw.CreateFormFile("file", f.name)
		if err != nil {
			t.Fatalf("create form file %q: %v", f.name, err)
		}
		if _, err := w.Write([]byte(f.content)); err != nil {
			t.Fatalf("write form file %q: %v", f.name, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, consoleURL+"/api/upload", &body)
	if err != nil {
		t.Fatalf("build upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload POST: %v", err)
	}
	var out uploadResp
	decodeJSON(t, resp, &out)
	return out
}

// TestUIConsole_Upload_E2E pins the /api/upload contract end-to-end through the
// real console binary: a dropped file is materialised on disk under the upload
// dir with its content intact, multiple files in one request get distinct
// paths, and a path-traversal filename is reduced to its base inside the dir.
func TestUIConsole_Upload_E2E(t *testing.T) {
	uploadDir := filepath.Join(t.TempDir(), "uploads")
	// The upload route never proxies, so an unreachable scheduler URL is fine.
	consoleURL := startConsole(t, "http://127.0.0.1:1", uploadDir)
	sep := string(os.PathSeparator)

	// Single file: name (with a space) preserved, content round-trips, path
	// lands under the upload dir.
	const content = "drop-codeword-7321"
	got := doUpload(t, consoleURL, []uploadFile{{name: "shot report.png", content: content}})
	if len(got.Files) != 1 {
		t.Fatalf("want 1 file in response, got %+v", got)
	}
	if got.Files[0].Name != "shot report.png" {
		t.Errorf("name = %q, want original preserved", got.Files[0].Name)
	}
	p := got.Files[0].Path
	if !strings.HasPrefix(p, uploadDir+sep) {
		t.Errorf("path %q not under upload dir %q", p, uploadDir)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != content {
		t.Errorf("readback = %q (err %v), want %q", string(b), err, content)
	}

	// Multiple files in one request → distinct on-disk paths.
	multi := doUpload(t, consoleURL, []uploadFile{
		{name: "a.txt", content: "AAA"},
		{name: "b.txt", content: "BBB"},
	})
	if len(multi.Files) != 2 {
		t.Fatalf("want 2 files, got %+v", multi)
	}
	if multi.Files[0].Path == multi.Files[1].Path {
		t.Errorf("two uploads collided on one path: %+v", multi.Files)
	}

	// Path-traversal filename: only the base name survives, still inside the
	// upload dir (never escapes to ../../etc).
	evil := doUpload(t, consoleURL, []uploadFile{{name: "../../etc/evil.conf", content: "x"}})
	if len(evil.Files) != 1 {
		t.Fatalf("want 1 file, got %+v", evil)
	}
	ep := evil.Files[0].Path
	if filepath.Base(ep) != "evil.conf" {
		t.Errorf("traversal base = %q, want evil.conf", filepath.Base(ep))
	}
	if !strings.HasPrefix(ep, uploadDir+sep) {
		t.Errorf("traversal escaped the upload dir: %q", ep)
	}
}

// fsTeacherCardYAML is a teacher with filesystem access enabled — it can use
// the read-only Read/LS/Glob/Grep tools — so the upload-then-read chain can
// prove the dropped file is reachable. allowed_paths is just its workspace; the
// agent auto-adds the console upload dir on top (the behaviour under test).
const fsTeacherCardYAML = `name: teacher
description: e2e teacher agent with filesystem read access
version: "1.0.0"
provider:
  name: ahsir
  url: https://github.com/wu8685/ahsir
skills:
  - name: teaching
    description: answer questions, can read files
claude:
  systemPrompt: |
    You are a teacher. When asked to read a file at a given path, use your
    Read tool to read it and answer based on its contents. Answer concisely.
  maxAgentCalls: 0
runtime:
  command: claude
  args: []
  timeout: 300s
  provider: deepseek
  baseURL: https://api.deepseek.com/anthropic
  apiKey: "${MODEL_API_KEY}"
  model: deepseek-v4-pro
filesystem:
  enabled: true
  allowed_paths:
    - "."
network:
  bind: "127.0.0.1"
`

// TestUIConsole_UploadThenAgentReads_E2E proves the whole drop-a-file path:
// upload a file through the console, then ask a separately-spawned, fs-enabled
// agent to read that path. The agent can only succeed if it auto-allow-listed
// the console's upload dir (--add-dir) at startup — which is the cross-process
// contract this guards. The shared dir is pinned via AHSIR_UPLOAD_DIR so both
// the scheduler-spawned agent and the console resolve the same location.
func TestUIConsole_UploadThenAgentReads_E2E(t *testing.T) {
	requireClaudeE2E(t)

	uploadDir := filepath.Join(t.TempDir(), "uploads")
	// Set before booting the stack: the scheduler (and thus its agents) and the
	// console both inherit this, so ResolveUploadDir agrees on both sides.
	t.Setenv("AHSIR_UPLOAD_DIR", uploadDir)

	fix := setupE2EWithCards(t, fsTeacherCardYAML, studentCardYAML)
	consoleURL := startConsole(t, fix.schedulerURL(), "") // "" → console uses AHSIR_UPLOAD_DIR

	const codeword = "papaya-drop-9173"
	up := doUpload(t, consoleURL, []uploadFile{
		{name: "note.txt", content: "The secret codeword is " + codeword + "."},
	})
	if len(up.Files) != 1 {
		t.Fatalf("upload response = %+v, want 1 file", up)
	}
	path := up.Files[0].Path
	if !strings.HasPrefix(path, uploadDir+string(os.PathSeparator)) {
		t.Fatalf("uploaded path %q not under the shared upload dir %q", path, uploadDir)
	}

	prompt := fmt.Sprintf("Read the file at %s and reply with the exact codeword it contains, nothing else.", path)
	reply, err := fix.sendMessageToAgent("teacher", "upload-read-m1", "upload-read-conv", prompt)
	if err != nil {
		t.Fatalf("teacher read-file turn failed: %v\nscheduler log:\n%s", err, fix.schedulerLog())
	}
	if !strings.Contains(reply, codeword) {
		t.Fatalf("teacher reply %q is missing codeword %q — the agent could not read the uploaded file "+
			"(upload dir likely not on its --add-dir)\nscheduler log:\n%s", reply, codeword, fix.schedulerLog())
	}
}
