package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/wu8685/ahsir/internal/wrapper"
)

func main() {
	workspace := flag.String("workspace", "", "Workspace directory")
	port := flag.Int("port", 0, "Listen port")
	registry := flag.String("registry", "", "Registry URL")
	flag.Parse()

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
		Port:        *port,
		RegistryURL: *registry,
		AgentCard:   runtimeCard,
	}

	w := wrapper.NewAgentWrapper(wrapperCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		log.Fatalf("Failed to start agent wrapper: %v", err)
	}

	log.Printf("Agent %s listening on port %d", runtimeCard.Name, *port)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	w.Stop(ctx)
}
