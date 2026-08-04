package main

// Observability subcommands: `ahsir doctor` and `ahsir trace`.
//
// Output conventions follow cli.go: human-readable plain text on stdout,
// composable with shell tools, --json escape hatch where structure matters.
//
//   - `doctor`: checklist of environment + scheduler health. Exit 0 when all
//     critical checks pass, 1 otherwise (warnings don't affect exit code).
//   - `trace`:  chronological view of the scheduler's invocation ledger for
//     one contextId (or a recent overview with no argument).

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wu8685/ahsir/internal/scheduler"
)

// doctorCmd: `ahsir doctor [--scheduler URL] [--config path]`
func doctorCmd(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler base URL")
	configFlag := fs.String("config", "", "Path to ahsir.yaml (for locating the admin-token file; default: auto-detect)")
	jsonOut := fs.Bool("json", false, "Output a machine-readable diagnostic report")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	failed := false
	report := doctorReport{}
	check := func(ok bool, critical bool, label, detail string) {
		report.Checks = append(report.Checks, doctorCheck{OK: ok, Critical: critical, Label: label, Detail: detail})
		mark := "✓"
		if !ok {
			if critical {
				mark = "✗"
				failed = true
			} else {
				mark = "⚠"
			}
		}
		if *jsonOut {
			return
		}
		if detail != "" {
			fmt.Printf("%s %-28s %s\n", mark, label, detail)
		} else {
			fmt.Printf("%s %s\n", mark, label)
		}
	}

	// 1. Config file. Missing is a warning, not an error — a scheduler can
	// be running off a config elsewhere; this check just points at the
	// conventional locations.
	cfgPath := *configFlag
	if cfgPath == "" {
		cfgPath = resolveDefaultConfig()
	}
	_, cfgErr := os.Stat(cfgPath)
	check(cfgErr == nil, false, "config file", cfgPath)

	// 2. Provider CLIs on PATH. Warnings: an agent fleet might only use one
	// provider, and doctor can't know which from here.
	for _, bin := range []string{"claude", "codex"} {
		path, err := exec.LookPath(bin)
		check(err == nil, false, bin+" binary", path)
	}

	// 3. Scheduler reachability — the one critical check.
	timeouts, err := httpGetJSON[map[string]string](*schedulerURL + "/config/timeouts")
	if err != nil {
		check(false, true, "scheduler", fmt.Sprintf("%s unreachable: %v", *schedulerURL, err))
		report.OK = false
		if *jsonOut {
			printDoctorJSON(report)
		}
		fmt.Fprintln(os.Stderr, "\nscheduler is down — start it with `ahsir start [config]`")
		os.Exit(1)
	}
	check(true, true, "scheduler", fmt.Sprintf("%s (chat timeout %s)", *schedulerURL, timeouts["chat"]))

	// Auth: report whether THIS CLI can find an admin token. We can't read
	// the scheduler's enforcement state over HTTP, but "CLI has a token" is
	// what determines whether control-plane commands (agent new/delete) will
	// work, so that's the actionable signal. Not critical — a token-less CLI
	// is fine for read/chat usage.
	if tok, source := scheduler.ReadAdminToken(cfgPath); tok != "" {
		check(true, false, "admin token", "found ("+source+")")
	} else {
		check(false, false, "admin token", "not found — `ahsir agent new/delete` will need AHSIR_ADMIN_TOKEN or a readable admin-token file beside "+cfgPath)
	}

	// 4. Scheduler-owned lifecycle state. Unlike registry heartbeat status,
	// this distinguishes healthy scale-to-zero from actual failures.
	agents, err := httpGetJSON[[]scheduler.AgentLifecycleSnapshot](*schedulerURL + "/diagnostics/agents")
	if err != nil {
		check(false, true, "agents", fmt.Sprintf("diagnostics failed: %v", err))
	} else if len(agents) == 0 {
		check(true, false, "agents", "none registered (use `ahsir agent new <name>`)")
	} else {
		report.Agents = agents
		if *jsonOut {
			for _, a := range agents {
				if a.Severity == scheduler.SeverityError {
					failed = true
				}
			}
		} else if renderAgentLifecycles(os.Stdout, agents) {
			failed = true
		}
	}

	report.OK = !failed
	if *jsonOut {
		printDoctorJSON(report)
	}
	if failed {
		os.Exit(1)
	}
}

type doctorCheck struct {
	OK       bool   `json:"ok"`
	Critical bool   `json:"critical"`
	Label    string `json:"label"`
	Detail   string `json:"detail,omitempty"`
}

type doctorReport struct {
	OK     bool                               `json:"ok"`
	Checks []doctorCheck                      `json:"checks"`
	Agents []scheduler.AgentLifecycleSnapshot `json:"agents"`
}

func printDoctorJSON(report doctorReport) {
	if report.Checks == nil {
		report.Checks = []doctorCheck{}
	}
	if report.Agents == nil {
		report.Agents = []scheduler.AgentLifecycleSnapshot{}
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
}

func renderAgentLifecycles(w io.Writer, agents []scheduler.AgentLifecycleSnapshot) bool {
	failed := false
	for _, agent := range agents {
		mark := "⚠"
		switch agent.Severity {
		case scheduler.SeverityOK:
			mark = "✓"
		case scheduler.SeverityInfo:
			mark = "○"
		case scheduler.SeverityError:
			mark = "✗"
			failed = true
		}
		detail := string(agent.State)
		if agent.Reason != "" {
			detail += ": " + agent.Reason
		}
		fmt.Fprintf(w, "%s %-28s %s\n", mark, "agent "+agent.Name, detail)
	}
	return failed
}

// traceCmd: `ahsir trace [contextId] [--scheduler URL] [--json]`
func traceCmd(args []string) {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler base URL")
	jsonOut := fs.Bool("json", false, "Output as JSON instead of plain text")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ahsir trace [contextId] [flags]\n")
		fs.PrintDefaults()
	}
	positionals := parsePositionals(fs, args)

	endpoint := *schedulerURL + "/invocations"
	if len(positionals) > 0 {
		endpoint += "?contextId=" + url.QueryEscape(positionals[0])
	}

	type invocation struct {
		ID         string `json:"id"`
		Source     string `json:"source"`
		AgentName  string `json:"agentName"`
		Method     string `json:"method"`
		ContextID  string `json:"contextId"`
		UserText   string `json:"userText"`
		Speaker    string `json:"speaker"`
		Status     string `json:"status"`
		StartedAt  string `json:"startedAt"`
		DurationMS int64  `json:"durationMs"`
		Error      string `json:"error"`
	}
	records, err := httpGetJSON[[]invocation](endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "trace failed: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		out, _ := json.MarshalIndent(records, "", "  ")
		fmt.Println(string(out))
		return
	}

	if len(records) == 0 {
		fmt.Fprintln(os.Stderr, "(no invocations recorded)")
		return
	}

	// One invocation per line, chronological (ledger order). Columns are
	// tab-separated so awk/cut work; the free-text tail (error or user-text
	// preview) goes last so the structured columns stay aligned.
	for _, rec := range records {
		ts := rec.StartedAt
		if t, err := time.Parse(time.RFC3339, rec.StartedAt); err == nil {
			ts = t.Format("01-02 15:04:05")
		}
		dur := "-"
		if rec.DurationMS > 0 {
			dur = (time.Duration(rec.DurationMS) * time.Millisecond).String()
		}
		tail := rec.UserText
		if rec.Error != "" {
			tail = "err: " + rec.Error
		}
		tail = strings.ReplaceAll(tail, "\n", " ")
		if len(tail) > 120 {
			tail = tail[:120] + "…"
		}
		ctx := rec.ContextID
		if ctx == "" {
			ctx = "-"
		}
		// Source column carries the speaker when present — "chat_gateway"
		// alone says how a turn arrived; "chat_gateway(alice)" says who sent
		// it (shared-context attribution).
		src := rec.Source
		if rec.Speaker != "" {
			src += "(" + rec.Speaker + ")"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", ts, rec.ID, src, rec.AgentName, rec.Method, ctx, rec.Status+"/"+dur, tail)
	}
}

// historyCmd: `ahsir history <agent> <contextId> [--scheduler URL] [--json]`
//
// Replays a shared context's transcript — who said what, what the agent
// answered, when. This is the takeover path for someone joining a context
// mid-conversation, and the documented fallback when an async taskId 404s
// after an agent restart.
func historyCmd(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler base URL")
	jsonOut := fs.Bool("json", false, "Output as JSON instead of plain text")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ahsir history <agent> <contextId> [flags]\n")
		fs.PrintDefaults()
	}
	positionals := parsePositionals(fs, args)
	if len(positionals) != 2 {
		fs.Usage()
		os.Exit(2)
	}
	agent, contextID := positionals[0], positionals[1]

	client := newSchedulerClient(*schedulerURL)
	turns, err := client.GetHistory(agent, contextID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "history failed: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		out, _ := json.MarshalIndent(turns, "", "  ")
		fmt.Println(string(out))
		return
	}
	if len(turns) == 0 {
		fmt.Fprintln(os.Stderr, "(no transcript for this context)")
		return
	}

	// One block per turn: a header line with turn number, time, speaker and
	// outcome, then the question and answer indented. Multi-line content is
	// kept verbatim — replay is the point, no truncation here.
	for _, turn := range turns {
		ts := turn.TS.Format("01-02 15:04:05")
		speaker := turn.Speaker
		if speaker == "" {
			speaker = "-"
		}
		outcome := turn.Status
		if turn.DurationMS > 0 {
			outcome += "/" + (time.Duration(turn.DurationMS) * time.Millisecond).String()
		}
		fmt.Printf("[%d] %s  %s → %s  (%s)\n", turn.Turn, ts, speaker, agent, outcome)
		fmt.Printf("    Q: %s\n", strings.ReplaceAll(turn.UserText, "\n", "\n       "))
		if turn.Error != "" {
			fmt.Printf("    E: %s\n", turn.Error)
		} else {
			fmt.Printf("    A: %s\n", strings.ReplaceAll(turn.Reply, "\n", "\n       "))
		}
	}
}

// httpGetJSON fetches a URL and decodes the JSON response into T. Small
// local helper — doctor/trace read endpoints the schedulerclient doesn't
// wrap (and shouldn't need to: these are CLI-only views).
func httpGetJSON[T any](rawURL string) (T, error) {
	var zero T
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return zero, fmt.Errorf("GET %s: status %d: %s", rawURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, fmt.Errorf("GET %s: decode: %w", rawURL, err)
	}
	return out, nil
}
