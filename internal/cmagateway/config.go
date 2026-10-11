package cmagateway

import "time"

// Config carries the CMA facade's settings. It is intentionally minimal: the
// ahsir client and session store are injected into New separately, so this
// holds only auth, runtime defaults, and the per-turn timeout.
type Config struct {
	// APIKeys is the x-api-key allowlist. Empty => allow all (local/zero-config).
	APIKeys map[string]bool

	// Runtime* seed translate.RuntimeDefaults for inline agent cards.
	RuntimeProvider string
	RuntimeBaseURL  string
	RuntimeAPIKey   string

	// TurnTimeout bounds a single turn's execution (0 => handler default).
	TurnTimeout time.Duration

	// Outcome knobs (issue #36). Zero values select the documented defaults;
	// there is deliberately no "unbounded" setting — see docs/cma-api.md.
	//
	// OutcomeMaxIterationsCap caps a requested max_iterations (default 20,
	// mirroring the CMA docs); OutcomeDefaultIterations applies when the
	// request omits it (default 3).
	OutcomeMaxIterationsCap  int
	OutcomeDefaultIterations int
	// GraderTimeout bounds one grading call (default 5m).
	GraderTimeout time.Duration
	// OutcomeHeartbeatInterval is the span.outcome_evaluation_ongoing cadence
	// while the grader runs (default 15s; negative disables).
	OutcomeHeartbeatInterval time.Duration
}
