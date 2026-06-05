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
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
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
	case "agent":
		agentCmd(os.Args[2:])
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
func resolveDefaultConfig() string {
	const localName = "ahsir.yaml"
	if _, err := os.Stat(localName); err == nil {
		return localName
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return localName
	}
	global := filepath.Join(home, ".ahsir", "ahsir.yaml")
	if _, err := os.Stat(global); err == nil {
		return global
	}
	// Neither exists — return the local name so the LoadConfig error
	// points at the conventional location rather than something deep
	// in $HOME.
	return localName
}

func usage() {
	fmt.Println("Usage: ahsir <command> [flags]")
	fmt.Println()
	fmt.Println("Scheduler:")
	fmt.Println("  start [config]                       Start the scheduler (long-running). Default config: ahsir.yaml")
	fmt.Println()
	fmt.Println("Interact with a running scheduler (default --scheduler http://127.0.0.1:9800):")
	fmt.Println("  list [--json]                        List registered agents")
	fmt.Println("  chat <agent> \"<message>\" [--context ID]")
	fmt.Println("                                       Send a message, print the reply")
	fmt.Println("  status <agent> <task-id>             Print a task's status JSON")
	fmt.Println("  ping                                 Check whether scheduler is reachable (exit 0/2)")
	fmt.Println()
	fmt.Println("Persona management:")
	fmt.Println("  agent new <name> [flags]             Scaffold + start an agent")
	fmt.Println("  agent delete <name>                  Stop a running agent (files preserved)")
	fmt.Println("  agent list-configs                   Show agents in ahsir.yaml")
}

func startCmd(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	// Resolution order, when no positional arg:
	//   1. ./ahsir.yaml         (project-scoped — e.g. running from
	//                            example/multi-agent/)
	//   2. ~/.ahsir/ahsir.yaml  (global — agents created via
	//                            `ahsir agent new` without --config)
	//   3. ./ahsir.yaml         (final fallback so the error message
	//                            points at the conventional location
	//                            even when neither exists)
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
