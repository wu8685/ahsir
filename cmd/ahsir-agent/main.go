package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wu8685/ahsir/internal/wrapper"
)

func main() {
	workspace := flag.String("workspace", "", "Workspace directory (holds .a2a/ card+sessions+transcripts; must be unique per agent)")
	workdir := flag.String("workdir", "", "Working directory / cwd for the LLM CLI (default: workspace). Several agents may share one workdir while keeping distinct workspaces.")
	port := flag.Int("port", 0, "Listen port")
	registry := flag.String("registry", "", "Registry URL")
	internalToken := flag.String("internal-token", "", "Internal scheduler-to-agent A2A token")
	adminToken := flag.String("admin-token", "", "Control-plane token for authenticating the registry heartbeat")
	flag.Parse()

	if *workspace == "" {
		fmt.Fprintf(os.Stderr, "Usage: ahsir-agent --workspace=<path> [--workdir=<path>] [--port=<port>] [--registry=<url>]\n")
		os.Exit(1)
	}

	// The working directory (cwd + base for relative allowed_paths) defaults to
	// the workspace. Pointing it elsewhere lets multiple agents share one cwd
	// while each keeps a private workspace for its .a2a/ state.
	effectiveWorkdir := *workdir
	if effectiveWorkdir == "" {
		effectiveWorkdir = *workspace
	}

	// Load agent card
	builder := wrapper.NewAgentCardBuilder(*workspace)
	cfg, err := builder.Load()
	if err != nil {
		log.Fatalf("Failed to load agent card: %v", err)
	}

	runtimeCard := builder.BuildRuntime(cfg, *port)

	// Resolve runtime config up-front so configuration mistakes (e.g. an
	// unset ${MODEL_API_KEY}) fail before we bind the listening port and
	// before any peer agent can hit a half-initialised endpoint.
	var sessionCfg wrapper.SessionConfig
	if *registry != "" {
		var err error
		sessionCfg, err = buildSessionConfig(cfg.Name, cfg.Runtime, cfg.Filesystem, cfg.MCP, cfg.Streaming, *workspace, effectiveWorkdir)
		if err != nil {
			log.Fatalf("Invalid runtime config for agent %q: %v", cfg.Name, err)
		}
	}

	wrapperCfg := wrapper.AgentWrapperConfig{
		Port:          *port,
		RegistryURL:   *registry,
		AgentCard:     runtimeCard,
		InternalToken: *internalToken,
		AdminToken:    *adminToken,
	}

	w := wrapper.NewAgentWrapper(wrapperCfg)
	// Per-context transcript (full turn content, 0600) lives in the workspace
	// next to sessions.json; backs the /history endpoint and `ahsir history`.
	transcripts := wrapper.NewTranscriptStore(*workspace)
	// Prune transcripts of contexts inactive past the retention window, once at
	// startup (mirrors the scheduler ledger's compaction; running agents never
	// prune mid-session).
	if n, err := transcripts.CompactForRetention(time.Now()); err != nil {
		log.Printf("transcript retention compaction failed: %v", err)
	} else if n > 0 {
		log.Printf("transcript retention: pruned %d inactive context(s) (>30d since last turn)", n)
	}
	w.SetTranscriptStore(transcripts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		log.Fatalf("Failed to start agent wrapper: %v", err)
	}
	if *registry != "" {
		startRegistryMonitor(ctx, defaultRegistryMonitorConfig(*registry), func() {
			log.Printf("Registry monitor exceeded failure grace; shutting down agent %s", runtimeCard.Name)
			cancel()
		})
	}

	log.Printf("Agent %s listening on port %d", runtimeCard.Name, *port)

	// Setup executor if registry URL is configured
	if *registry != "" {
		listAgents := wrapper.RegistryAgentLister(*registry)
		callAgent := wrapper.RegistryAgentCaller(*registry)
		maxCalls := cfg.Claude.MaxAgentCalls
		basePrompt := cfg.Claude.SystemPrompt

		retention, err := poolRetentionConfig(cfg.Pool)
		if err != nil {
			log.Fatalf("agent %q: %v", cfg.Name, err)
		}

		// Session per A2A contextID, pooled with sliding idle TTL.
		// Claude uses one long-running stream-json subprocess; Codex forks
		// `codex exec --json` per turn and resumes by thread_id.
		factory := func(ctx context.Context, contextID, resumeID string) (wrapper.Session, error) {
			if sessionCfg.Provider == wrapper.ProviderCodex {
				return wrapper.NewCodexSession(ctx, sessionCfg, resumeID)
			}
			return wrapper.NewClaudeSession(ctx, sessionCfg, resumeID)
		}
		// Persist contextID → sessionID mappings so a restart of this agent
		// process can `--resume` prior conversations instead of starting
		// fresh. File lives in the workspace next to agent-card.yaml; a
		// corrupt or missing file falls back to "no prior state" so the
		// agent always boots.
		persistPath := filepath.Join(*workspace, ".a2a", "sessions.json")
		persist := wrapper.NewFilePersistence(persistPath)
		pool := wrapper.NewSessionPoolWithPersistence(factory, retention.idleTTL, retention.evictedTTL, persist)
		pool.SetMaxEvicted(retention.maxEvicted)
		defer pool.Stop()

		// Apply pool capacity from agent-card.yaml's `pool:` block if set.
		// Default (max_active=0) keeps the pool unbounded — historical
		// behaviour. Set max_active to cap concurrent live claude
		// subprocesses for this agent; overload_policy decides whether to
		// reject new requests or evict the LRU when at cap.
		if cfg.Pool.MaxActive > 0 {
			policy, err := wrapper.ParseOverloadPolicy(cfg.Pool.OverloadPolicy)
			if err != nil {
				log.Fatalf("agent %q: %v", cfg.Name, err)
			}
			pool.SetCap(cfg.Pool.MaxActive, policy)
			log.Printf("Pool cap: max_active=%d policy=%s", cfg.Pool.MaxActive, policy)
		}

		// Per-context turn queue depth. Unset keeps the pool default;
		// explicit 0 restores the pre-queue fail-fast busy contract.
		if cfg.Pool.QueueDepth != nil {
			pool.SetQueueDepth(*cfg.Pool.QueueDepth)
			log.Printf("Pool queue: queue_depth=%d", *cfg.Pool.QueueDepth)
		}

		w.SetupExecutor(pool.LookupOrCreate, listAgents, callAgent, maxCalls, basePrompt)
		log.Printf("Executor wired: %s SessionPool (%s %v, timeout=%s, persist=%s, idle_ttl=%s, evicted_ttl=%s, max_evicted=%d)", sessionCfg.Provider, sessionCfg.Command, sessionCfg.Args, sessionCfg.Timeout, persistPath, retention.idleTTL, retention.evictedTTL, retention.maxEvicted)
	}

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigCh:
	case <-ctx.Done():
	}

	log.Println("Shutting down...")
	w.Stop(ctx)
}

type registryMonitorConfig struct {
	URL          string
	Interval     time.Duration
	Timeout      time.Duration
	FailureGrace time.Duration
}

func defaultRegistryMonitorConfig(url string) registryMonitorConfig {
	return registryMonitorConfig{
		URL:          url,
		Interval:     5 * time.Second,
		Timeout:      2 * time.Second,
		FailureGrace: 30 * time.Second,
	}
}

func startRegistryMonitor(ctx context.Context, cfg registryMonitorConfig, onFailure func()) {
	if strings.TrimSpace(cfg.URL) == "" || onFailure == nil {
		return
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.FailureGrace <= 0 {
		cfg.FailureGrace = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		var firstFailure time.Time
		for {
			if registryReachable(ctx, cfg.URL, cfg.Timeout) {
				firstFailure = time.Time{}
			} else {
				now := time.Now()
				if firstFailure.IsZero() {
					firstFailure = now
				}
				if now.Sub(firstFailure) >= cfg.FailureGrace {
					onFailure()
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func registryReachable(parent context.Context, registryURL string, timeout time.Duration) bool {
	url := strings.TrimRight(registryURL, "/") + "/agents"
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

type poolRetention struct {
	idleTTL    time.Duration
	evictedTTL time.Duration
	maxEvicted int
}

func poolRetentionConfig(cfg wrapper.PoolConfig) (poolRetention, error) {
	ret := poolRetention{
		idleTTL:    30 * time.Minute,
		evictedTTL: 30 * 24 * time.Hour,
		maxEvicted: 1000,
	}
	if cfg.IdleTTL != "" {
		d, err := parsePoolDuration(cfg.IdleTTL)
		if err != nil {
			return poolRetention{}, fmt.Errorf("pool.idle_ttl %q: %w", cfg.IdleTTL, err)
		}
		ret.idleTTL = d
	}
	if cfg.EvictedTTL != "" {
		d, err := parsePoolDuration(cfg.EvictedTTL)
		if err != nil {
			return poolRetention{}, fmt.Errorf("pool.evicted_ttl %q: %w", cfg.EvictedTTL, err)
		}
		ret.evictedTTL = d
	}
	if cfg.MaxEvicted > 0 {
		ret.maxEvicted = cfg.MaxEvicted
	}
	return ret, nil
}

func parsePoolDuration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// buildSessionConfig translates a card RuntimeConfig + FilesystemConfig into a
// SessionConfig.
//
// Filesystem access is granted to the underlying CLI (e.g. `claude -p`) by
// emitting `--add-dir=<abs-path>` per entry in fs.AllowedPaths and a
// read-only `--allowedTools=...` whitelist. This relies on Claude Code's
// built-in Read/LS/Glob/Grep tools — no custom tool server is involved.
//
// Convention: when adding any new flag to args here, ALWAYS use the
// `--flag=value` form, never `--flag value`. Several Claude Code flags
// (--add-dir, --allowedTools, ...) are variadic and the space-separated
// form will greedily eat the next token. The prompt itself is fed via
// stdin (see SessionManager.Send), so a runaway variadic flag can no
// longer swallow it — but it can still eat *other* flag values and
// produce confusing behaviour. Stick to `=` form to stay safe across
// future Claude Code versions.
//
// Provider-derived env (ANTHROPIC_BASE_URL etc.) and explicit Env entries are
// merged on top of the parent process env so the LLM CLI inherits PATH/HOME
// /login credentials unless explicitly overridden.
// workspace holds the agent's private .a2a/ state (codex-home etc.); workdir is
// the CLI cwd and the base for relative allowed_paths. workdir == workspace
// unless the operator decoupled them to share a cwd across agents.
func buildSessionConfig(name string, rt wrapper.RuntimeConfig, fs wrapper.FilesystemConfig, mcp wrapper.MCPConfig, stream wrapper.StreamingConfig, workspace, workdir string) (wrapper.SessionConfig, error) {
	timeout := 120 * time.Second
	if rt.Timeout != "" {
		d, err := time.ParseDuration(rt.Timeout)
		if err != nil {
			return wrapper.SessionConfig{}, fmt.Errorf("runtime.timeout %q: %w", rt.Timeout, err)
		}
		timeout = d
	}

	extra, err := wrapper.ResolveProviderEnv(rt)
	if err != nil {
		return wrapper.SessionConfig{}, err
	}
	model, err := wrapper.ResolveRuntimeModel(rt)
	if err != nil {
		return wrapper.SessionConfig{}, err
	}
	provider := wrapper.RuntimeProvider(rt)
	if provider == wrapper.ProviderCodex {
		if extra == nil {
			extra = make(map[string]string)
		}
		if _, ok := extra["CODEX_HOME"]; !ok {
			codexHome := filepath.Join(workspace, ".a2a", "codex-home")
			if err := os.MkdirAll(codexHome, 0o700); err != nil {
				return wrapper.SessionConfig{}, fmt.Errorf("create isolated CODEX_HOME %q: %w", codexHome, err)
			}
			if err := mirrorCodexHomeForAgent(codexHome); err != nil {
				return wrapper.SessionConfig{}, err
			}
			extra["CODEX_HOME"] = codexHome
		}
	}

	var env []string
	if len(extra) > 0 {
		env = mergeEnv(os.Environ(), extra)
	}

	args := append([]string(nil), rt.Args...)
	if fs.Enabled {
		for _, p := range fs.AllowedPaths {
			abs := p
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(workdir, p)
			}
			// Use --add-dir=<path> form (one path per flag). Both claude and
			// codex accept it, and claude's --add-dir is variadic — the
			// space-separated form would greedily consume trailing values.
			args = append(args, "--add-dir="+abs)
		}
		// Also allow-list the web console's file-drop dir, so a file dragged
		// into the composer (which `ahsir ui` copies there) is readable without
		// the operator hand-editing allowed_paths. The console resolves the same
		// dir, so this matches with zero config; override both with
		// AHSIR_UPLOAD_DIR. Gated on fs.Enabled — without read tools the path
		// would be useless anyway.
		args = append(args, "--add-dir="+wrapper.ResolveUploadDir())
		if provider != wrapper.ProviderCodex {
			// Default to a read-only whitelist: model can inspect files but not
			// modify them or run arbitrary shell. write_access widens this to the
			// built-in edit/write tools without granting bash; shell_access is the
			// separate, explicit opt-in that adds Bash (arbitrary command
			// execution). Both are owned by the card schema, not raw CLI args —
			// stripClaudePermissionBypassArgs blocks the --dangerously-skip-permissions
			// escalation vector precisely so widening must come through here.
			// --allowedTools is variadic too — same trap as --add-dir; use the
			// = form so it cannot consume the trailing prompt positional.
			allowedTools := "Read,LS,Glob,Grep"
			if fs.WriteAccess {
				allowedTools = "Read,LS,Glob,Grep,Edit,MultiEdit,Write"
			}
			if fs.ShellAccess {
				allowedTools += ",Bash"
			}
			args = append(args, "--allowedTools="+allowedTools)
		}
	}
	if provider == wrapper.ProviderCodex {
		if model != "" && !hasFlag(args, "--model", "-m") {
			args = append(args, "--model="+model)
		}
		// Codex configures MCP servers through its own config.toml under the
		// isolated CODEX_HOME, not through this card field. Fail loudly rather
		// than silently ignore so the operator isn't misled into thinking the
		// declared servers are active.
		if len(mcp.Servers) > 0 {
			return wrapper.SessionConfig{}, fmt.Errorf("mcp.servers is only supported for claude-backed agents; codex configures MCP via its CODEX_HOME config.toml")
		}
	}
	if provider != wrapper.ProviderCodex {
		// Isolate the agent's claude from the operator's global (~/.claude.json)
		// and project (.mcp.json) MCP configuration. --strict-mcp-config makes
		// claude load ONLY servers passed via --mcp-config — with none passed,
		// that's zero servers. This is deliberate: an agent persona must not
		// silently inherit whatever MCP servers happen to be configured on the
		// host (measured to add tens of thousands of input tokens + seconds of
		// per-call init on a trivial turn, and a privilege-bleed risk). Opt back
		// in per-agent via the card's `mcp.servers`.
		args = append(args, "--strict-mcp-config")
		if len(mcp.Servers) > 0 {
			raw, err := json.Marshal(map[string]any{"mcpServers": mcp.Servers})
			if err != nil {
				return wrapper.SessionConfig{}, fmt.Errorf("marshal mcp.servers: %w", err)
			}
			// `--flag=value` form (see the convention note above): --mcp-config
			// accepts an inline JSON document, so we don't need a temp file.
			args = append(args, "--mcp-config="+string(raw))
		}
	}
	if provider != wrapper.ProviderCodex && stream.PartialMessages {
		// Tell claude to interleave incremental content_block_delta lines into
		// the NDJSON stream so subscribers can see assistant text as it is
		// produced. The wrapper surfaces these as EventTextDelta and the A2A
		// server forwards them as TaskStatusUpdateEvent on message/stream.
		args = append(args, "--include-partial-messages")
	}

	return wrapper.SessionConfig{
		Name:     name,
		Provider: provider,
		Command:  rt.Command,
		Args:     args,
		Env:      env,
		WorkDir:  workdir,
		Timeout:  timeout,
	}, nil
}

func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			if _, replace := extra[key]; replace {
				continue
			}
		}
		out = append(out, item)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func mirrorCodexHomeForAgent(dst string) error {
	src := sourceCodexHome()
	if src == "" || samePath(src, dst) {
		return nil
	}
	if err := copyFileIfExists(filepath.Join(src, "auth.json"), filepath.Join(dst, "auth.json"), 0o600); err != nil {
		return fmt.Errorf("mirror codex auth: %w", err)
	}
	if err := copyFilteredCodexConfig(filepath.Join(src, "config.toml"), filepath.Join(dst, "config.toml")); err != nil {
		return fmt.Errorf("mirror codex config: %w", err)
	}
	return nil
}

func sourceCodexHome() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func samePath(a, b string) bool {
	aa, err := filepath.Abs(filepath.Clean(a))
	if err == nil {
		a = aa
	}
	bb, err := filepath.Abs(filepath.Clean(b))
	if err == nil {
		b = bb
	}
	return a == b
}

func copyFileIfExists(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

func copyFilteredCodexConfig(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "notify") || strings.HasPrefix(trimmed, "CODEX_HOME") {
			continue
		}
		kept = append(kept, line)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(strings.Join(kept, "\n")), 0o600)
}

func hasFlag(args []string, long, short string) bool {
	for i, a := range args {
		if a == long || a == short {
			return i+1 < len(args)
		}
		if strings.HasPrefix(a, long+"=") || strings.HasPrefix(a, short+"=") {
			return true
		}
	}
	return false
}
