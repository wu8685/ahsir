package main

// CLI subcommands for dynamic persona management.
//
//	ahsir agent new <name> [flags]    scaffold + register + spawn a new agent
//	ahsir agent delete <name>         tear down a running agent (files preserved)
//	ahsir agent list-configs          show locally-configured agents (vs `ahsir list`
//	                                  which shows running)
//
// `agent new` writes both an agent-card.yaml workspace AND appends to
// ahsir.yaml — so the agent survives a scheduler restart — then hits the
// scheduler admin API at POST /admin/agents to bring it up immediately
// without restarting. The two halves are intentional: the disk write
// keeps state durable; the API call avoids restart friction.
//
// `agent delete` only stops the process and removes from ahsir.yaml.
// Workspace files stay on disk so users don't lose their persona by
// accident — to fully remove, they `rm -rf <workspace>`.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wu8685/ahsir/internal/scheduler"
)

const (
	defaultProviderBaseURL = "https://api.deepseek.com/anthropic"
	defaultProviderModel   = "deepseek-v4-pro"
	defaultProviderAPIKey  = "${MODEL_API_KEY}"
)

// Default file layout when neither --config nor --workspace is given.
// Putting state under ~/.ahsir keeps agent files OUT of the user's
// current directory — running `ahsir agent new` from a git repo
// shouldn't drop generated yaml + workspace dirs into that repo.
//
//	~/.ahsir/
//	├── ahsir.yaml                         # scheduler config (auto-created)
//	└── agents/
//	    └── <name>/.a2a/agent-card.yaml    # per-agent workspace
//
// Users with project-scoped configs (e.g. example/multi-agent/ahsir.yaml)
// pass --config explicitly.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Pathological case (no $HOME): fall back to CWD so commands still
		// run, even though they'll drop files in the local directory.
		return "ahsir.yaml"
	}
	return filepath.Join(home, ".ahsir", "ahsir.yaml")
}

func defaultWorkspaceDir(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("workspaces", name)
	}
	return filepath.Join(home, ".ahsir", "agents", name)
}

// repeatStringFlag captures a flag that may be specified multiple times,
// accumulating its values into a slice. Used for --skill / --allow-fs.
type repeatStringFlag struct {
	values []string
}

func (r *repeatStringFlag) String() string     { return strings.Join(r.values, ",") }
func (r *repeatStringFlag) Set(s string) error { r.values = append(r.values, s); return nil }

// agentCmd routes the `ahsir agent <subcommand>` family. Kept as a
// single dispatcher so the top-level main.go stays small.
func agentCmd(args []string) {
	if len(args) == 0 {
		agentUsage()
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "new":
		agentNewCmd(rest)
	case "delete":
		agentDeleteCmd(rest)
	case "list-configs":
		agentListConfigsCmd(rest)
	default:
		fmt.Fprintf(os.Stderr, "ahsir agent: unknown subcommand %q\n", sub)
		agentUsage()
		os.Exit(2)
	}
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, "Usage: ahsir agent <subcommand> [flags]")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  new <name>          Scaffold an agent and bring it up immediately")
	fmt.Fprintln(os.Stderr, "  delete <name>       Stop a running agent (workspace files preserved)")
	fmt.Fprintln(os.Stderr, "  list-configs        Show agents locally configured in ahsir.yaml")
}

// agentNewCmd: `ahsir agent new <name> [flags]`
func agentNewCmd(args []string) {
	// The name is positional and must come first ("agent new SECURITY-REVIEWER
	// --prompt ..."). Go's flag package stops parsing at the first non-flag,
	// so a naive fs.Parse(args) interprets the name as terminating the flag
	// list and leaves all subsequent --flags unparsed. Pull the name out
	// up-front, then let fs.Parse handle the rest as pure flags.
	name, flagArgs, err := extractPositionalName(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Usage: ahsir agent new <name> [flags]")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("agent new", flag.ExitOnError)

	cfgPath := fs.String("config", defaultConfigPath(), "Path to ahsir.yaml (created if missing under ~/.ahsir/)")
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler admin URL")
	prompt := fs.String("prompt", "", "System prompt (the persona's instructions)")
	model := fs.String("model", defaultProviderModel, "LLM model identifier")
	provider := fs.String("provider", "deepseek", "Provider (anthropic|zhipu|deepseek|codex)")
	baseURL := fs.String("base-url", defaultProviderBaseURL, "Provider base URL")
	apiKeyEnv := fs.String("api-key-env", defaultProviderAPIKey, "API key (literal or ${ENV_VAR})")
	timeoutDur := fs.String("timeout", "300s", "Per-LLM-call timeout")
	wsOverride := fs.String("workspace", "", "Workspace directory (default: ~/.ahsir/agents/<name>)")
	skipStart := fs.Bool("skip-start", false, "Only scaffold files; don't ask the scheduler to start the agent")
	poolMaxActive := fs.Int("pool-max-active", 0, "Cap on concurrent live claude processes for this agent (0 = unlimited)")
	poolOverload := fs.String("pool-overload-policy", "", "Behaviour when pool is at capacity: reject (default) or evict-lru")

	var skills repeatStringFlag
	var allowFS repeatStringFlag
	fs.Var(&skills, "skill", "Skill in `name=description` form (repeatable)")
	fs.Var(&allowFS, "allow-fs", "Allow filesystem access to a path (repeatable; enables fs tools)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: ahsir agent new <name> [flags]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(flagArgs); err != nil {
		os.Exit(2)
	}
	if name == "" {
		fatal("agent new: name must be non-empty")
	}

	cfgAbs, err := filepath.Abs(*cfgPath)
	if err != nil {
		fatal("resolve --config: %v", err)
	}

	workspace := *wsOverride
	if workspace == "" {
		workspace = defaultWorkspaceDir(name)
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		fatal("resolve workspace: %v", err)
	}

	// Bootstrap ~/.ahsir/ahsir.yaml on first use so the user (or Claude)
	// doesn't have to manually scaffold it before running `agent new`.
	// No-op if the file already exists.
	if err := ensureConfigExists(cfgAbs); err != nil {
		fatal("init %s: %v", cfgAbs, err)
	}

	// Parse --skill name=desc pairs once before any side-effects so a
	// formatting bug rejects the whole command rather than half-writing
	// and bailing.
	skillConfigs, err := parseSkillFlags(skills.values)
	if err != nil {
		fatal("agent new: %v", err)
	}

	// 1. Scaffold workspace + agent-card.yaml.
	if err := writeAgentCard(workspace, agentCardScaffold{
		Name:               name,
		SystemPrompt:       *prompt,
		Skills:             skillConfigs,
		Provider:           *provider,
		BaseURL:            *baseURL,
		APIKey:             *apiKeyEnv,
		Model:              *model,
		Timeout:            *timeoutDur,
		FSEnabled:          len(allowFS.values) > 0,
		FSPaths:            allowFS.values,
		PoolMaxActive:      *poolMaxActive,
		PoolOverloadPolicy: *poolOverload,
	}); err != nil {
		fatal("write agent card: %v", err)
	}
	fmt.Fprintf(os.Stderr, "✓ scaffold: %s\n", filepath.Join(workspace, ".a2a", "agent-card.yaml"))

	// 2. Append to ahsir.yaml (idempotent on duplicate name).
	if err := scheduler.AddAgentToConfig(cfgAbs, name, workspace, 0); err != nil {
		fatal("update %s: %v", cfgAbs, err)
	}
	fmt.Fprintf(os.Stderr, "✓ registered: %s\n", cfgAbs)

	// 3. Ask the running scheduler to spin it up.
	if *skipStart {
		fmt.Fprintln(os.Stderr, "(--skip-start) restart the scheduler to activate")
		return
	}
	port, err := requestStart(*schedulerURL, name, workspace)
	if err != nil {
		if errors.Is(err, errSchedulerDown) {
			fmt.Fprintf(os.Stderr, "✗ scheduler at %s not reachable — files written; start scheduler with `ahsir start %s` to activate\n", *schedulerURL, cfgAbs)
			return
		}
		fatal("start agent via admin API: %v", err)
	}
	fmt.Fprintf(os.Stderr, "✓ running on port %d\n", port)
	fmt.Printf("%s\n", name)
}

// agentDeleteCmd: `ahsir agent delete <name>`
func agentDeleteCmd(args []string) {
	name, flagArgs, err := extractPositionalName(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Usage: ahsir agent delete <name> [flags]")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("agent delete", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "Path to ahsir.yaml")
	schedulerURL := fs.String("scheduler", defaultSchedulerURL, "Scheduler admin URL")
	if err := fs.Parse(flagArgs); err != nil {
		os.Exit(2)
	}

	if err := requestStop(*schedulerURL, name); err != nil && !errors.Is(err, errSchedulerDown) {
		fmt.Fprintf(os.Stderr, "warn: scheduler stop request failed: %v\n", err)
	}

	cfgAbs, _ := filepath.Abs(*cfgPath)
	if err := scheduler.RemoveAgentFromConfig(cfgAbs, name); err != nil {
		fatal("remove from %s: %v", cfgAbs, err)
	}
	fmt.Fprintf(os.Stderr, "✓ %s offline (workspace files preserved)\n", name)
}

// agentListConfigsCmd: `ahsir agent list-configs` shows what ahsir.yaml
// has registered, regardless of whether the agent is currently running.
// Distinct from `ahsir list`, which only sees what's registered with the
// running scheduler's registry.
func agentListConfigsCmd(args []string) {
	fs := flag.NewFlagSet("agent list-configs", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "Path to ahsir.yaml")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg, err := scheduler.LoadConfig(*cfgPath)
	if err != nil {
		fatal("load %s: %v", *cfgPath, err)
	}
	if len(cfg.Agents) == 0 {
		fmt.Fprintln(os.Stderr, "no agents configured")
		return
	}
	for _, a := range cfg.Agents {
		fmt.Printf("%s\t%s\n", a.Name, a.Workspace)
	}
}

// --- helpers ---

// ensureConfigExists creates a minimal ahsir.yaml at path if it doesn't
// exist yet. Used to bootstrap ~/.ahsir/ahsir.yaml on first `agent new`
// so the user doesn't have to scaffold the scheduler config manually.
// No-op when the file is already there.
//
// The template matches the example/multi-agent/ahsir.yaml shape: empty
// agents list, registry on the default localhost port, sensible
// timeouts, port range. `ahsir start <this-path>` works out of the box
// against this default — no further edits needed.
func ensureConfigExists(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}
	const tmpl = `agents: []

registry:
  host: "127.0.0.1"
  port: 9800
  heartbeat_interval: 10s
  heartbeat_timeout: 30s

mcp: {}

timeouts:
  chat: 10m
  task_status: 30s

port_range:
  start: 9801
  end: 9900
`
	return os.WriteFile(path, []byte(tmpl), 0o644)
}

// extractPositionalName pulls the first non-flag-looking arg off the
// front of args so flag.Parse can handle the rest as pure flags. Without
// this, a positional name before flags causes Go's flag package to stop
// parsing prematurely.
//
// Returns (name, remaining-args, error). An empty args slice or one
// starting with "-" is an error.
func extractPositionalName(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("name is required")
	}
	if strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("name must come before flags, got %q", args[0])
	}
	return args[0], args[1:], nil
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ahsir: "+format+"\n", args...)
	os.Exit(1)
}

// parseSkillFlags splits each `name=description` pair. Empty descriptions
// are allowed (an agent can have skills without descriptive blurbs); an
// empty name is not.
func parseSkillFlags(raws []string) ([]skillPair, error) {
	out := make([]skillPair, 0, len(raws))
	for _, raw := range raws {
		eq := strings.IndexByte(raw, '=')
		var name, desc string
		if eq < 0 {
			name = raw
		} else {
			name = raw[:eq]
			desc = raw[eq+1:]
		}
		if name == "" {
			return nil, fmt.Errorf("--skill: empty name in %q", raw)
		}
		out = append(out, skillPair{Name: name, Description: desc})
	}
	return out, nil
}

type skillPair struct{ Name, Description string }

// agentCardScaffold is the field set writeAgentCard needs to emit a
// minimal-but-complete agent-card.yaml that the wrapper's card.go loader
// will accept.
type agentCardScaffold struct {
	Name               string
	SystemPrompt       string
	Skills             []skillPair
	Provider           string
	BaseURL            string
	APIKey             string
	Model              string
	Timeout            string
	FSEnabled          bool
	FSPaths            []string
	PoolMaxActive      int
	PoolOverloadPolicy string
}

// agentCardYAMLShape mirrors the fields wrapper.AgentCardConfig consumes.
// Kept as a local type (rather than importing AgentCardConfig) so the
// CLI doesn't depend on the wrapper package — the wrapper package depends
// on the scheduler binary's existence indirectly already, this avoids a
// circular-ish import.
type agentCardYAMLShape struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Version     string      `yaml:"version"`
	Provider    yamlMap     `yaml:"provider"`
	Skills      []skillPair `yaml:"skills"`
	Claude      yamlMap     `yaml:"claude"`
	Runtime     yamlMap     `yaml:"runtime"`
	Filesystem  yamlMap     `yaml:"filesystem"`
	Network     yamlMap     `yaml:"network"`
	Pool        yamlMap     `yaml:"pool,omitempty"`
}

// yamlMap is an alias for map[string]any used so each section can be
// emitted as a sub-mapping without declaring N tiny structs.
type yamlMap map[string]any

func writeAgentCard(workspace string, s agentCardScaffold) error {
	dir := filepath.Join(workspace, ".a2a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	poolSection := yamlMap{}
	if s.PoolMaxActive > 0 {
		poolSection["max_active"] = s.PoolMaxActive
	}
	if s.PoolOverloadPolicy != "" {
		poolSection["overload_policy"] = s.PoolOverloadPolicy
	}
	command := "claude"
	baseURL := s.BaseURL
	if strings.EqualFold(s.Provider, "codex") {
		command = "codex"
		if baseURL == defaultProviderBaseURL {
			baseURL = ""
		}
		if s.Model == defaultProviderModel {
			s.Model = ""
		}
		if s.APIKey == defaultProviderAPIKey {
			s.APIKey = ""
		}
	}

	card := agentCardYAMLShape{
		Name:        s.Name,
		Description: fmt.Sprintf("Agent %q configured via `ahsir agent new`.", s.Name),
		Version:     "1.0.0",
		Provider:    yamlMap{"name": "ahsir", "url": "https://github.com/wu8685/ahsir"},
		Skills:      s.Skills,
		Claude: yamlMap{
			"systemPrompt":  s.SystemPrompt,
			"maxAgentCalls": 0,
		},
		Runtime: yamlMap{
			"command":  command,
			"args":     []string{},
			"timeout":  s.Timeout,
			"provider": s.Provider,
			"baseURL":  baseURL,
			"apiKey":   s.APIKey,
			"model":    s.Model,
		},
		Filesystem: yamlMap{
			"enabled":       s.FSEnabled,
			"allowed_paths": s.FSPaths,
		},
		Network: yamlMap{"bind": "127.0.0.1"},
		Pool:    poolSection,
	}

	out := &bytes.Buffer{}
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	if err := enc.Encode(card); err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("close encoder: %w", err)
	}

	path := filepath.Join(dir, "agent-card.yaml")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// --- admin API client ---

var errSchedulerDown = errors.New("scheduler not reachable")

func requestStart(schedulerURL, name, workspace string) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"name":      name,
		"workspace": workspace,
	})
	resp, err := http.Post(schedulerURL+"/admin/agents", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", errSchedulerDown, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated {
		var out struct {
			Port int `json:"port"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return 0, fmt.Errorf("decode start response: %w", err)
		}
		return out.Port, nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return 0, fmt.Errorf("scheduler %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

func requestStop(schedulerURL, name string) error {
	req, _ := http.NewRequest(http.MethodDelete, schedulerURL+"/admin/agents/"+name, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errSchedulerDown, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("scheduler %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}
