package cma

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// ID prefixes mirror the Anthropic resource conventions.
const (
	PrefixAgent       = "agent"
	PrefixEnvironment = "env"
	PrefixSession     = "sesn"
	PrefixEvent       = "sevt"
	// PrefixOutcome identifies a defined outcome ("outc_…", per the CMA docs).
	PrefixOutcome = "outc"
	// PrefixGraderContext identifies a one-shot grader conversation. Every
	// grading call uses a fresh one so the grader's context window holds only
	// the goal, rubric, and deliverable under evaluation.
	PrefixGraderContext = "gctx"
)

var enc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewID returns a prefixed, collision-resistant id, e.g. "agent_a1b2c3...".
func NewID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "_" + strings.ToLower(enc.EncodeToString(b))
}
