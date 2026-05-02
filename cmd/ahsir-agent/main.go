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
	"time"

	"github.com/wu8685/ahsir/internal/wrapper"
)

func main() {
	workspace := flag.String("workspace", "", "Workspace directory")
	port := flag.Int("port", 0, "Listen port")
	registry := flag.String("registry", "", "Registry URL")
	fsMCP := flag.Bool("fs-mcp", false, "Run as filesystem MCP stdio server")
	flag.Parse()

	// --fs-mcp mode: run as a filesystem MCP stdio server for claude -p to spawn
	if *fsMCP {
		if *workspace == "" {
			fmt.Fprintf(os.Stderr, "Usage: ahsir-agent --fs-mcp --workspace=<path>\n")
			os.Exit(1)
		}
		builder := wrapper.NewAgentCardBuilder(*workspace)
		cfg, err := builder.Load()
		if err != nil {
			log.Fatalf("Failed to load agent card: %v", err)
		}
		srv := wrapper.NewFSMCPServer(cfg.Filesystem.AllowedPaths, *workspace)
		runStdioMCPServer(srv)
		return
	}

	if *workspace == "" {
		fmt.Fprintf(os.Stderr, "Usage: ahsir-agent --workspace=<path> [--port=<port>] [--registry=<url>]\n")
		os.Exit(1)
	}

	// Load agent card
	builder := wrapper.NewAgentCardBuilder(*workspace)
	cfg, err := builder.Load()
	if err != nil {
		log.Fatalf("Failed to load agent card: %v", err)
	}

	runtimeCard := builder.BuildRuntime(cfg, *port)

	wrapperCfg := wrapper.AgentWrapperConfig{
		Port:         *port,
		RegistryURL:  *registry,
		AgentCard:    runtimeCard,
		FSCfg:        cfg.Filesystem,
		WorkspaceDir: *workspace,
	}

	w := wrapper.NewAgentWrapper(wrapperCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		log.Fatalf("Failed to start agent wrapper: %v", err)
	}

	log.Printf("Agent %s listening on port %d", runtimeCard.Name, *port)

	// Setup executor if registry URL is configured
	if *registry != "" {
		sessionCfg := wrapper.SessionConfig{
			Command: "claude",
			Args:    []string{"-p", "--output-format", "text"},
			WorkDir: *workspace,
			Timeout: 120 * time.Second,
		}
		session := wrapper.NewSessionManager(sessionCfg)
		if err := session.Start(ctx); err != nil {
			log.Printf("Warning: Failed to start Claude Code session: %v", err)
		} else {
			defer session.Stop()

			listAgents := wrapper.RegistryAgentLister(*registry)
			callAgent := wrapper.RegistryAgentCaller(*registry)
			maxCalls := cfg.Claude.MaxAgentCalls
			basePrompt := cfg.Claude.SystemPrompt

			w.SetupExecutor(session, listAgents, callAgent, maxCalls, basePrompt)
			log.Printf("Executor wired: Claude Code session ready")
		}
	}

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	w.Stop(ctx)
}

// runStdioMCPServer runs an MCP stdio server using the FSMCPServer.
// It reads JSON-RPC requests from stdin and writes responses to stdout.
func runStdioMCPServer(srv *wrapper.FSMCPServer) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		data := scanner.Bytes()
		if len(data) == 0 {
			continue
		}
		resp, err := srv.HandleMessage(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "MCP error: %v\n", err)
			continue
		}
		os.Stdout.Write(resp)
		os.Stdout.Write([]byte("\n"))
	}
}
