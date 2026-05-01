package main

import (
	"fmt"
	"os"

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
		if err := sch.Start(nil); err != nil {
			fmt.Fprintf(os.Stderr, "Error starting scheduler: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Scheduler started")
		select {}
	case "stop":
		fmt.Println("Stopping scheduler...")
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", cmd)
		os.Exit(1)
	}
}
