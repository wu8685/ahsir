package scheduler

import (
	"errors"
	"fmt"
	"os"

	"github.com/wu8685/ahsir/internal/wrapper"
)

type agentConfigurationError struct {
	reasonCode string
	err        error
}

func (e *agentConfigurationError) Error() string { return e.err.Error() }
func (e *agentConfigurationError) Unwrap() error { return e.err }

func validateAgentConfiguration(cfg AgentConfig) error {
	card, err := wrapper.NewAgentCardBuilder(cfg.Workspace).Load()
	if err != nil {
		// Several scheduler embedders and lifecycle tests provide their own
		// agentCommand and intentionally omit an on-disk card. Preserve that
		// supported injection path; the real ahsir-agent remains responsible for
		// reporting a missing card when the default command is used.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return &agentConfigurationError{
			reasonCode: "invalid-agent-card",
			err:        fmt.Errorf("invalid agent card: %w", err),
		}
	}
	if err := wrapper.ValidateRuntimeConfig(card.Runtime, card.MCP); err != nil {
		return &agentConfigurationError{
			reasonCode: "invalid-runtime",
			err:        fmt.Errorf("invalid runtime config: %w", err),
		}
	}
	return nil
}
