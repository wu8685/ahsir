package cmagateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wu8685/ahsir/internal/cmagateway/ahsir"
	"github.com/wu8685/ahsir/internal/cmagateway/cma"
	"github.com/wu8685/ahsir/internal/cmagateway/translate"
)

// GradeRequest is one rubric evaluation: the goal, the rubric, the 0-indexed
// iteration, and the executor's deliverable under evaluation (already
// size-capped by the caller).
type GradeRequest struct {
	OutcomeID string
	Goal      string
	Rubric    string
	Iteration int
	Response  string
}

// OutcomeGrader evaluates a deliverable against the outcome rubric. It runs
// independently of the executor: the executor's conversation history is never
// part of grading, and only the sanitized OutcomeGrade reaches the CMA wire.
type OutcomeGrader interface {
	Grade(ctx context.Context, req GradeRequest) (*cma.OutcomeGrade, error)
}

// Grader reply sanitation caps — bound resource amplification from a hostile
// or confused grader model.
const (
	gradeMaxCriteria       = 100
	gradeMaxCriterionBytes = 1 << 10
	gradeMaxFeedbackBytes  = 4 << 10
	gradeMaxExplainBytes   = 8 << 10
)

// graderSystemPrompt keeps the grader's output machine-readable and free of
// chain-of-thought. Only the parsed fields ever leave this package.
func graderSystemPrompt() string {
	return `You are an impartial outcome grader. You evaluate whether an agent's deliverable satisfies a stated goal and rubric. You see only the goal, the rubric, and the deliverable — never the agent's conversation.

Respond with ONLY a JSON object — no prose, no markdown fences:
{
  "verdict": "satisfied" | "needs_revision",
  "criteria": [{"criterion": "<one rubric criterion>", "passed": true|false, "feedback": "<short, actionable>"}],
  "explanation": "<2-3 sentence summary safe to show the user>"
}

Rules:
- Cover every rubric criterion in criteria, one entry each.
- verdict is "satisfied" only when every criterion passes.
- feedback and explanation are shown to the user and handed back to the executing agent: keep them concise, specific, and actionable.
- Never include internal deliberation or chain-of-thought in any field.`
}

func graderUserMessage(req GradeRequest) string {
	return fmt.Sprintf(`# Goal
%s

# Rubric
%s

# Deliverable under evaluation (iteration %d)
%s`, req.Goal, req.Rubric, req.Iteration, req.Response)
}

// ahsirGrader provisions a dedicated grader ahsir agent (its own process, its
// own context) and grades over the synchronous chat endpoint. Each Grade call
// uses a fresh contextId, so the grader's context window contains exactly one
// goal + rubric + deliverable — structurally independent of the executor.
type ahsirGrader struct {
	client  *ahsir.Client
	name    string
	card    *ahsir.AgentCard
	timeout time.Duration
	cardErr error // deferred card-build error (e.g. invalid metadata env name)
}

// newAhsirGrader derives the grader agent from the session's agent: same
// runtime provider/model/credentials, but a grader system prompt, no tools, no
// filesystem, no shell, no network.
func newAhsirGrader(ac *ahsir.Client, rt translate.RuntimeDefaults, agent *cma.Agent, timeout time.Duration) OutcomeGrader {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	g := &ahsirGrader{
		client:  ac,
		name:    translate.AhsirGraderName(agent.ID, agent.Version),
		timeout: timeout,
	}
	graderAgent := *agent // copy; never mutate the shared agent
	graderAgent.System = graderSystemPrompt()
	graderAgent.Tools = nil
	graderAgent.Skills = nil
	graderAgent.MCPServers = nil
	card, err := translate.AgentToCard(g.name, &graderAgent, rt)
	if err != nil {
		g.cardErr = err
		return g
	}
	card.Filesystem = ahsir.FilesystemConfig{Enabled: false}
	card.Network = ahsir.NetworkConfig{OutboundAccess: false}
	g.card = card
	return g
}

func (g *ahsirGrader) Grade(ctx context.Context, req GradeRequest) (*cma.OutcomeGrade, error) {
	if g.cardErr != nil {
		return nil, fmt.Errorf("grader card: %w", g.cardErr)
	}
	// Registration reconciles on every call (same semantics as the executor):
	// a restarted scheduler loses the grader runtime and must re-provision.
	if err := g.client.RegisterAgent(ctx, g.name, g.card, 0); err != nil {
		return nil, fmt.Errorf("provision grader: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	reply, err := g.client.Chat(ctx, g.name, cma.NewID(cma.PrefixGraderContext), "", graderUserMessage(req))
	if err != nil {
		return nil, err
	}
	return parseGradeReply(reply)
}

// parseGradeReply extracts and validates the grader's JSON verdict. The grader
// is instructed to emit bare JSON but may wrap it in prose or a markdown fence;
// we extract the outermost {...} span. Unknown fields (e.g. a "reasoning" key
// the model invented) are dropped by the typed decode.
func parseGradeReply(raw string) (*cma.OutcomeGrade, error) {
	span := extractJSONObject(raw)
	if span == "" {
		return nil, fmt.Errorf("grader reply contained no JSON object")
	}
	var wire struct {
		Verdict  string `json:"verdict"`
		Criteria []struct {
			Criterion string `json:"criterion"`
			Passed    bool   `json:"passed"`
			Feedback  string `json:"feedback"`
		} `json:"criteria"`
		Explanation string `json:"explanation"`
	}
	if err := json.Unmarshal([]byte(span), &wire); err != nil {
		return nil, fmt.Errorf("grader reply JSON: %w", err)
	}
	switch wire.Verdict {
	case cma.OutcomeVerdictSatisfied, cma.OutcomeVerdictNeedsRevision:
	default:
		return nil, fmt.Errorf("grader verdict %q is not satisfied|needs_revision", wire.Verdict)
	}
	grade := &cma.OutcomeGrade{
		Verdict:     wire.Verdict,
		Explanation: truncateForOutcome(wire.Explanation, gradeMaxExplainBytes),
	}
	for i, c := range wire.Criteria {
		if i >= gradeMaxCriteria {
			break
		}
		criterion := strings.TrimSpace(c.Criterion)
		if criterion == "" {
			continue
		}
		grade.Criteria = append(grade.Criteria, cma.OutcomeCriterion{
			Criterion: truncateForOutcome(criterion, gradeMaxCriterionBytes),
			Passed:    c.Passed,
			Feedback:  truncateForOutcome(c.Feedback, gradeMaxFeedbackBytes),
		})
	}
	return grade, nil
}

// extractJSONObject returns the outermost balanced {...} span, tolerating a
// leading markdown fence or prose. Returns "" when no object is present.
func extractJSONObject(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	return s[start : end+1]
}

// truncateForOutcome caps a wire-bound string, marking truncation.
func truncateForOutcome(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "… [truncated]"
}
