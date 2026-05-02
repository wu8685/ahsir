package main

import (
	"bufio"
	"context"
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
		fmt.Println("Usage: ahsir <command>")
		fmt.Println("Commands:")
		fmt.Println("  start [config]  Start the scheduler (default: ahsir.yaml)")
		fmt.Println("  stop            Stop the scheduler")
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "start":
		configPath := "ahsir.yaml"
		if len(os.Args) > 2 {
			configPath = os.Args[2]
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

		// Handle graceful shutdown
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		// MCP stdio loop in goroutine for MCP mode
		go func() {
			mcpServer := mcp.NewServer(sch)
			scanner := bufio.NewScanner(os.Stdin)
			// Increase buffer for large messages
			scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

			log.Println("MCP server ready, waiting for messages on stdin...")
			for scanner.Scan() {
				data := scanner.Bytes()
				if len(data) == 0 {
					continue
				}
				resp, err := mcpServer.HandleMessage(data)
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
		}()

		// Wait for shutdown signal
		<-sigCh
		log.Println("Signal received, shutting down...")
		cancel()
		sch.Stop()
	case "stop":
		fmt.Println("Stopping scheduler...")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		os.Exit(1)
	}
}
