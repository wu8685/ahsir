package wrapper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Agent Session lifecycle (issue #43).
//
// The lifecycle block is ahsir-owned, provider-neutral runtime infrastructure:
// an optional one-time Session initialization step and a two-stage turn
// deadline (work_timeout soft deadline -> grace_timeout finalize turn -> hard
// terminate). It knows nothing about DingTalk, dws, GitHub, or any other
// business protocol — lifecycle prompts are plain text the operator owns.
//
// A missing lifecycle block preserves the historical behaviour byte-for-byte:
// no initialization, and the one-step provider timeout stays the only
// per-turn deadline.

// LifecycleConfig is the versioned `lifecycle:` block of agent-card.yaml.
//
// Example:
//
//	lifecycle:
//	  initialize:
//	    enabled: true
//	    timeout: 1m
//	    prompt: "Read the workspace guidance and validate your tools."
//	  finalize:
//	    enabled: true
//	    work_timeout: 20m
//	    grace_timeout: 2m
//	    prompt: "Stop investigating and deliver from existing evidence."
//
// Durations use time.ParseDuration form and are validated + bounded by
// ResolveLifecycle. Lifecycle turns run inside the Agent's existing sandbox —
// a lifecycle prompt cannot widen filesystem, shell, network, or credential
// capabilities beyond what the card already grants.
type LifecycleConfig struct {
	Initialize LifecycleInitializeConfig `yaml:"initialize" json:"initialize"`
	Finalize   LifecycleFinalizeConfig   `yaml:"finalize" json:"finalize"`
}

// LifecycleInitializeConfig controls the lazy, once-per-Session
// initialization turn that runs before the first business turn.
type LifecycleInitializeConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Timeout bounds the initialization turn. Default: 1m. Bounded to
	// [lifecycleMinDuration, lifecycleMaxDuration].
	Timeout string `yaml:"timeout" json:"timeout"`
	// Prompt is the initialization instruction. It SHOULD be idempotent —
	// ahsir persists a completion marker, but a crash between the marker
	// write and an irreversible external write can rerun the step, so
	// initialization must not perform irreversible external writes.
	Prompt string `yaml:"prompt" json:"prompt"`
	// OnFailure selects the failure policy: "fail_closed" (default — the
	// business turn does not start and a typed initialization-failure
	// outcome is recorded) or "degraded" (explicit opt-in: the failure is
	// recorded once per lifecycle version and the business turn proceeds
	// without initialization).
	OnFailure string `yaml:"on_failure" json:"on_failure"`
}

// LifecycleFinalizeConfig controls the two-stage turn deadline.
type LifecycleFinalizeConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// WorkTimeout is the soft deadline of a business turn: when it fires,
	// the active runtime turn is cancelled (transcript, tool evidence, and
	// the provider resume identity are preserved) and a high-priority
	// finalize turn starts in the same logical Session. Required when
	// enabled.
	WorkTimeout string `yaml:"work_timeout" json:"work_timeout"`
	// GraceTimeout bounds the finalize turn itself. If finalization cannot
	// complete within the grace period the runtime is hard-terminated with a
	// distinct finalize-timeout outcome. Default: 2m. Must not exceed
	// work_timeout.
	GraceTimeout string `yaml:"grace_timeout" json:"grace_timeout"`
	// Prompt is the finalize instruction. Default: stop new investigation
	// and summarize/deliver from existing evidence.
	Prompt string `yaml:"prompt" json:"prompt"`
}

// Initialization failure policies.
const (
	// InitFailClosed (default): a failed initialization prevents the first
	// business turn and produces a typed initialization-failure outcome.
	InitFailClosed = "fail_closed"
	// InitDegraded: explicit opt-in — record the failure once per lifecycle
	// version and serve business turns without initialization.
	InitDegraded = "degraded"
)

// Lifecycle duration bounds. Sub-second deadlines are never meaningful for an
// LLM turn; hour-plus deadlines defeat the resource-protection purpose.
const (
	lifecycleMinDuration = time.Second
	lifecycleMaxDuration = time.Hour
)

// Default lifecycle prompts, used when a block is enabled without an explicit
// prompt. Both instruct system-owned lifecycle work only — they grant no new
// capabilities.
const (
	DefaultInitializePrompt = "This is a one-time session initialization turn owned by the runtime, not a user message. " +
		"Read the workspace guidance, validate that the tools you need are available, and prepare durable context for the session. " +
		"Keep this step idempotent and do not perform irreversible external writes. Reply with a brief readiness summary."
	DefaultFinalizePrompt = "The work deadline for this turn was reached. This is a finalize turn owned by the runtime, not a user message. " +
		"Stop all new investigation and tool use now. Summarize the evidence gathered so far and deliver the best final answer you can from it. " +
		"Do not start new lines of inquiry."
)

// ResolvedLifecycle is the validated, defaults-applied form of
// LifecycleConfig. It is the only shape the runtime consults.
type ResolvedLifecycle struct {
	Initialize struct {
		Enabled   bool
		Timeout   time.Duration
		Prompt    string
		OnFailure string
	}
	Finalize struct {
		Enabled      bool
		WorkTimeout  time.Duration
		GraceTimeout time.Duration
		Prompt       string
	}
	// Version is a stable hash of the canonical lifecycle config. The
	// per-Session initialization marker is keyed by it, so a lifecycle
	// prompt/config change re-runs initialization exactly once per Session.
	Version string
}

// Enabled reports whether any lifecycle block is on. When false the runtime
// must behave exactly as if no lifecycle block existed.
func (r ResolvedLifecycle) Enabled() bool {
	return r.Initialize.Enabled || r.Finalize.Enabled
}

// ResolveLifecycle validates a card's lifecycle block and applies defaults.
// It is side-effect-free; both the card loader and the agent bootstrap call
// it so a bad lifecycle block fails before any provider process starts.
func ResolveLifecycle(cfg LifecycleConfig) (ResolvedLifecycle, error) {
	var r ResolvedLifecycle

	r.Initialize.Enabled = cfg.Initialize.Enabled
	r.Initialize.Prompt = cfg.Initialize.Prompt
	r.Initialize.OnFailure = cfg.Initialize.OnFailure
	if r.Initialize.Enabled {
		d, err := resolveLifecycleDuration("lifecycle.initialize.timeout", cfg.Initialize.Timeout, time.Minute)
		if err != nil {
			return ResolvedLifecycle{}, err
		}
		r.Initialize.Timeout = d
		switch r.Initialize.OnFailure {
		case "":
			r.Initialize.OnFailure = InitFailClosed
		case InitFailClosed, InitDegraded:
		default:
			return ResolvedLifecycle{}, fmt.Errorf("lifecycle.initialize.on_failure %q: want %q or %q", r.Initialize.OnFailure, InitFailClosed, InitDegraded)
		}
		if r.Initialize.Prompt == "" {
			r.Initialize.Prompt = DefaultInitializePrompt
		}
	}

	r.Finalize.Enabled = cfg.Finalize.Enabled
	r.Finalize.Prompt = cfg.Finalize.Prompt
	if r.Finalize.Enabled {
		if cfg.Finalize.WorkTimeout == "" {
			return ResolvedLifecycle{}, fmt.Errorf("lifecycle.finalize.work_timeout is required when finalize is enabled")
		}
		work, err := resolveLifecycleDuration("lifecycle.finalize.work_timeout", cfg.Finalize.WorkTimeout, 0)
		if err != nil {
			return ResolvedLifecycle{}, err
		}
		grace, err := resolveLifecycleDuration("lifecycle.finalize.grace_timeout", cfg.Finalize.GraceTimeout, 2*time.Minute)
		if err != nil {
			return ResolvedLifecycle{}, err
		}
		if grace > work {
			return ResolvedLifecycle{}, fmt.Errorf("lifecycle.finalize.grace_timeout (%v) must not exceed work_timeout (%v)", grace, work)
		}
		r.Finalize.WorkTimeout = work
		r.Finalize.GraceTimeout = grace
		if r.Finalize.Prompt == "" {
			r.Finalize.Prompt = DefaultFinalizePrompt
		}
	}

	r.Version = lifecycleVersion(cfg)
	return r, nil
}

func resolveLifecycleDuration(field, raw string, def time.Duration) (time.Duration, error) {
	if raw == "" {
		if def == 0 {
			return 0, fmt.Errorf("%s is required", field)
		}
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", field, raw, err)
	}
	if d < lifecycleMinDuration || d > lifecycleMaxDuration {
		return 0, fmt.Errorf("%s %q: must be between %v and %v", field, raw, lifecycleMinDuration, lifecycleMaxDuration)
	}
	return d, nil
}

// lifecycleVersion hashes the canonical YAML form of the lifecycle block so
// the initialization marker can detect config changes without ahsir tracking
// schema versions by hand.
func lifecycleVersion(cfg LifecycleConfig) string {
	canonical, err := yaml.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:8])
}
