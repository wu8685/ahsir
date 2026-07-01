package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/wu8685/ahsir/internal/scheduler"
	"github.com/wu8685/ahsir/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(version.String())
	case "start":
		startCmd(os.Args[2:])
	case "list":
		listCmd(os.Args[2:])
	case "chat":
		chatCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
	case "ping":
		pingCmd(os.Args[2:])
	case "doctor":
		doctorCmd(os.Args[2:])
	case "trace":
		traceCmd(os.Args[2:])
	case "history":
		historyCmd(os.Args[2:])
	case "agent":
		agentCmd(os.Args[2:])
	case "ui":
		uiCmd(os.Args[2:])
	case "stop":
		fmt.Println("Stopping scheduler...")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

// resolveDefaultConfig picks the most appropriate config path when the
// user doesn't pass one. CWD wins if a project-local file exists —
// preserves the existing convention for `ahsir start` from a project
// dir — otherwise falls back to the global file under ~/.ahsir/.
// resolveDefaultConfig picks the config for `ahsir start` when no path is given.
// It prefers the GLOBAL ~/.ahsir/ahsir.yaml so that everything — agents, the
// invocation ledger, per-agent transcripts, and roundtable rooms — converges
// under ~/.ahsir by default, matching where `ahsir agent new` writes. A
// project-local ./ahsir.yaml is used only when no global config exists, and an
// explicit positional arg always overrides both.
func resolveDefaultConfig() string {
	const localName = "ahsir.yaml"
	home, err := os.UserHomeDir()
	if err != nil {
		// Pathological case (no $HOME): fall back to the local config.
		return localName
	}
	global := filepath.Join(home, ".ahsir", "ahsir.yaml")
	if _, err := os.Stat(global); err == nil {
		return global
	}
	// No global config yet — honour a project-local one if present.
	if _, err := os.Stat(localName); err == nil {
		return localName
	}
	// Neither exists — point the error at the global location, the
	// conventional home that `ahsir agent new` also targets.
	return global
}

func usage() {
	fmt.Println("Usage: ahsir <command> [flags]")
	fmt.Println()
	fmt.Println("Scheduler:")
	fmt.Println("  start [config]                       Start the scheduler (long-running). Default config: ~/.ahsir/ahsir.yaml")
	fmt.Println()
	fmt.Println("Web console (optional, independent process):")
	fmt.Println("  ui [--addr host:port] [--scheduler URL]")
	fmt.Println("                                       Start the browser console (default http://127.0.0.1:9801)")
	fmt.Println()
	fmt.Println("Interact with a running scheduler (default --scheduler http://127.0.0.1:9800):")
	fmt.Println("  list [--json]                        List registered agents")
	fmt.Println("  chat <agent> \"<message>\" [--context ID] [--as NAME]")
	fmt.Println("                                       Send a message, print the reply (--as: speaker identity")
	fmt.Println("                                       on shared contexts; default: OS username)")
	fmt.Println("  status <agent> <task-id>             Print a task's status JSON")
	fmt.Println("  ping                                 Check whether scheduler is reachable (exit 0/2)")
	fmt.Println()
	fmt.Println("Observability:")
	fmt.Println("  doctor [--config path]               Health checklist: config, provider CLIs, scheduler, agents, auth")
	fmt.Println("  trace [contextId] [--json]           Show the invocation ledger (filtered by contextId)")
	fmt.Println("  history <agent> <contextId> [--json] Replay a context's transcript (speaker, question, reply)")
	fmt.Println()
	fmt.Println("Persona management:")
	fmt.Println("  agent new <name> [flags]             Scaffold + start an agent")
	fmt.Println("  agent delete <name>                  Stop a running agent (files preserved)")
	fmt.Println("  agent restart <name>                 Restart an agent so it re-reads its edited agent-card.yaml")
	fmt.Println("  agent list-configs                   Show agents in ahsir.yaml")
	fmt.Println()
	fmt.Println("Misc:")
	fmt.Println("  version | --version | -v             Print the ahsir build version")
}

func startCmd(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	// Resolution order, when no positional arg (see resolveDefaultConfig):
	//   1. ~/.ahsir/ahsir.yaml  (global — the default home; agents + ledger +
	//                            transcripts + rooms all converge here)
	//   2. ./ahsir.yaml         (project-scoped — only when no global exists)
	//   3. ~/.ahsir/ahsir.yaml  (final fallback for the error message)
	configPath := resolveDefaultConfig()
	if rest := fs.Args(); len(rest) > 0 {
		configPath = rest[0]
	}

	cfg, err := scheduler.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	sch := scheduler.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := sch.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting scheduler: %v\n", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	log.Println("Signal received, shutting down...")
	cancel()
	sch.Stop()
}
