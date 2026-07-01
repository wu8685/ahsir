package main

// `ahsir ui` — start the standalone web console.
//
// The console is a separate, optional process from the scheduler: it serves a
// single-page interface and proxies the scheduler's gateway API so the operator
// can browse contexts, pick an agent, dispatch a turn, and inspect the result
// from a browser. It does no orchestration itself (see internal/ui).
//
// It needs a *running* scheduler at --scheduler. --config is only used to
// discover the control-plane admin token so agent start/stop from the console
// works without the operator pasting a secret; read/chat routes never need it.

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/wu8685/ahsir/internal/ui"
)

func uiCmd(args []string) {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:9801", "Address for the console to listen on")
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler base URL the console proxies to")
	cfgPath := fs.String("config", "", "Path to ahsir.yaml (used only to discover the admin token; default: auto-detect)")
	uploadDir := fs.String("upload-dir", "", "Directory composer file-drops are copied into (default: $AHSIR_UPLOAD_DIR or $TMPDIR/ahsir-uploads). Must be readable by the agents (their allowed_paths).")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: ahsir ui [--addr host:port] [--scheduler URL] [--config path] [--upload-dir path]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	// Resolve the admin token the same way the agent commands do, so the
	// console can drive the control plane (agent start/stop) transparently.
	// Empty is fine — the console only attaches it to /admin/* requests, and
	// the scheduler answers 401 with a hint if it actually requires one.
	configPath := *cfgPath
	if configPath == "" {
		configPath = resolveDefaultConfig()
	}
	adminToken := resolveAdminToken(configPath)

	srv, err := ui.New(*schedulerURL, adminToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building console: %v\n", err)
		os.Exit(1)
	}
	srv.SetUploadDir(*uploadDir) // empty keeps the shared default ($TMPDIR/ahsir-uploads)

	log.Printf("ahsir console listening on http://%s (scheduler: %s)", *addr, *schedulerURL)
	log.Printf("file-drop upload dir: %s (agents with filesystem.enabled auto-allow-list it)", srv.UploadDir())
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		fmt.Fprintf(os.Stderr, "console server error: %v\n", err)
		os.Exit(1)
	}
}
