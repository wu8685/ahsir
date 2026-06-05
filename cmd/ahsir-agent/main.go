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
	"time"

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

	// Resolve runtime config up-front so configuration mistakes (e.g. an
	// unset ${MODEL_API_KEY}) fail before we bind the listening port and
	// before any peer agent can hit a half-initialised endpoint.
	var sessionCfg wrapper.SessionConfig
	if *registry != "" {
		var err error
		sessionCfg, err = buildSessionConfig(cfg.Name, cfg.Runtime, cfg.Filesystem, *workspace)
		if err != nil {
			log.Fatalf("Invalid runtime config for agent %q: %v", cfg.Name, err)
		}
	}

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

	// Setup executor if registry URL is configured
	if *registry != "" {
		listAgents := wrapper.RegistryAgentLister(*registry)
		callAgent := wrapper.RegistryAgentCaller(*registry)
		maxCalls := cfg.Claude.MaxAgentCalls
		basePrompt := cfg.Claude.SystemPrompt

		// Long-running stream-json claude session per A2A contextID, pooled
		// with sliding 30-minute idle TTL. Evicted entries keep their
		// sessionID for 24h so a returning conversation can `--resume`.
		factory := func(ctx context.Context, contextID, resumeID string) (wrapper.Session, error) {
			return wrapper.NewClaudeSession(ctx, sessionCfg, resumeID)
		}
		// Persist contextID → sessionID mappings so a restart of this agent
		// process can `--resume` prior conversations instead of starting
		// fresh. File lives in the workspace next to agent-card.yaml; a
		// corrupt or missing file falls back to "no prior state" so the
		// agent always boots.
		persistPath := filepath.Join(*workspace, ".a2a", "sessions.json")
		persist := wrapper.NewFilePersistence(persistPath)
		pool := wrapper.NewSessionPoolWithPersistence(factory, 30*time.Minute, 24*time.Hour, persist)
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

		w.SetupExecutor(pool.LookupOrCreate, listAgents, callAgent, maxCalls, basePrompt)
		log.Printf("Executor wired: stream-json SessionPool (%s %v, timeout=%s, persist=%s)", sessionCfg.Command, sessionCfg.Args, sessionCfg.Timeout, persistPath)
	}

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	w.Stop(ctx)
}

// buildSessionConfig translates a card RuntimeConfig + FilesystemConfig into a
// SessionConfig.
//
// Filesystem access is granted to the underlying CLI (e.g. `claude -p`) by
// emitting `--add-dir=<abs-path>` per entry in fs.AllowedPaths and a
// read-only `--allowedTools=...` whitelist. This relies on Claude Code's
// built-in Read/LS/Glob/Grep tools — no custom MCP server is involved.
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
func buildSessionConfig(name string, rt wrapper.RuntimeConfig, fs wrapper.FilesystemConfig, workspace string) (wrapper.SessionConfig, error) {
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

	var env []string
	if len(extra) > 0 {
		env = append(env, os.Environ()...)
		for k, v := range extra {
			env = append(env, k+"="+v)
		}
	}

	args := append([]string(nil), rt.Args...)
	if fs.Enabled {
		for _, p := range fs.AllowedPaths {
			abs := p
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(workspace, p)
			}
			// Use --add-dir=<path> form (one path per flag). claude's --add-dir
			// is variadic — the space-separated form would greedily consume the
			// trailing prompt positional as another directory and the CLI would
			// then bail out with "Input must be provided ... when using --print".
			args = append(args, "--add-dir="+abs)
		}
		// Read-only whitelist: model can inspect files but not modify them or
		// run arbitrary shell. Drop this list (or swap to bypassPermissions)
		// if write tools are needed.
		// --allowedTools is variadic too — same trap as --add-dir; use the
		// = form so it cannot consume the trailing prompt positional.
		args = append(args, "--allowedTools=Read,LS,Glob,Grep")
	}

	return wrapper.SessionConfig{
		Name:    name,
		Command: rt.Command,
		Args:    args,
		Env:     env,
		WorkDir: workspace,
		Timeout: timeout,
	}, nil
}
