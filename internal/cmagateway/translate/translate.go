// Package translate maps between the CMA wire model and ahsir's agent card.
package translate

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/wu8685/ahsir/internal/cmagateway/ahsir"
	"github.com/wu8685/ahsir/internal/cmagateway/cma"
)

// RuntimeDefaults carry the provider credentials baked into every card.
type RuntimeDefaults struct {
	Provider string
	BaseURL  string
	APIKey   string
}

// AhsirAgentName derives a stable, unique ahsir agent name for one
// (agentID, version). Versioning lives entirely here — ahsir sees distinct
// agents and stays version-agnostic.
//
// agentID looks like "agent_<base32>"; we drop the prefix and append the
// version so the name is a clean DNS-ish token.
func AhsirAgentName(agentID string, version int64) string {
	id := strings.TrimPrefix(agentID, cma.PrefixAgent+"_")
	return fmt.Sprintf("cma-%s-v%d", id, version)
}

// AgentToCard renders a versioned CMA agent into an ahsir card.
//
//   - model        -> runtime.model (provider/baseURL/apiKey from defaults)
//   - system       -> claude.systemPrompt
//   - skills        -> descriptive skills list (ahsir skills are descriptive)
//   - mcp_servers  -> mcp.servers (claude remote-server shape; auth deferred)
//   - filesystem    -> enabled + write_access so the prebuilt toolset
//     (bash/read/write/edit) is available, the agent_toolset_20260401 analog
//   - metadata["session_isolation"] -> pool.session_isolation so concurrent
//     sessions of one facade agent get isolated working trees (issue #19)
//   - metadata["agent_idle_timeout"] -> runtime.agent_idle_timeout so an
//     always-hot agent can be pinned resident ("0") and never cold-started on
//     the next event (issue #17)
//   - streaming.partial_messages -> true so deltas flow once A2A is wired
func AgentToCard(name string, a *cma.Agent, d RuntimeDefaults) *ahsir.AgentCard {
	card := &ahsir.AgentCard{
		Name:        name,
		Description: a.Description,
		Version:     fmt.Sprintf("%d", a.Version),
		Claude: ahsir.ClaudeConfig{
			SystemPrompt: a.System,
		},
		Runtime: ahsir.RuntimeConfig{
			Provider: d.Provider,
			BaseURL:  d.BaseURL,
			APIKey:   d.APIKey,
			Model:    a.Model.ID,
			// Optional per-agent override of ahsir's 120s runtime timeout, for
			// long-running tool-driven turns (e.g. an event agent that shells
			// out repeatedly). Empty -> ahsir default.
			Timeout: a.Metadata["runtime_timeout"],
			// Optional per-agent override of the scale-to-zero idle-reap window
			// (issue #6). "0" pins an always-hot agent resident so it never gets
			// idle-reaped and then hit with a cold-start wake on the next facade
			// event (issue #17). Empty -> ahsir default (10m).
			AgentIdleTimeout: a.Metadata["agent_idle_timeout"],
		},
		Filesystem: ahsir.FilesystemConfig{
			Enabled:     true,
			WriteAccess: true,
			// Opt into the Bash tool only when the agent explicitly asks for it
			// via metadata — e.g. an event-driven agent that must run git/CLI
			// tools itself. Default stays shell-less.
			ShellAccess:  a.Metadata["shell_access"] == "true",
			AllowedPaths: []string{"."},
		},
		Streaming: ahsir.StreamingConfig{PartialMessages: true},
		Pool: ahsir.PoolConfig{
			// Opt a facade agent into per-session filesystem isolation
			// ("off" | "scratch" | "worktree"). Empty -> ahsir default (off,
			// shared workdir). Mirrors the shell_access / runtime_timeout knobs.
			SessionIsolation: a.Metadata["session_isolation"],
		},
	}

	for _, s := range a.Skills {
		card.Skills = append(card.Skills, ahsir.SkillConfig{
			Name:        s.SkillID,
			Description: skillDescription(s),
		})
	}

	if len(a.MCPServers) > 0 {
		servers := map[string]any{}
		for _, m := range a.MCPServers {
			// claude remote MCP shape; vault-based auth is deferred.
			servers[m.Name] = map[string]any{"type": "http", "url": m.URL}
		}
		card.MCP = ahsir.MCPConfig{Servers: servers}
	}

	return card
}

// Instances reads the optional metadata["instances"] knob — the maximum number
// of concurrent runtime instances the scheduler may pool for this agent so
// parallel sessions run in isolated workspaces (issue #18). It is a scheduler
// start-agent field (not part of the card), so callers pass the result to
// RegisterAgent separately. Empty, non-numeric, or < 1 yields 0, which the
// scheduler treats as single-instance (unchanged behaviour).
func Instances(a *cma.Agent) int {
	n, err := strconv.Atoi(strings.TrimSpace(a.Metadata["instances"]))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

func skillDescription(s cma.SkillRef) string {
	if s.Type == "anthropic" {
		return "anthropic prebuilt skill: " + s.SkillID
	}
	return "custom skill: " + s.SkillID
}
