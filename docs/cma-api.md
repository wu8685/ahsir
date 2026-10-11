# CMA Facade API

ahsir can serve an [Anthropic Managed Agents](https://platform.claude.com/docs/en/managed-agents/overview)
compatible HTTP surface (beta header `managed-agents-2026-04-01`) alongside the
scheduler, so the official `anthropic` SDK — pointed at this base URL — drives
ahsir agents as a drop-in. Start it with:

```bash
ahsir start ahsir.yaml --cma-listen 127.0.0.1:18790
```

The facade speaks the CMA wire and drives the local scheduler over loopback.
State (agents, environments, sessions, event logs, outcomes) persists to a JSON
state file and survives restarts. Authentication: when the facade is configured
with an `x-api-key` allowlist every route requires it; by default (local,
zero-config) all routes are open.

Resources: `POST/GET /v1/agents`, `POST/GET /v1/environments`,
`POST/GET /v1/sessions`, `POST/GET /v1/sessions/{id}/events`, and the SSE tail
`GET /v1/sessions/{id}/events/stream`. This document covers the
outcome-oriented surface added in issue #36; the design rationale lives in
[docs/specs/issue-36-cma-outcome-sessions.md](specs/issue-36-cma-outcome-sessions.md).

---

## Outcome-driven sessions

An outcome tells the session what "done" means: a goal plus a **required**
rubric. The executor agent iterates under an independent grader until the
rubric is satisfied, the iteration budget runs out, grading fails, or the
outcome is interrupted. Wire shapes follow
[Define outcomes](https://platform.claude.com/docs/en/managed-agents/define-outcomes).

### Define an outcome

```bash
curl -fsSL "http://127.0.0.1:18790/v1/sessions/$SESSION_ID/events" \
  -H "x-api-key: $KEY" \
  --json @- <<EOF
{
  "events": [{
    "type": "user.define_outcome",
    "description": "Build a DCF model for Costco in .xlsx",
    "rubric": {"type": "text", "content": "# DCF Model Rubric\n- Uses 5 years of historical revenue\n- ..."},
    "max_iterations": 5
  }]
}
EOF
```

- `description` — required, non-blank.
- `rubric` — required. Only `{"type":"text","content":...}` is supported;
  `{"type":"file","file_id":...}` is rejected (400) because the Files API is a
  non-goal of this facade.
- `max_iterations` — optional; `0`/absent resolves to **3**, the configured cap
  is **20** (`CMA_OUTCOME_MAX_ITERATIONS`). Negative or over-cap values are
  rejected with 400.
- The event is persisted and echoed on the event list / stream with a generated
  `outcome_id` (`outc_…`) and the resolved `max_iterations`.
- One active outcome per session: while an outcome is `pending` / `running` /
  `evaluating`, another `user.define_outcome` is rejected with **409**. After a
  terminal end event, a new outcome can be defined on the same session; history
  accumulates in `outcome_evaluations[]`.

### Observe progress

Three span events are persisted and pushed over SSE:

```
span.outcome_evaluation_start    {id, outcome_id, iteration, processed_at}
span.outcome_evaluation_ongoing  {id, outcome_id, iteration, processed_at}   # heartbeat while grading
span.outcome_evaluation_end      {id, outcome_id, outcome_evaluation_start_id,
                                  iteration, result, explanation, usage, processed_at}
```

- `iteration` is 0-indexed: `0` is the first evaluation, `1` the first
  re-evaluation after revision feedback.
- `result` ∈ `satisfied | needs_revision | max_iterations_reached | failed |
  interrupted`. On `max_iterations_reached` the facade runs one final,
  unevaluated acknowledgment turn before the session goes idle.
- `outcome_evaluation_start_id` links an end to its start; it is `""` when the
  outcome was interrupted before any evaluation began.
- `usage` is a zero-valued object — the ahsir transport does not report grader
  token counts.
- The grader's internal reasoning is opaque by design: `ongoing` proves
  liveness, `explanation` carries only the user-safe summary.

### Read the outcome state

```bash
curl -fsSL "http://127.0.0.1:18790/v1/sessions/$SESSION_ID" -H "x-api-key: $KEY" | jq .outcome_evaluations
```

```json
[{
  "outcome_id": "outc_…",
  "description": "Build a DCF model for Costco in .xlsx",
  "rubric": "# DCF Model Rubric\n- …",
  "max_iterations": 5,
  "iteration": 1,
  "result": "evaluating",
  "explanation": "…",
  "criteria": [{"criterion": "Uses 5 years of historical revenue", "passed": true}],
  "deliverable": "…",
  "created_at": "…", "updated_at": "…", "completed_at": null
}]
```

While active, `result` is `pending` (accepted, first turn not yet running),
`running` (executor turn in flight), or `evaluating` (grader running). The
terminal entry carries the structured grader summary (`criteria` +
`explanation`) and the `deliverable` — the final executor reply, the v1
readable artifact. No file artifacts are exposed: there is no sandbox / Files
API here, so no host paths ever cross the wire.

### Interrupt

```bash
curl -fsSL "http://127.0.0.1:18790/v1/sessions/$SESSION_ID/events" \
  -H "x-api-key: $KEY" --json '{"events":[{"type":"user.interrupt"}]}'
```

`user.interrupt` cancels the in-flight executor task **and** the active
outcome. The outcome terminates as `interrupted` (with a terminal end event)
and the session returns to `idle`, ready for a new outcome.

### How grading works

The grader is a dedicated ahsir agent (`cma-grader-<agent>-v<version>`)
registered with the executor's runtime model/credentials but a grader system
prompt, no tools, no filesystem, no shell, no network. Every grading call uses
a fresh `contextId`, so the grader's context window contains exactly one
goal + rubric + deliverable — the executor's conversation is never replayed
into it. The grader answers with a JSON verdict; a call that errors or returns
unparseable output is retried once (fresh context), then the outcome ends
`failed`. Raw grader output never appears in the event stream or read model.

### Restart semantics

Outcome state and every evaluation event persist on every transition. If the
facade restarts while an outcome is active (`pending` / `running` /
`evaluating`), startup recovery marks it `failed` with an explanatory end
event: the facade cannot know whether the executor or grader finished, and an
uncertain outcome is never reported as `satisfied`.

### Limits and timeouts

| Knob | Default | Notes |
|---|---|---|
| `max_iterations` | 3, cap 20 (`CMA_OUTCOME_MAX_ITERATIONS`) | bounds executor turns and grading calls |
| `CMA_GRADER_TIMEOUT` | 5m | per grading call |
| `CMA_OUTCOME_HEARTBEAT_INTERVAL` | 15s | `ongoing` cadence while grading; negative disables |
| `CMA_TURN_TIMEOUT` | 720h (shipped default) | per executor turn |
| description / rubric | 16 KiB / 64 KiB | 400 beyond |
| deliverable stored / graded | 256 KiB | truncated with a marker |
| grader criteria | 100 entries, fields truncated | sanitation at parse |

**No infinite waits.** A `0` duration never means "wait forever" and never
silently degrades to a short timeout: `0` selects the documented default above.
An outcome's total duration is bounded by `max_iterations` × per-turn timeout
plus grading time; set explicit large values for long-running outcomes.
