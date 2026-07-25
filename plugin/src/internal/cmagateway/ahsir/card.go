// Package ahsir is the client to the ahsir scheduler gateway — the internal
// agent runtime that backs every CMA session.
package ahsir

// AgentCard mirrors ahsir's internal wrapper.AgentCardConfig, restricted to the
// fields this service sets, with JSON tags equal to ahsir's YAML tags.
//
// This is the body of the proposed inline-registration admin endpoint
// (`POST /admin/agents` with a `card` field). For it to work end-to-end, ahsir
// must add matching `json` tags to AgentCardConfig (today it only has `yaml`
// tags) — that is the one agreed ahsir-side change, pending implementation.
type AgentCard struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Version     string           `json:"version,omitempty"`
	Skills      []SkillConfig    `json:"skills,omitempty"`
	Claude      ClaudeConfig     `json:"claude"`
	Runtime     RuntimeConfig    `json:"runtime"`
	Filesystem  FilesystemConfig `json:"filesystem"`
	Network     NetworkConfig    `json:"network,omitempty"`
	Streaming   StreamingConfig  `json:"streaming"`
	Pool        PoolConfig       `json:"pool,omitempty"`
	MCP         MCPConfig        `json:"mcp,omitempty"`
}

type SkillConfig struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type ClaudeConfig struct {
	SystemPrompt  string `json:"systemPrompt,omitempty"`
	MaxAgentCalls int    `json:"maxAgentCalls,omitempty"`
}

type RuntimeConfig struct {
	Provider   string                  `json:"provider,omitempty"`
	BaseURL    string                  `json:"baseURL,omitempty"`
	APIKey     string                  `json:"apiKey,omitempty"`
	Model      string                  `json:"model,omitempty"`
	WireAPI    string                  `json:"wireAPI,omitempty"`
	Credential RuntimeCredentialConfig `json:"credential,omitempty"`
	Command    string                  `json:"command,omitempty"`
	Args       []string                `json:"args,omitempty"`
	Env        map[string]string       `json:"env,omitempty"`
	Timeout    string                  `json:"timeout,omitempty"`
	// AgentIdleTimeout maps to ahsir's runtime.agent_idle_timeout — the
	// scale-to-zero idle-reap window (issue #6). Empty -> ahsir default (10m).
	// An explicit "0" pins the agent resident (never idle-reaped), the only way
	// to keep a facade-created always-hot agent from being scaled to zero and
	// then hit with a cold-start wake on the next event (issue #17).
	AgentIdleTimeout string `json:"agent_idle_timeout,omitempty"`
}

type RuntimeCredentialConfig struct {
	EnvKey string `json:"envKey,omitempty"`
}

type FilesystemConfig struct {
	Enabled     bool `json:"enabled"`
	WriteAccess bool `json:"write_access"`
	// ShellAccess opts the agent into the Bash tool (arbitrary command
	// execution) — separate from WriteAccess by design. Maps to ahsir's
	// filesystem.shell_access card knob.
	ShellAccess  bool     `json:"shell_access"`
	AllowedPaths []string `json:"allowed_paths,omitempty"`
}

type StreamingConfig struct {
	PartialMessages bool `json:"partial_messages"`
}

type NetworkConfig struct {
	OutboundAccess bool `json:"outbound_access"`
}

// PoolConfig mirrors the subset of ahsir's wrapper.PoolConfig this facade sets.
type PoolConfig struct {
	// SessionIsolation gives each A2A contextID (session) its own filesystem
	// working directory so concurrent sessions in one agent process don't race
	// on a shared working tree (issue #19). Values mirror ahsir's card knob:
	// "" / "off" (default, shared workdir), "scratch" (empty private dir), or
	// "worktree" (a git worktree checkout per session). Maps to ahsir's
	// pool.session_isolation card knob.
	SessionIsolation string `json:"session_isolation,omitempty"`
}

type MCPConfig struct {
	Servers map[string]any `json:"servers,omitempty"`
}
