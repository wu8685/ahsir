# Issue #36: Outcome-Driven CMA Sessions with Rubric Evaluation

## Status

Implements user-authored issue #36. Wire shapes follow
[Anthropic — Define outcomes](https://platform.claude.com/docs/en/managed-agents/define-outcomes)
(beta header `managed-agents-2026-04-01`); where the public docs are silent the
shape is annotated and verified against the fetched page on 2026-10-11.

## Problem

The CMA facade exposes Sessions as "user.message in, event stream out". An
outcome is different: the caller declares a goal and a **required** rubric, and
the executing agent iterates under an **independent grader** until the rubric is
satisfied, the iteration budget is exhausted, grading fails, or the user
interrupts. The facade needs first-class wire events, a persisted state machine,
and a grader that never shares the executor's context window.

## Wire shape

### Inbound: `user.define_outcome`

Sent through the existing `POST /v1/sessions/{id}/events` batch:

```json
{
  "events": [{
    "type": "user.define_outcome",
    "description": "Build a DCF model for Costco in .xlsx",
    "rubric": {"type": "text", "content": "# DCF Model Rubric\n..."},
    "max_iterations": 5
  }]
}
```

- `description` — required, non-blank goal text. (The docs name this field
  `description`, not `goal`.)
- `rubric` — required object. `{"type":"text","content":"..."}` is supported.
  `{"type":"file","file_id":"..."}` is **rejected** with 400: the Files API is a
  documented non-goal of this facade.
- `max_iterations` — optional. `0`/absent resolves to the facade default
  (**3**, mirroring the docs); negative or greater than the configured cap
  (default **20**, mirroring the docs' max) is rejected with 400.

The event is persisted and echoed back on the event list / SSE stream with a
facade-generated `outcome_id` (`outc_…`), the resolved `max_iterations`, and a
`processed_at` timestamp. A session accepts a new outcome only when no outcome
is active (pending / running / evaluating); a concurrent `define_outcome` is
rejected with 409.

### Outbound: evaluation spans

```
span.outcome_evaluation_start    {id, outcome_id, iteration, processed_at}
span.outcome_evaluation_ongoing  {id, outcome_id, iteration, processed_at}
span.outcome_evaluation_end      {id, outcome_id, outcome_evaluation_start_id,
                                  iteration, result, explanation, usage,
                                  processed_at}
```

- `iteration` is the 0-indexed revision counter from the docs: `0` is the first
  evaluation, `1` the first re-evaluation after revision feedback, and so on.
  Serialized even when `0` (pointer field).
- `outcome_evaluation_start_id` links an end event to its start; it is the
  empty string when an outcome is interrupted before any evaluation started
  (docs-blessed edge case). Serialized as `""` in that case (pointer field).
- `result` ∈ `satisfied | needs_revision | max_iterations_reached | failed |
  interrupted`.
- `explanation` is the grader's user-safe summary. Structured per-criterion
  results live on the session read model, not the event (the docs' end event
  carries `explanation` only).
- `usage` is emitted as a zero-valued object: the ahsir chat transport does not
  report grader token usage, and the SDK model requires the integer fields when
  the object is present.
- `ongoing` is a heartbeat: emitted when an evaluation attempt begins, before
  each retry, and on a configurable interval while the grader runs. It carries
  no grader payload — the grader's reasoning is opaque by design.

### Session read model

`GET /v1/sessions/{id}` gains `outcome_evaluations[]` — one entry per outcome
defined on the session, oldest first (chained outcomes accumulate):

```json
{
  "outcome_id": "outc_…",
  "description": "…",
  "rubric": "…",
  "max_iterations": 5,
  "iteration": 1,
  "result": "evaluating",
  "explanation": "…",
  "criteria": [{"criterion": "…", "passed": false, "feedback": "…"}],
  "deliverable": "…",
  "created_at": "…", "updated_at": "…", "completed_at": null
}
```

`result` on the read model doubles as the outcome state: `pending | running |
evaluating` while active, then a terminal `satisfied | max_iterations_reached |
failed | interrupted`. `deliverable` is the final executor reply (truncated to
the configured cap) — the v1 readable artifact together with the structured
grader summary (`criteria` + `explanation`). No file artifacts: the sandbox /
Files API is a non-goal, so no host paths are ever exposed.

## State machine

```
                 user.define_outcome
                        │
                   pending ──(turn starts)──► running ──(turn done)──► evaluating
                        │                       │                          │
                        │                       │                     grader verdict
                        │                       │                    ┌────┴─────┐
                        │                       │               satisfied   needs_revision
                        │                       │                  │       budget left? ──yes──► running (iteration+1)
                        │                       │                  │       no
                        │                       │                  │  max_iterations_reached
                        │                       │                  │  → one final acknowledgment
                        │                       │                  │    turn (no evaluation) → idle
                        │                       │             grader error ×2 → failed → idle
                        │                executor error → failed (session terminated)
                        └── user.interrupt at any point → interrupted → idle
```

- One active outcome per session. A new `user.define_outcome` is accepted only
  after the previous outcome reached a terminal result.
- Each iteration is a normal executor turn (`session.status_running` →
  `agent.message`); the session stays `running` across grading and goes `idle`
  (`end_turn`) when the outcome terminates. Queued `user.message` turns run
  after the outcome completes (the session's serial turn queue).
- Revision turns re-send goal + rubric plus the grader's explanation and failing
  criteria; the executor never sees grader internals beyond those safe fields.
- On `max_iterations_reached` the facade runs one final acknowledgment turn
  (docs: "one final acknowledgment turn follows before the session transitions
  to idle"), whose reply replaces the deliverable, then goes idle. The
  acknowledgment turn is best-effort: its failure does not change the recorded
  result.

## Grader interface and provisioning

```go
type OutcomeGrader interface {
    Grade(ctx context.Context, req GradeRequest) (*cma.OutcomeGrade, error)
}
```

`GradeRequest{OutcomeID, Goal, Rubric, Iteration, Response}`. The default
implementation is ahsir-backed:

- A dedicated grader agent `cma-grader-<agent>-v<version>` is registered with
  the same runtime credentials/model as the session's agent but a grader system
  prompt, filesystem disabled, shell off, network off.
- Every `Grade` call uses a **fresh `contextId`** (`gctx_…`), so the grader's
  context window contains only goal + rubric + the deliverable under
  evaluation. The executor's conversation is never replayed into the grader.
- The grader is instructed to answer with only a JSON object
  (`{verdict, criteria[], explanation}`); the parser tolerates markdown fences
  and surrounding prose, validates the verdict, and caps field sizes. Only the
  parsed, sanitized fields reach the event stream and read model — raw grader
  output (including any hidden reasoning) is never emitted.
- A grader call that errors or returns unparseable output is retried **once**
  with another fresh context; a second failure ends the outcome `failed`.
- Grader agents are reclaimed together with the executor agent when the last
  referencing session is archived/deleted.

## Persistence and recovery

Outcome state lives on the persisted session (`outcome_evaluations`), updated
copy-on-write under the session record lock and persisted on every transition,
alongside the event log. All `span.outcome_evaluation_*` events are in the same
persisted log, so the events list and SSE stay consistent.

On facade startup, any outcome still in a non-terminal state (`pending`,
`running`, `evaluating`) is transitioned to `failed` with an explanatory end
event: after a restart the facade cannot know whether the executor or grader
finished, and an uncertain outcome must never be reported as `satisfied`.
In-flight cancellation handles are transient and are simply absent after a
restart.

## Interrupt

`user.interrupt` (1) cancels the in-flight executor task (existing
`tasks/cancel` path) and (2) cancels the active outcome context, which aborts a
running executor stream or grader call. The outcome loop is the single writer
of terminal outcome state: it observes the cancellation, records `interrupted`
(with an end event whose `outcome_evaluation_start_id` is `""` if no evaluation
had started), and returns the session to idle. Interrupting a session with no
active outcome is unchanged. Session archive/delete also cancels the outcome.

## Errors, permissions, resource boundaries

| Boundary | Rule |
|---|---|
| Blank `description` / blank rubric content | 400, nothing persisted |
| `rubric.type: "file"` | 400 (Files API not implemented) |
| `max_iterations` < 0 or > cap | 400; `0`/absent → facade default 3 |
| Outcome already active | 409 |
| description / rubric size | capped (16 KiB / 64 KiB), 400 beyond |
| Executor reply stored / graded | truncated to 256 KiB with a marker |
| Grader reply | verdict validated; ≤100 criteria; per-field truncation |
| Events per outcome | bounded by `max_iterations` × constant per-iteration events |
| Grader failure | one retry with a fresh context, then `failed` |
| Auth | unchanged: the facade's x-api-key allowlist gates every route |

### Wait semantics (explicit, per issue)

There is **no true infinite wait**. `0` for a duration knob never means
"forever" and never silently degrades to a short timeout: `0` selects the
documented default (`CMA_TURN_TIMEOUT` ships as 720 h via `ahsir start`;
`CMA_GRADER_TIMEOUT` defaults to 5 min). An outcome has no overall wall-clock
budget of its own; its total duration is bounded by `max_iterations` × per-turn
timeout plus grading time. Operators wanting long-running outcomes set explicit
large durations.

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `CMA_OUTCOME_MAX_ITERATIONS` | 20 | hard cap on requested `max_iterations` |
| `CMA_GRADER_TIMEOUT` | 5m | per grading call (0 → this default) |
| `CMA_OUTCOME_HEARTBEAT_INTERVAL` | 15s | `ongoing` heartbeat cadence; negative disables |

Description/rubric/deliverable byte caps are facade constants (overridable in
`cmagateway.Config` for embedders).

## Test plan

Failing-first tests in `internal/cmagateway/outcome_test.go` and
`grader_test.go`: satisfied flow; needs_revision → satisfied (feedback reaches
the revision turn); max_iterations → acknowledgment turn → idle; grader failure
after retry → failed; invalid grader JSON → failed; interrupt during executor
turn and during grading; concurrent define → 409; chained outcome after a
terminal one; validation rejections persist nothing; restart recovery marks
active outcomes failed (never satisfied); SSE stream equals events list; grader
hidden reasoning never appears in events or read model; grader runs under its
own agent name with a fresh contextId; pending/running/evaluating visible on
the read model.
