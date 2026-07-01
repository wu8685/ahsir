# Spec: Roundtable (multi-agent group chat)

- Status: drafting
- Date: 2026-06-09
- Surfaces: scheduler (engine), web console (UI). No wrapper changes.

## Motivation

Today the operator talks 1:1 with an agent; multi-agent collaboration only
happens implicitly via agent→agent delegation. We want a real **roundtable**:
several agents converse together in one shared thread, see each other's turns,
and can address one another — with a human (or an agent) moderating.

## Model

- A **room** hosts N registered agents in one conversation. The room **id is the
  shared A2A contextId**, so every participant's session and transcript key on
  it (reusing shared-context + speaker attribution; the executor already tags
  each message `[speaker: <name>]`).
- The room keeps a **merged transcript** of turns: `{speaker, text, mentions,
  ts}`. `speaker` is an agent name, or `operator` for a human message.

## Turn-taking: @-mention addressing

- Any message — an operator message OR an agent's reply — may address
  `@<name>`. The first mentioned participant becomes the **next speaker**. This
  is available to agents too: an agent can hand a question to a peer
  (`@teacher what do you think?`), forming an organic Q&A chain.
- **No @-mention → control returns to the room's `organizer`.** The organizer is
  either:
  - `operator` (the human, default): the room parks in `waiting` for the next
    operator message; or
  - a **participant agent** designated as moderator: that agent becomes the next
    speaker (it receives the discussion delta and decides who continues by
    @-mentioning someone, or concludes).
- **Self-organizer guard**: if the current speaker IS the organizer and it
  addressed no one, the room parks in `waiting` (the moderator had its turn to
  direct and declined) — prevents an organizer self-loop.

## Hearing each other: incremental relay

When it is an agent's turn, it is sent only the transcript entries it has not
consumed since it last spoke, attributed per line, plus a turn instruction
("轮到你发言；如需交给某位参与者请 @名字"). So each agent runs the LLM only on
its own turn (bounded cost, no broadcast storm) while staying in sync.

## Runaway guard

An autonomous agent→agent (or organizer↔speaker) chain is bounded by `MaxChain`
turns **between operator interventions** (default 8). Hitting the bound parks
the room in `waiting`. **Every operator message resets the budget**, so a human
can drive indefinitely while autonomous chains stay bounded.

## Lifecycle / status

- `active`  — a speaker is scheduled (`next` set); the drive loop runs.
- `waiting` — no speaker scheduled; awaiting an operator message.
- `stopped` — operator halted the room (terminal). Underlying agent sessions are
  untouched (the conversation survives; the room is just no longer driven).

Rooms are in-memory for v1 (lost on scheduler restart); the per-agent sessions
+ transcripts persist under the room contextId, so re-hydration is possible
later. Out of scope for v1.

> **Update (implemented):** room persistence later landed — `RoomStore` writes
> one `<roomId>.jsonl` per room and reconstructs them on restart (recovered
> rooms return as `waiting`; 30-day retention). See `internal/scheduler/roomstore.go`.

## HTTP API (scheduler gateway)

- `POST /rooms` — create. Body: `{topic, participants[], organizer?, maxChain?,
  message?}`. `organizer` defaults to `operator`; if it names an agent it must
  be a participant. `message`, if present, is posted as the operator (may
  @-mention the first speaker, kicking off the loop). 201 + RoomView.
- `GET  /rooms` — list (newest first).
- `GET  /rooms/{id}` — one RoomView (poll for live updates).
- `POST /rooms/{id}/say` — operator message. Body: `{text, speaker?}`. Resets
  the chain budget; an @-mention schedules the next speaker. RoomView.
- `POST /rooms/{id}/stop` — halt. RoomView.

These are read/dispatch routes (not control-plane); no admin token. The console
reaches them transparently via its existing `/api/*` reverse proxy.

## Console surface

A "圆桌" mode: pick participants + organizer + topic, an opening operator
message; a merged thread view (speaker-labelled, markdown-rendered, timestamps);
an operator say box (supports @mention); a stop button; live polling of the
RoomView.

## Decisions (agreed with operator)

- Engine lives in the **scheduler** (reusable by console + future CLI; console
  stays a thin proxy).
- Turn-taking is **@-mention**, usable by operator AND agents.
- Hearing each other via **incremental relay** (no wrapper changes).
- No-mention hands back to the **organizer**, which may be the user or a
  moderator agent.

## Test plan (TDD)

Engine (`roundtable_test.go`, fake turnFunc — no LLM):
1. @-mention chain: operator `@a` → a replies `@b` → b replies (no mention) →
   parks `waiting` (operator organizer). Assert transcript order + speakers.
2. Incremental relay: each agent's turn receives only unseen entries; verify the
   delta passed to the fake turnFunc grows by exactly the new turns.
3. Organizer = agent: no-mention turn hands the turn to the organizer agent (it
   becomes next speaker), not the operator.
4. Self-organizer guard: organizer agent with no mention → parks `waiting`.
5. Runaway guard: two agents @-mention each other; the autonomous chain stops at
   `MaxChain`, status `waiting`; an operator `say` resets and resumes.
6. Validation: non-registered participant rejected; `@stranger` (non-participant)
   ignored → organizer fallback.
7. Stop: terminal; further `say` rejected.

Gateway (`gateway_test.go`): create/say/stop/get over HTTP against a stub agent;
assert RoomView shape and that a `say` drives a turn end-to-end.
