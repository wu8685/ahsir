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
}
