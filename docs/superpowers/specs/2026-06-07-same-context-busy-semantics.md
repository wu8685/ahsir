# Same-Context Concurrency: Busy Semantics

## Background

A contextId keys one long-lived provider session (one `claude` process / one
Codex thread). Provider turns are physically serial: the session's stdin/stdout
is a single NDJSON stream with no turn multiplexing. Today a second request
arriving while a turn is in flight on the same contextId is rejected deep in the
session layer with the internal message `previous turn not drained`, which
surfaces to users as an opaque 502. The behaviour is correct (fail fast, first
request unaffected) but the contract is undefined: no typed error, no dedicated
HTTP status, no test pinning it, and the wording is wrapper jargon.

A future queueing mode ("shared collaborative context") was considered and
deliberately deferred: it requires caller identity injection, queue bounds, a
`queued` ledger state, and ingress auth to be meaningful. This spec covers only
the fail-fast contract.

## Goals

- Define one typed sentinel for "a turn is already in flight on this context"
  shared by all session providers.
- Surface a human-readable busy message that names the contextId and tells the
  caller what to do (retry after the current turn completes).
- Map the busy condition to HTTP 409 Conflict on the scheduler's
  `/agents/{name}/chat` endpoint.
- Keep the first (in-flight) request completely unaffected.
- Pin the contract with unit tests and an e2e case: concurrent same-context
  requests yield exactly one success and one busy rejection, and a retry after
  completion succeeds with conversation memory intact.

## Non-Goals

- No per-context queueing or waiting (deferred to a follow-up that lands with
  ingress auth and caller identity).
- No change to the A2A `/a2a/{name}` proxy shape: JSON-RPC errors relay
  verbatim; the busy text is carried in the JSON-RPC error message.
- No change to distinct-context parallelism.
- No retry logic in the CLI; it reports the busy message and exits non-zero.

## Design

### Typed sentinel (wrapper)

`internal/wrapper` exports `ErrTurnInFlight`. `ClaudeSession.Stream` and
`CodexSession.Stream` return an error wrapping it (`errors.Is`-able) instead of
their current ad-hoc strings. The wrapped text reads:

    agent busy: a turn is already running for this conversation; retry after it completes

The executor propagates session errors with `%w`, so `errors.Is(err,
wrapper.ErrTurnInFlight)` holds at the A2A server layer inside the agent
process.

### Wire marker

Error types do not cross the A2A process boundary; the stable marker is the
literal substring `agent busy:` in the error message. The agent's JSON-RPC
error message carries it; the scheduler detects it by substring. This is the
same pragmatic pattern the gateway already uses for "agent not found".

### Gateway mapping

`handleChat` returns HTTP 409 with a JSON error body when the upstream error
contains the busy marker:

    {"error":"agent busy: ... (contextId \"X\"); retry after the current turn completes"}

The ledger records the rejected invocation as `failed` with the busy message —
trace must show that the collision happened.

### CLI

`ahsir chat` keeps its existing behaviour (print error to stderr, exit 1); the
message is now actionable because the body text explains the retry semantics.

## Acceptance Criteria

- Unit: ClaudeSession and CodexSession concurrent Stream returns an error
  satisfying `errors.Is(err, ErrTurnInFlight)` and containing `agent busy:`.
- Unit: gateway `handleChat` maps a busy upstream error to HTTP 409 with the
  busy text in the JSON body; non-busy errors keep their current mapping.
- E2E: two concurrent `message/send` on one contextId → exactly one success,
  one JSON-RPC error containing `agent busy:`; a third request after completion
  succeeds and retains conversation memory from the successful turn.
- `make test` (race suite) passes.
