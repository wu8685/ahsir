# AHSIR TODO

## Client Experience

- [ ] After a client sends chat to the scheduler and receives a response, support automatic voice playback on the client side.
  - Scope: client-side behavior after `ahsir chat` / scheduler chat response returns.
  - Design notes: keep scheduler response semantics unchanged; add an optional client feature flag/config so text output still works for non-audio environments.
  - Candidate implementation: macOS `say` first for local development, with an abstraction for other TTS backends later.
  - Requested: 2026-06-07.

## Multi-agent Collaboration

- [x] Provide a multi-agent group-chat ("roundtable") mode so several agents can converse together in one shared conversation, not just 1:1 with the operator.
  - **Implemented 2026-06-09.** Spec: `docs/superpowers/specs/2026-06-09-roundtable-group-chat.md`.
    Engine in `internal/scheduler/roundtable.go` (+ `roundtable_test.go`), HTTP
    `/rooms` in `gateway.go`, console "圆桌" mode in `internal/ui/assets/`.
    @-mention turn-taking (operator + agents), organizer fallback (user or
    moderator agent), incremental relay, MaxChain runaway guard. No wrapper
    changes. Verified end-to-end (teacher↔student @-mention chain).
  - Goal: a real roundtable — multiple agents see each other's turns in the same thread and can respond to one another, with the operator moderating.
  - Scope: builds on the existing shared-context model (one contextId chaining several agents) and speaker attribution (`--as`). Likely needs a turn-taking / addressing policy (who speaks next, @-mention or round-robin), fan-out of each turn to participants, and a transcript merged across agents (the per-agent history endpoint is currently 1:1).
  - Surfaces: scheduler (dispatch/turn-taking), wrapper (each agent must ingest peer turns into its session), and the web console (a group thread view + participant roster).
  - Open questions: turn-taking policy (round-robin vs moderator-driven vs free-for-all), termination/convergence, and how to bound cost when N agents all respond.
  - Requested: 2026-06-08.

- [ ] Expose the roundtable over the **CLI**, not just the HTTP `/rooms` API and the web console.
  - Background: the roundtable runtime ships (engine in `internal/scheduler/roundtable.go`, gateway `/rooms`, console "圆桌" mode), and the spec already calls the engine "reusable by console + **future CLI**". Today the CLI only has `chat <agent> --context <id>` (1:1); there is no `ahsir room` / `ahsir roundtable` verb. Consequence: from a terminal you cannot create a room, post an operator `say`, or list/inspect rooms — you must use the console or hand-curl `/rooms`. This surfaced while trying to rejoin the `mindpowers OKR 设计` roundtable (contextId `019eab36-bcd4-71fb-88a7-e365660693f4`) as operator purely from the CLI.
  - Scope: first-class **built-in** `ahsir room` verbs (not an external wrapper), reusing ahsir's own gateway client + admin-token resolution, same tier as `chat`/`list`/`trace` — `room list`, `room get <id>`, `room create {topic, participants, organizer?, message?}`, `room say <id> <text>`, `room stop <id>`, plus an interactive `room join <id>` (live thread + type to speak). The transport is necessarily the `/rooms` gateway (the engine lives in the scheduler process), but the verbs ship in the `ahsir` binary.
  - Correction: rooms **do persist** — `RoomStore` writes one `<roomId>.jsonl` per room and reconstructs them on scheduler restart (recovered rooms return as `waiting`; 30-day retention). Verified on disk: the `mindpowers OKR 设计` room (`019eab36-…`) lives at `~/.ahsir/.ahsir/rooms/`. So it can already be rejoined from the **console** today; only the CLI entry point is missing.
  - **Decision (2026-06-10):** deferred — the CLI ships **together with** the new roundtable *deliberation* mode, not before. The current relay model is being reworked into a real round-table (shared broadcast + single-speaker rounds + round-robin/interjection floor control + convergence/synthesis). See `docs/superpowers/specs/2026-06-10-roundtable-mode.md`.
  - Requested: 2026-06-10 (surfaced while participating in the `mindpowers OKR 设计` roundtable as operator).

