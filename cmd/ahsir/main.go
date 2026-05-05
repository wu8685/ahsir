package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/wu8685/ahsir/internal/mcp"
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
	case "mcp":
		mcpCmd(os.Args[2:])
	case "stop":
		fmt.Println("Stopping scheduler...")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println("Usage: ahsir <command>")
	fmt.Println("Commands:")
	fmt.Println("  start [config]              Start the scheduler (long-running). Default config: ahsir.yaml")
	fmt.Println("  mcp --scheduler <url>       Run an MCP stdio shim that forwards to a running scheduler")
	fmt.Println("  stop                        Stop the scheduler")
}

func startCmd(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	configPath := "ahsir.yaml"
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

// mcpCmd runs the MCP stdio shim. It does not spawn agents, does not load
// ahsir.yaml, does not embed a scheduler — it is purely a protocol adapter
// that forwards each tools/call to a running scheduler over HTTP.
//
// MCP clients (like Claude Code reading .mcp.json) launch us with a piped
// stdin; we read JSON-RPC messages line-by-line and write responses to
// stdout. Logs go to stderr so they don't corrupt the MCP wire.
func mcpCmd(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	schedulerURL := fs.String("scheduler", "http://127.0.0.1:9800", "Scheduler base URL (the same host:port that ahsir start binds for its registry/gateway)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	client := mcp.NewSchedulerHTTPClient(*schedulerURL)
	server := mcp.NewServer(client)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	log.SetOutput(os.Stderr)
	// Pull the gateway-side chat timeout from the scheduler so the shim's
	// own http.Client.Timeout (the outermost cap) matches what the operator
	// configured in ahsir.yaml — keeping timeout config to a single place.
	if effective, err := client.RefreshTimeout(); err != nil {
		log.Printf("ahsir mcp shim: timeout sync failed, using default %v: %v", effective, err)
	} else {
		log.Printf("ahsir mcp shim: client timeout aligned to %v (scheduler chat + 1m buffer)", effective)
	}
	log.Printf("ahsir mcp shim ready (scheduler=%s); reading JSON-RPC from stdin", *schedulerURL)

	for scanner.Scan() {
		data := scanner.Bytes()
		if len(data) == 0 {
			continue
		}
		resp, err := server.HandleMessage(data)
		if err != nil {
			log.Printf("MCP error: %v", err)
			continue
		}
		if resp != nil {
			os.Stdout.Write(resp)
			os.Stdout.Write([]byte("\n"))
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("stdin scanner error: %v", err)
	}
}
