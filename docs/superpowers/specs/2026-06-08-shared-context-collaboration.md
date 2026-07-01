# Shared-Context Collaboration: Speaker Attribution, FIFO Queueing, Transcript

## Background

A contextId is a bare string key: any client that can reach the scheduler and
sends the same contextId joins the same provider session. Mechanically, sharing
already works — verified by live experiment (2026-06-08, anthropic/haiku agent,
independent `ahsir chat` processes):

1. **Sequential sharing works.** Client B (separate process) reads state client
   A established in the same context.
2. **Attribution fails silently.** A control run with no multi-speaker framing:
   Alice states "my favorite color is red"; Bob (different person) asks "what
   is MY favorite color?" → the model answers "Red." with no warning. Only when
   a speaker self-declared ("I'm a DIFFERENT person") did the model
   disambiguate — prompt luck, not a mechanism.
3. **Concurrency is fail-fast only.** A second request during an in-flight turn
   gets HTTP 409 (specs/2026-06-07-same-context-busy-semantics.md). Correct for
   one user, hostile for several: every collision becomes a human retry.
4. **History is unreadable.** `ahsir trace` shows invocation metadata plus a
   512-byte userText preview — no agent replies, no speaker. The full
   conversation exists only inside the provider session's private format.
   Someone taking over a context cannot replay what happened in it.

The busy-semantics spec deferred queueing with the note that it "requires
caller identity injection, queue bounds, a `queued` ledger state, and ingress
auth to be meaningful". Decision (2026-06-08, user-confirmed): split that
dependency. The **domain model** (speaker attribution, queueing, transcript)
lands now for same-machine multi-client use; **verified identity, context ACLs,
and any cross-machine access** stay gated behind the ingress-auth baseline from
docs/reviews/2026-06-07-adversarial-review.md. The trust boundary for this spec
is the local machine: anyone who can reach 127.0.0.1 is trusted, speakers are
self-claimed.

Confirmed product decisions:

- Speaker identity: `--as` flag, defaulting to the OS username.
- Waiting model: enqueue returns an A2A task; the CLI wraps it — synchronous by
  default (CLI polls internally), `--async` hands the taskId to the user.
- Transcript: on by default, full content, `0600` on disk.

## Goals

- **Speaker attribution**: every message carries a `speaker`; the provider
  session sees `[speaker: X]` tags and a standing multi-speaker instruction;
  ledger and `ahsir trace` show who said what.
- **Per-context FIFO queueing**: a request landing on a busy context waits in
  arrival order instead of failing. The queue is bounded; overflow degrades to
  the existing 409 busy contract (wire-compatible — busy becomes the
  backpressure signal instead of the collision signal).
- **Task-based waiting**: enqueue immediately yields an A2A task in `submitted`
  state via the standard `configuration.blocking=false`; callers poll
  `tasks/get`. The in-memory TaskStore and `tasks/get` already exist; no
  persistent store is pulled forward.
- **Per-context transcript**: the wrapper appends every turn — speaker, full
  user text, full reply — to an append-only jsonl under the agent workspace,
  `0600`. `ahsir history` replays it so a person joining a context can read
  what happened before their first turn.

## Non-Goals

- No verified identity. `speaker` is self-claimed; spoofing is in-scope for the
  auth phase, not this one. The field is positioned so principal substitution
  later changes its *source*, not its shape.
- No context ACLs, no "join"/"create" APIs — contexts remain
  created-on-first-use. ACL metadata gets added when auth lands.
- No cross-machine access. Non-loopback `network.bind` stays rejected.
- No real-time subscription. The transcript is an event log that a future SSE
  tail can serve; this spec only does replay.
- No persistent TaskStore. Queued tasks die with the agent process; the queue
  is memory-only and that is acceptable at this trust level.
- No fairness/priority scheduling — strict arrival order only.

## Design

### Speaker attribution

- `ahsir chat` gains `--as <name>`; empty defaults to the OS username
  (`os/user.Current()`, fallback `$USER`). The value travels as A2A
  `Message.Metadata["speaker"]`.
- The scheduler chat endpoint forwards the field; the ledger records it
  (`speaker` column in `started` events) and `ahsir trace` prints it in place
  of today's constant `chat_gateway` source where present.
- The wrapper executor prepends `[speaker: <name>]` to the user text it feeds
  the provider session whenever the metadata is present, and appends one
  standing line to the system prompt: messages may come from multiple people,
  each tagged `[speaker: …]`; attribute statements and preferences to the
  speaker who made them.
- Absent metadata (old clients, internal callers) behaves exactly as today —
  no tag, no system-prompt addition on contexts that never saw a speaker.

### Per-context FIFO queue (session pool)

- The turn gate lives where `ErrTurnInFlight` is born: the per-context entry in
  the session pool. Each entry gets a bounded FIFO of waiters; a turn acquires
  the gate in arrival order.
- Bound: `pool.queue_depth` in the agent card (default 4; `0` restores today's
  fail-fast). A request arriving with the queue full fails with
  `ErrTurnInFlight` — same sentinel, same `agent busy:` wire marker, same
  gateway 409 mapping. The busy e2e contract stays valid with depth 0.
- A queued waiter whose caller context is cancelled (client gone, deadline
  passed) leaves the queue without running. A task cancelled via `tasks/cancel`
  while queued is skipped at dequeue.
- Once dequeued, the turn runs under the existing per-turn timeout
  (`runtime.timeout`). Queue wait time does not consume turn timeout.
- Internal delegation (executor sub-agent calls through the scheduler) keeps
  blocking semantics and now waits in the queue instead of colliding — a
  delegation hitting a busy specialist queues rather than erroring.
- Ledger: a queued invocation records a `queued` event between `started` and
  terminal, so `ahsir trace` makes waiting visible.

### Task-based waiting

- `message/send` honours the standard A2A `configuration.blocking` field
  (already in the vendored SDK): `blocking=false` saves a `submitted` task,
  enqueues, and returns immediately; the task moves `submitted → working →
  completed|failed` as the queue drains. Default (`blocking` unset/true) keeps
  today's hold-until-done semantics, now queueing instead of 409.
- `tasks/get` already works through the verbatim JSON-RPC proxy at
  `/a2a/{name}`; no new gateway surface is required for polling.
- CLI:
  - default: send `blocking=false`, poll `tasks/get` with backoff until
    terminal, print the result — synchronous UX, no long-held HTTP connection,
    works for arbitrarily long queue waits.
  - `--async`: print the taskId and exit; the existing
    `ahsir status <agent> <taskId>` fetches state/result later.
- **Restart degradation path**: tasks are memory-only — after an agent process
  restart, a previously issued taskId answers 404. That loses only the query
  handle, not the conversation: sessions.json preserves resume, the transcript
  preserves content. The sanctioned client fallback on 404 is `ahsir history
  <agent> <contextId>`: if a record for the submitted turn is present, the turn
  executed (read its reply there); if absent, it never ran and is safe to
  resend. The CLI bakes this in — on a 404 while polling, it exits with an
  error that names the history command instead of a bare "task not found".

### Transcript

- Writer: the wrapper executor, at turn end (success and failure), appends one
  jsonl record to `<workspace>/.a2a/transcripts/<file>.jsonl`:

      {"ts":"…","turn":3,"taskId":"…","speaker":"alice",
       "userText":"…full text…","reply":"…full text…",
       "status":"completed","durationMs":24973}

  Directory `0700`, file `0600`, append-only, no rotation in v1.
- **Filename safety**: contextId is caller-supplied and must never reach the
  filesystem raw. File name is `hex(sha256(contextId))[:16]`; an `index.json`
  (also `0600`) maps contextId → file for listing.
- **Privacy stance**: this deliberately reverses the ledger's 512-byte-preview
  decision *for this one file class*: full content is the point — replay for
  whoever takes over the context. Scope stays inside the agent workspace with
  owner-only permissions; the API is the sanctioned access path (and the place
  ACLs attach later). Documented in README alongside the ledger notes.
- Read path: the wrapper serves `GET /history?contextId=…` (internal-token
  protected, like all wrapper ingress); the scheduler proxies it as
  `GET /agents/{name}/history/{contextId}`; `ahsir history <agent> <contextId>`
  renders turns with speaker and timestamps. The scheduler does not read
  workspace files directly — file ownership stays with the wrapper.

### Future seams (explicitly designed-for, not built)

- `speaker` self-claim → verified principal from ingress auth; same field.
- Context ACL checks attach to the gateway chat/history endpoints.
- Transcript SSE tail (`?follow=true`) for group-chat visibility.
- Persistent TaskStore if queued-task survival across restarts is ever needed.

## Acceptance Criteria

- Unit: executor injects `[speaker: X]` and the system-prompt line exactly when
  speaker metadata is present; absent metadata produces byte-identical provider
  input to today.
- Unit: session-pool queue — N concurrent turns on one context all complete in
  arrival order; depth+1 concurrent waiters → exactly one `ErrTurnInFlight`;
  cancelled waiters and cancelled tasks never execute; distinct contexts stay
  parallel.
- Unit: `blocking=false` returns a `submitted` task whose `tasks/get` converges
  to the same terminal state and text as the blocking path.
- Unit: transcript writer — record per turn including failures, `0600`/`0700`
  modes, contextId never appears raw in a path, index maps back correctly.
- E2E (extends the busy-semantics scenario): two clients with different `--as`
  values interleave on one context; both turns complete in order (no 409 at
  default depth); `ahsir history` shows both turns with correct speakers and
  full replies; `ahsir trace` shows the second invocation passing through
  `queued`.
- E2E: `--async` returns a taskId while the turn is queued; polling reaches
  `completed` with the reply.
- `make test` (race suite) passes — the queue is new shared state under
  concurrency.

## Implementation Order

Three independently landable slices, in this order:

1. **Speaker attribution** — smallest, no concurrency surface, immediately
   fixes the silent-misattribution failure.
2. **Transcript + history** — independent of queueing; unblocks the takeover
   workflow.
3. **Queue + task waiting + CLI modes** — largest; replaces the 409-first
   contract and touches the session pool's locking.
