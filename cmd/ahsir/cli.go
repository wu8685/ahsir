package main

// CLI subcommands that talk to a running scheduler over its HTTP gateway.
//
// All of these reuse mcp.SchedulerHTTPClient — the same code path that
// powers the MCP shim. The only difference here is output formatting:
// these commands are designed for human eyes (and, by extension, Claude
// Code's Bash tool, which reads stdout as plain text).
//
// Output conventions:
//   - `chat`:   pure reply text on stdout, nothing else. Composable.
//   - `list`:   one agent per line, `name\tskills...` format.
//   - `status`: pretty-printed task JSON.
//   - `ping`:   prints "ok" + exits 0 if scheduler is reachable, else
//               error to stderr + exit 2.
//
// Every command shares a --scheduler flag (default http://127.0.0.1:9800).
// Anything else (timeouts, etc.) is fetched from the scheduler itself via
// the same /config/timeouts endpoint the MCP shim uses.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/wu8685/ahsir/internal/mcp"
)

const defaultSchedulerURL = "http://127.0.0.1:9800"

// newSchedulerClient builds an HTTP client against the given URL and
// asks the scheduler for its configured chat timeout so the client's
// own http.Client.Timeout matches what the operator put in ahsir.yaml.
// Falls back to the client's default if the scheduler isn't reachable
// (the next real call will fail with a clearer error).
func newSchedulerClient(schedulerURL string) *mcp.SchedulerHTTPClient {
	c := mcp.NewSchedulerHTTPClient(schedulerURL)
	_, _ = c.RefreshTimeout()
	return c
}

// listCmd: `ahsir list [--scheduler URL]`
func listCmd(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler base URL")
	jsonOut := fs.Bool("json", false, "Output as JSON instead of plain text")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	client := newSchedulerClient(*schedulerURL)
	agents := client.ListAgents()

	if *jsonOut {
		out, _ := json.MarshalIndent(agents, "", "  ")
		fmt.Println(string(out))
		return
	}

	if len(agents) == 0 {
		// Empty registry is a legitimate state, not an error — the scheduler
		// is running fine, there just aren't any agents configured yet. Exit
		// 0 so shell chains (and Claude Code's Bash tool) don't treat it as
		// a failure. Informational hint goes to stderr; stdout stays empty
		// so callers parsing list output get an unambiguous "no agents"
		// (zero lines) rather than a noise message they have to filter.
		fmt.Fprintln(os.Stderr, "(no agents registered — use `ahsir agent new <name>` to configure one)")
		return
	}

	// Plain text: one agent per line. Skills are comma-separated so the
	// output is easy to grep / awk from a shell or from Claude Code's Bash.
	for _, a := range agents {
		skills := make([]string, len(a.Skills))
		for i, s := range a.Skills {
			skills[i] = s.Name
		}
		fmt.Printf("%s\t%s\t[%s]\n", a.Name, a.URL, strings.Join(skills, ", "))
	}
}

// chatCmd: `ahsir chat <agent> "<message>" [--scheduler URL] [--context ID]`
func chatCmd(args []string) {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler base URL")
	contextID := fs.String("context", "", "ContextID for session reuse across calls (omit for isolated turns)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ahsir chat <agent> \"<message>\" [flags]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	rest := fs.Args()
	if len(rest) < 2 {
		fs.Usage()
		os.Exit(2)
	}
	agent := rest[0]
	// Join remaining args with spaces — lets the user write either
	// `ahsir chat foo "long message"` (shell preserves quotes) or
	// `ahsir chat foo this is also fine` (multiple tokens).
	message := strings.Join(rest[1:], " ")

	client := newSchedulerClient(*schedulerURL)
	reply, err := client.ChatWithAgent(agent, *contextID, message)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat failed: %v\n", err)
		os.Exit(1)
	}
	// Plain reply text on stdout — composable. If callers want metadata
	// (timing, task id, etc.) we can add a --verbose flag later.
	fmt.Println(reply)
}

// statusCmd: `ahsir status <agent> <task-id> [--scheduler URL]`
func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler base URL")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ahsir status <agent> <task-id> [flags]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	rest := fs.Args()
	if len(rest) != 2 {
		fs.Usage()
		os.Exit(2)
	}
	agent := rest[0]
	taskID := rest[1]

	client := newSchedulerClient(*schedulerURL)
	task, err := client.GetTaskStatus(agent, taskID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "status failed: %v\n", err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(task, "", "  ")
	fmt.Println(string(out))
}

// pingCmd: `ahsir ping [--scheduler URL]`. Cheap reachability check used
// by the skill/SKILL.md to decide "is the scheduler up before I try
// listing or chatting". Exit code is the contract: 0 = up, 2 = down.
func pingCmd(args []string) {
	fs := flag.NewFlagSet("ping", flag.ExitOnError)
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler base URL")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	client := newSchedulerClient(*schedulerURL)
	// RefreshTimeout hits /config/timeouts — a small, cheap endpoint that
	// returns 200 only when the scheduler is fully started (registry +
	// gateway ready). That makes it a reliable liveness probe.
	if _, err := client.RefreshTimeout(); err != nil {
		fmt.Fprintf(os.Stderr, "scheduler at %s unreachable: %v\n", *schedulerURL, err)
		os.Exit(2)
	}
	fmt.Println("ok")
}
