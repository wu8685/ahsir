package scheduler

import (
	"sort"
	"strings"
	"time"
)

// AgentLifecycleState is the scheduler-owned operational state of one base
// agent. It is deliberately independent from registry heartbeat liveness.
type AgentLifecycleState string

const (
	AgentLifecycleOnline         AgentLifecycleState = "online"
	AgentLifecycleIdle           AgentLifecycleState = "idle"
	AgentLifecycleStopped        AgentLifecycleState = "stopped"
	AgentLifecycleInvalidConfig  AgentLifecycleState = "invalid-config"
	AgentLifecycleRestartBackoff AgentLifecycleState = "restart-backoff"
	AgentLifecycleHealthFailed   AgentLifecycleState = "health-failed"
)

type AgentLifecycleSeverity string

const (
	SeverityOK      AgentLifecycleSeverity = "ok"
	SeverityInfo    AgentLifecycleSeverity = "info"
	SeverityWarning AgentLifecycleSeverity = "warning"
	SeverityError   AgentLifecycleSeverity = "error"
)

// AgentLifecycleSnapshot is the stable wire model served by
// GET /diagnostics/agents and consumed by `ahsir doctor`.
type AgentLifecycleSnapshot struct {
	Name           string                 `json:"name"`
	State          AgentLifecycleState    `json:"state"`
	ReasonCode     string                 `json:"reasonCode"`
	Reason         string                 `json:"reason"`
	Severity       AgentLifecycleSeverity `json:"severity"`
	Wakeable       bool                   `json:"wakeable"`
	RestartAttempt int                    `json:"restartAttempt,omitempty"`
	RestartAt      *time.Time             `json:"restartAt,omitempty"`
	UpdatedAt      time.Time              `json:"updatedAt"`
}

func lifecycleSeverity(state AgentLifecycleState) AgentLifecycleSeverity {
	switch state {
	case AgentLifecycleOnline:
		return SeverityOK
	case AgentLifecycleIdle:
		return SeverityInfo
	case AgentLifecycleStopped:
		return SeverityWarning
	default:
		return SeverityError
	}
}

func (s *Scheduler) setLifecycleLocked(name string, state AgentLifecycleState, reasonCode, reason string, attempt int, restartAt time.Time) {
	if s.lifecycles == nil {
		s.lifecycles = make(map[string]AgentLifecycleSnapshot)
	}
	var restartAtPtr *time.Time
	if !restartAt.IsZero() {
		value := restartAt.UTC()
		restartAtPtr = &value
	}
	s.lifecycles[name] = AgentLifecycleSnapshot{
		Name:           name,
		State:          state,
		ReasonCode:     reasonCode,
		Reason:         sanitizeLifecycleReason(reason),
		Severity:       lifecycleSeverity(state),
		Wakeable:       state == AgentLifecycleIdle,
		RestartAttempt: attempt,
		RestartAt:      restartAtPtr,
		UpdatedAt:      time.Now().UTC(),
	}
}

func sanitizeLifecycleReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	const maxReasonBytes = 512
	if len(reason) > maxReasonBytes {
		reason = reason[:maxReasonBytes] + "…"
	}
	return reason
}

// AgentLifecycles returns one stable snapshot per base agent. A current
// heartbeat only overrides a stored state when the scheduler still owns a live
// process (or the card is registry-only/remote); stale cards cannot hide an
// idle or failed managed process.
func (s *Scheduler) AgentLifecycles() []AgentLifecycleSnapshot {
	s.mu.Lock()
	items := make(map[string]AgentLifecycleSnapshot, len(s.lifecycles))
	for name, item := range s.lifecycles {
		items[name] = item
	}
	running := make(map[string]bool, len(s.agents))
	for name := range s.agents {
		running[name] = true
	}
	s.mu.Unlock()

	for _, card := range s.registry.List() {
		if isInstanceChild(card.Name) {
			continue
		}
		status := s.registry.GetStatus(card.Name)
		item, known := items[card.Name]
		if status == "online" && (running[card.Name] || !known || item.ReasonCode == "configured-not-started") {
			items[card.Name] = AgentLifecycleSnapshot{
				Name: card.Name, State: AgentLifecycleOnline, ReasonCode: "healthy",
				Reason: "heartbeat current", Severity: SeverityOK, UpdatedAt: time.Now().UTC(),
			}
			continue
		}
		if !known || item.ReasonCode == "configured-not-started" || item.State == AgentLifecycleOnline {
			items[card.Name] = AgentLifecycleSnapshot{
				Name: card.Name, State: AgentLifecycleHealthFailed, ReasonCode: "heartbeat-timeout",
				Reason: "registry heartbeat timed out", Severity: SeverityError, UpdatedAt: time.Now().UTC(),
			}
		}
	}

	out := make([]AgentLifecycleSnapshot, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
