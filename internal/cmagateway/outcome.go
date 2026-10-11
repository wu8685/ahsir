package cmagateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wu8685/ahsir/internal/cmagateway/cma"
	"github.com/wu8685/ahsir/internal/cmagateway/store"
)

// Outcome payload limits (issue #36). These bound resource amplification: an
// outcome runs up to max_iterations executor turns plus one grading call each,
// so unbounded inputs would multiply through the loop.
const (
	outcomeMaxDescriptionBytes = 16 << 10
	outcomeMaxRubricBytes      = 64 << 10
	outcomeMaxDeliverableBytes = 256 << 10

	defaultOutcomeMaxIterationsCap = 20 // mirrors the CMA docs' max
	defaultOutcomeIterations       = 3  // mirrors the CMA docs' default
	defaultGraderTimeout           = 5 * time.Minute
	defaultOutcomeHeartbeat        = 15 * time.Second
)

// validateDefineOutcome checks a user.define_outcome event's shape, returning
// the trimmed description, rubric text, and resolved max_iterations, or a
// client-facing error message (400).
func (s *Server) validateDefineOutcome(ev cma.Event) (desc, rubric string, maxIter int, errMsg string) {
	desc = strings.TrimSpace(ev.Description)
	if desc == "" {
		return "", "", 0, "user.define_outcome: description is required"
	}
	if len(ev.Description) > outcomeMaxDescriptionBytes {
		return "", "", 0, fmt.Sprintf("user.define_outcome: description exceeds %d bytes", outcomeMaxDescriptionBytes)
	}
	if ev.Rubric == nil {
		return "", "", 0, "user.define_outcome: rubric is required"
	}
	switch ev.Rubric.Type {
	case "text":
		rubric = strings.TrimSpace(ev.Rubric.Content)
		if rubric == "" {
			return "", "", 0, "user.define_outcome: rubric content must not be blank"
		}
		if len(ev.Rubric.Content) > outcomeMaxRubricBytes {
			return "", "", 0, fmt.Sprintf("user.define_outcome: rubric exceeds %d bytes", outcomeMaxRubricBytes)
		}
	case "file":
		return "", "", 0, "user.define_outcome: file rubrics require the Files API, which this facade does not implement; pass the rubric inline as {\"type\":\"text\",\"content\":...}"
	default:
		return "", "", 0, fmt.Sprintf("user.define_outcome: unsupported rubric type %q (want \"text\")", ev.Rubric.Type)
	}
	if ev.MaxIterations < 0 {
		return "", "", 0, "user.define_outcome: max_iterations must not be negative"
	}
	cap := s.cfg.OutcomeMaxIterationsCap
	if cap <= 0 {
		cap = defaultOutcomeMaxIterationsCap
	}
	if ev.MaxIterations > cap {
		return "", "", 0, fmt.Sprintf("user.define_outcome: max_iterations %d exceeds the configured cap %d", ev.MaxIterations, cap)
	}
	maxIter = ev.MaxIterations
	if maxIter == 0 {
		maxIter = s.cfg.OutcomeDefaultIterations
		if maxIter <= 0 {
			maxIter = defaultOutcomeIterations
		}
	}
	return desc, rubric, maxIter, ""
}

// deliverDefineOutcome validates, persists, and starts an outcome. It returns
// an HTTP status + message for the sendEvents handler; (200, "") on success.
// The inbound event is persisted (echoed with the generated outcome_id), so it
// is visible on the events list and the SSE stream.
func (s *Server) deliverDefineOutcome(rec *store.SessionRecord, ev cma.Event) (int, string) {
	desc, rubric, maxIter, errMsg := s.validateDefineOutcome(ev)
	if errMsg != "" {
		return 400, errMsg
	}
	now := time.Now().UTC()
	oc := cma.OutcomeEvaluation{
		OutcomeID:     cma.NewID(cma.PrefixOutcome),
		Description:   desc,
		Rubric:        rubric,
		MaxIterations: maxIter,
		Result:        cma.OutcomeStatePending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	ok, err := s.store.BeginOutcome(rec, oc)
	if err != nil {
		return 500, "persist outcome: " + err.Error()
	}
	if !ok {
		return 409, "an outcome is already active on this session; interrupt it or wait for its terminal span.outcome_evaluation_end"
	}
	echo := newEvent(cma.EvtUserDefineOutcome)
	echo.Description = desc
	echo.Rubric = ev.Rubric
	echo.MaxIterations = maxIter
	echo.OutcomeID = oc.OutcomeID
	s.emit(rec, echo)

	// The cancel func is registered before the job is enqueued so an interrupt
	// landing while the outcome is still queued behind another turn is honored
	// as soon as the loop starts.
	ctx, cancel := context.WithCancel(context.Background())
	rec.SetOutcomeCancel(cancel)
	rec.EnqueueTurn(func() { s.executeOutcome(ctx, rec, oc.OutcomeID) })
	return 200, ""
}

// executeOutcome runs the outcome loop on the session's serial turn goroutine:
// executor turn → independent grading → revise / finish. It is the single
// writer of the outcome's terminal state.
func (s *Server) executeOutcome(ctx context.Context, rec *store.SessionRecord, outcomeID string) {
	defer rec.SetOutcomeCancel(nil)

	oc := s.findOutcome(rec, outcomeID)
	if oc == nil {
		return
	}
	agent, err := s.sessionAgent(rec)
	if err != nil {
		s.emitOutcomeEnd(rec, outcomeID, 0, "", cma.OutcomeResultFailed, "executor agent unavailable: "+err.Error())
		s.finishOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
			o.Result = cma.OutcomeResultFailed
			o.Explanation = "executor agent unavailable: " + err.Error()
		})
		s.emitIdle(rec)
		return
	}

	var lastGrade *cma.OutcomeGrade
	deliverable := ""
	for i := 0; i < oc.MaxIterations; i++ {
		if ctx.Err() != nil {
			s.finishOutcomeInterrupted(rec, outcomeID, i, "")
			return
		}
		s.updateOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
			o.Result = cma.OutcomeStateRunning
			o.Iteration = i
		})
		res, err := s.executeTurnCore(ctx, rec, outcomePrompt(oc, i, lastGrade))
		switch {
		case res.canceled || ctx.Err() != nil:
			s.finishOutcomeInterrupted(rec, outcomeID, i, "")
			return
		case err != nil:
			// executeTurnCore already terminated the session; record the outcome
			// failure but do not flip the session back to idle.
			s.emitOutcomeEnd(rec, outcomeID, i, "", cma.OutcomeResultFailed, "executor turn failed: "+err.Error())
			s.finishOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
				o.Result = cma.OutcomeResultFailed
				o.Explanation = "executor turn failed"
			})
			return
		}
		deliverable = res.reply

		start := s.emitOutcomeStart(rec, outcomeID, i)
		s.updateOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
			o.Result = cma.OutcomeStateEvaluating
			o.Iteration = i
		})
		grade, gerr := s.gradeWithRetry(ctx, rec, agent, oc, i, deliverable)
		if ctx.Err() != nil {
			s.finishOutcomeInterrupted(rec, outcomeID, i, start.ID)
			return
		}
		if gerr != nil {
			msg := "grader failed after one retry: " + gerr.Error()
			s.emitOutcomeEnd(rec, outcomeID, i, start.ID, cma.OutcomeResultFailed, msg)
			s.finishOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
				o.Result = cma.OutcomeResultFailed
				o.Explanation = msg
			})
			s.emitIdle(rec)
			return
		}
		lastGrade = grade

		if grade.Verdict == cma.OutcomeVerdictSatisfied {
			s.emitOutcomeEnd(rec, outcomeID, i, start.ID, cma.OutcomeResultSatisfied, grade.Explanation)
			s.finishOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
				o.Result = cma.OutcomeResultSatisfied
				o.Explanation = grade.Explanation
				o.Criteria = grade.Criteria
				o.Deliverable = truncateForOutcome(deliverable, outcomeMaxDeliverableBytes)
			})
			s.emitIdle(rec)
			return
		}

		if i+1 >= oc.MaxIterations {
			s.emitOutcomeEnd(rec, outcomeID, i, start.ID, cma.OutcomeResultMaxIterationsReached, grade.Explanation)
			s.finishOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
				o.Result = cma.OutcomeResultMaxIterationsReached
				o.Explanation = grade.Explanation
				o.Criteria = grade.Criteria
				o.Deliverable = truncateForOutcome(deliverable, outcomeMaxDeliverableBytes)
			})
			// Per the CMA docs, one final acknowledgment turn follows
			// max_iterations_reached before the session goes idle. It is not
			// evaluated; its reply (if any) becomes the recorded deliverable.
			res, err := s.executeTurnCore(ctx, rec, outcomeAckPrompt(oc, grade))
			if err == nil && !res.canceled && strings.TrimSpace(res.reply) != "" {
				s.updateOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
					o.Deliverable = truncateForOutcome(res.reply, outcomeMaxDeliverableBytes)
				})
			}
			s.emitIdle(rec)
			return
		}

		s.emitOutcomeEnd(rec, outcomeID, i, start.ID, cma.OutcomeResultNeedsRevision, grade.Explanation)
		s.updateOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
			o.Explanation = grade.Explanation
			o.Criteria = grade.Criteria
		})
	}
}

// gradeWithRetry runs one bounded retry: a grader error or unparseable reply
// gets exactly one second attempt with a fresh grader context before the
// outcome is failed. Each attempt is announced with an ongoing heartbeat.
func (s *Server) gradeWithRetry(ctx context.Context, rec *store.SessionRecord, agent *cma.Agent, oc *cma.OutcomeEvaluation, iteration int, reply string) (*cma.OutcomeGrade, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.emitOutcomeOngoing(rec, oc.OutcomeID, iteration)
		grade, err := s.gradeOnce(ctx, rec, agent, oc, iteration, reply)
		if err == nil {
			return grade, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *Server) gradeOnce(ctx context.Context, rec *store.SessionRecord, agent *cma.Agent, oc *cma.OutcomeEvaluation, iteration int, reply string) (*cma.OutcomeGrade, error) {
	stop := s.startOutcomeHeartbeat(rec, oc.OutcomeID, iteration)
	defer stop()
	return s.graderFor(agent).Grade(ctx, GradeRequest{
		OutcomeID: oc.OutcomeID,
		Goal:      oc.Description,
		Rubric:    oc.Rubric,
		Iteration: iteration,
		Response:  truncateForOutcome(reply, outcomeMaxDeliverableBytes),
	})
}

// startOutcomeHeartbeat emits span.outcome_evaluation_ongoing on the
// configured cadence while a grading call is in flight. Negative interval
// disables the ticker (tests); 0 selects the default.
func (s *Server) startOutcomeHeartbeat(rec *store.SessionRecord, outcomeID string, iteration int) (stop func()) {
	interval := s.cfg.OutcomeHeartbeatInterval
	if interval == 0 {
		interval = defaultOutcomeHeartbeat
	}
	if interval < 0 {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.emitOutcomeOngoing(rec, outcomeID, iteration)
			}
		}
	}()
	return func() { close(done) }
}

// finishOutcomeInterrupted records the interrupted terminal state. startID is
// "" when the interrupt arrived before any evaluation started — the end event
// then carries an empty outcome_evaluation_start_id, per the CMA docs.
func (s *Server) finishOutcomeInterrupted(rec *store.SessionRecord, outcomeID string, iteration int, startID string) {
	s.emitOutcomeEnd(rec, outcomeID, iteration, startID, cma.OutcomeResultInterrupted, "outcome interrupted by user")
	s.finishOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
		o.Result = cma.OutcomeResultInterrupted
		o.Explanation = "outcome interrupted by user"
	})
	s.emitIdle(rec)
}

// finishOutcome applies a terminal mutation and stamps completed_at.
func (s *Server) finishOutcome(rec *store.SessionRecord, outcomeID string, mutate func(*cma.OutcomeEvaluation)) {
	now := time.Now().UTC()
	s.updateOutcome(rec, outcomeID, func(o *cma.OutcomeEvaluation) {
		mutate(o)
		o.CompletedAt = &now
	})
}

func (s *Server) updateOutcome(rec *store.SessionRecord, outcomeID string, mutate func(*cma.OutcomeEvaluation)) {
	_ = s.store.UpdateOutcome(rec, outcomeID, mutate)
}

func (s *Server) findOutcome(rec *store.SessionRecord, outcomeID string) *cma.OutcomeEvaluation {
	for i, o := range rec.OutcomeEvaluations() {
		if o.OutcomeID == outcomeID {
			cp := rec.OutcomeEvaluations()[i]
			return &cp
		}
	}
	return nil
}

// ----- outcome events -----

func (s *Server) emitOutcomeStart(rec *store.SessionRecord, outcomeID string, iteration int) cma.Event {
	ev := newEvent(cma.EvtSpanOutcomeEvaluationStart)
	ev.OutcomeID = outcomeID
	ev.Iteration = intPtr(iteration)
	s.emit(rec, ev)
	return ev
}

func (s *Server) emitOutcomeOngoing(rec *store.SessionRecord, outcomeID string, iteration int) {
	ev := newEvent(cma.EvtSpanOutcomeEvaluationOngoing)
	ev.OutcomeID = outcomeID
	ev.Iteration = intPtr(iteration)
	s.emit(rec, ev)
}

func (s *Server) emitOutcomeEnd(rec *store.SessionRecord, outcomeID string, iteration int, startID, result, explanation string) {
	ev := newEvent(cma.EvtSpanOutcomeEvaluationEnd)
	ev.OutcomeID = outcomeID
	ev.Iteration = intPtr(iteration)
	ev.OutcomeEvaluationStartID = &startID
	ev.Result = result
	ev.Explanation = explanation
	// The SDK model's usage fields are required ints; the ahsir chat transport
	// does not report grader token usage, so emit a zero-valued object.
	ev.Usage = &cma.ModelUsage{}
	s.emit(rec, ev)
}

func intPtr(v int) *int { return &v }

// ----- executor prompts -----

// outcomePrompt renders the executor turn text. Iteration 0 is goal + rubric;
// revisions add the grader's explanation and failing criteria — the only
// grader output the executor ever sees.
func outcomePrompt(oc *cma.OutcomeEvaluation, iteration int, grade *cma.OutcomeGrade) string {
	var b strings.Builder
	if iteration == 0 {
		b.WriteString("You are working toward a defined outcome. Work autonomously until the deliverable satisfies the rubric.\n\n")
	} else {
		fmt.Fprintf(&b, "Your previous deliverable (iteration %d) did not satisfy the rubric. Grader feedback:\n%s\n", iteration-1, grade.Explanation)
		var failing []string
		for _, c := range grade.Criteria {
			if !c.Passed {
				line := "- " + c.Criterion
				if c.Feedback != "" {
					line += ": " + c.Feedback
				}
				failing = append(failing, line)
			}
		}
		if len(failing) > 0 {
			b.WriteString("\nFailing criteria:\n" + strings.Join(failing, "\n") + "\n")
		}
		b.WriteString("\nRevise your deliverable to address every failing criterion.\n\n")
	}
	fmt.Fprintf(&b, "## Goal\n%s\n\n## Rubric\n%s\n", oc.Description, oc.Rubric)
	return b.String()
}

// outcomeAckPrompt is the final, unevaluated acknowledgment turn after
// max_iterations_reached.
func outcomeAckPrompt(oc *cma.OutcomeEvaluation, grade *cma.OutcomeGrade) string {
	return fmt.Sprintf(`The iteration budget for this outcome is exhausted; the rubric is not fully satisfied. Provide a concise final summary of the deliverable's current state, noting which rubric criteria remain unmet.

## Goal
%s

## Last grader feedback
%s`, oc.Description, grade.Explanation)
}

// recoverOutcomes reconciles persisted outcome state on facade startup. Any
// outcome that was still active when the process died is marked failed: the
// facade cannot know whether the executor or grader finished, and an uncertain
// outcome must never be reported as satisfied.
func (s *Server) recoverOutcomes() {
	for _, sess := range s.store.ListSessions() {
		rec, ok := s.store.Session(sess.ID)
		if !ok {
			continue
		}
		for _, o := range rec.OutcomeEvaluations() {
			if cma.OutcomeTerminal(o.Result) {
				continue
			}
			const msg = "facade restarted before the outcome completed; recorded as failed rather than guessing satisfied"
			s.emitOutcomeEnd(rec, o.OutcomeID, o.Iteration, "", cma.OutcomeResultFailed, msg)
			s.finishOutcome(rec, o.OutcomeID, func(e *cma.OutcomeEvaluation) {
				e.Result = cma.OutcomeResultFailed
				e.Explanation = msg
			})
		}
	}
}
