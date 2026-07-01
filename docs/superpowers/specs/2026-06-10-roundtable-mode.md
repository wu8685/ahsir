# Spec: Roundtable mode (consensus rounds)

- Status: implemented (engine + gateway + persistence + console; CLI deferred)
- Date: 2026-06-10
- Naming: this **roundtable** mode IS the real round-table. The shipped
  @-mention/relay flow is now **多 Agent 协同 (multi-agent collaboration)**. Code:
  `RoomMode` = `relay` | `roundtable` in `internal/scheduler/rooms.go`.
- Surfaces: scheduler (engine `driveRoundtable` + moderator/judge call), gateway
  (`POST /rooms` `mode:"roundtable"`), persistence (`roomstore` meta), console
  (圆桌 create form). CLI deferred (ships together with this).
- Relation: a second **mode** alongside relay
  (`docs/superpowers/specs/2026-06-09-roundtable-group-chat.md`).

## Motivation

The shipped roundtable is moderator-driven serial **relay** (hub-and-spoke): a
chair / `@`-pointer picks the next speaker — "the chair relays to the people it
cares about, one by one." Good for *delegation*; not a real roundtable.

A **real roundtable** is flat (no head of the table), everyone shares the same
field of view, and the floor is not assigned by a biased chair. Its job is
**deliberation**: diverse independent perspectives that interact and **converge
to a collective decision**. It solves a different problem than relay:

| | relay (shipped) | deliberation (this spec) |
| --- | --- | --- |
| good for | routing a subtask to expert X | a hard, ambiguous, trade-off decision |
| floor control | `@`-pointer / organizer picks | random round-robin, no chair |
| ends when | a mention chain runs out | a full round reaches consensus |
| output | the relayed answer | a moderator-written summary/decision |

Driving scenario: **`mindpowers OKR 设计`** — proposers (`strategist`,
`architect`, `delivery-coach`) and red-team critics (`kr-redteam`,
`measure-redteam`) deliberate an OKR and converge.

## Model — consensus rounds (Texas Hold'em betting rounds)

Same shared-context substrate as relay (one `contextId` = the room id,
speaker-attributed turns, persisted transcript). The mechanic:

1. **Shared broadcast.** On its turn, every participant sees the **full thread**.
2. **A round = one full pass around the table**, in a **fresh random order** each
   round. Every participant is `@`-ed in turn and takes exactly one turn — like a
   betting round where the dealer goes around once.
3. **Serial within a round.** One speaks at a time; a later speaker sees
   everything so far **including earlier turns of the same round** (the existing
   `deltaLocked` already feeds this — each turn is appended before the next is
   consulted).
4. **Speak or agree.** A participant either **contributes** (an objection /
   addition = a "raise") or, with no objection, **agrees** (a "check"). By
   convention its persona answers `同意` when it has nothing to add.
5. **Consensus = a full round where everyone agrees** (no open objection). That
   ends the discussion.
6. **Objections reopen the NEXT round, not mid-round.** Finish the current pass;
   if anyone raised a point, run another full round so everyone (including those
   who already agreed) re-evaluates the new points. Poker-clean, bounded.
7. **No chair picks speakers.** Order is random each round; the moderator (below)
   only consolidates 已达成/待议 — it has no floor-control power. The table stays
   flat.

## Moderator agent (rolling consolidation)

A **dedicated agent** — not a participant, not the operator — that runs under its
**own contextId** (`<roomId>#mod`), so its calls never enter the shared
transcript nor pollute any participant's session. **One call per round.**

Instead of a hidden CONSENSUS/CONTINUE verdict, each round the moderator produces
a **rolling consolidation** — a running, accumulating decision ledger — split
into three columns:

- **【已达成】 (locked).** Points the table has accepted. They **carry forward**
  and are **not relitigated** in later rounds — unless a new argument breaks an
  assumption one of them rests on, in which case the moderator moves it back to
  待议 and notes why. The list **grows** each round.
- **【待议】 (open).** Points still in dispute. The list **shrinks** each round as
  things get resolved into 已达成.
- **【纠偏】 (steering).** The moderator also **reviews for drift**: if a
  participant's reasoning wandered off the proposition, over-diverged, or burrowed
  into decision-irrelevant detail, it **names them** (`@<name>`) with a one-line
  redirect. Next round, the per-turn prompt tells a named participant to heed it
  and reconverge. This keeps the table on-topic, not just converging.

The reply ends with a single verdict line — **CONSENSUS** (待议 empty / fully
agreed) or **CONTINUE** — which the engine parses (semantic judgment, robust to a
rephrased "同意"). The verdict line is stripped; the rest of the consolidation is
appended as a **visible** turn that **anchors the next round** (participants see
it via the delta and are told to push only the open points). On convergence the
last consolidation's 【已达成】 *is* the decision — no separate synthesis call.

Crucially the moderator has **no say in who speaks** (that is random round-robin),
so it is a facilitator/recorder, not the biased chair the relay model is.
Implemented with the existing `run` call: same function, a `<roomId>#mod`
contextId; the verdict-stripped consolidation is appended as a turn (the only
moderator output that enters the thread). No new call path is needed.

## Termination

- **Consensus** (a CONSENSUS consolidation) → park for operator; the last
  consolidation's 已达成 is the conclusion.
- **Budget** (default **12 rounds**) without consensus → park for operator (never
  force agreement); the latest consolidation shows what's locked vs still open.
- **Operator continuation.** A new operator message (`Say`) poses a new question
  → resets the cycle and starts fresh rounds.

## Cost

Per round: N participant turns (short once converging — mostly `同意`) + **1**
moderator consolidation call. No separate synthesis call. Bounded by the budget.
The consolidation also *drives* convergence: locking agreed points out of the
discussion is what stops critics from re-sprawling each round.

## The "同意" convention (per-turn prompt, zero agent config)

The convention — *"若对当前议题无异议，只回复『同意』；有异议或补充则直接说出观点，不要加『同意』
前缀。"* — is injected by the engine into **each turn's prompt** (`deltaLocked`'s
roundtable branch), NOT into the agents' system prompts. So roundtable works with
any existing agent, no card/persona changes. It only nudges phrasing anyway — the
moderator's consensus judgment is semantic, so a paraphrased agreement still
counts.

## Code landing points (as built, in `internal/scheduler/rooms.go`)

| Concern | Relay (unchanged) | Roundtable |
| --- | --- | --- |
| Room model (`Room`) | relay fields | `Mode: relay\|roundtable`, `Moderator`, `Budget`, round state (`order`, `cursor`, `round`, `roundStart`) |
| Turn model (`RoomTurn`) | — | `Round` (1-based; drives the UI 第 X 轮 dividers) |
| Floor control (`drive` / `scheduleNextLocked`) | mention→organizer→park | `driveRoundtable`: a **round queue** — pop the next from the current random permutation; when it empties, run the moderator consolidation → CONSENSUS or budget → park, else re-shuffle and start a new round |
| Broadcast/instruction (`deltaLocked`) | "your turn; @ to hand off" | roundtable branch: "push only the 待议 from the latest 小结; don't relitigate 已达成 unless you break an assumption; reply 同意 if none." No @-handoff. Same-round visibility already works |
| Moderator call | — | `roundtableConsolidate` → `run(moderator, roomId+"#mod", "system", prompt)`; verdict line parsed + stripped; the consolidation appended as a visible turn |
| Persistence (`roomstore` meta) | meta | persist `mode` + `moderator` + `budget`; a restored roundtable parks (round state resets, fresh round on the next operator question) |
| Entry (gateway `POST /rooms`, console) | relay only | `mode`/`moderator`/`budget`; console 「圆桌」 create form + 小结 card + 第 X 轮 dividers |

## Decisions (agreed with operator)

1. Model = **consensus rounds** (Texas Hold'em): a round is one full pass in a
   fresh **random order**; consensus = a round whose consolidation has no 待议.
2. The moderator runs a **rolling consolidation** per round (【已达成】/【待议】 +
   CONSENSUS/CONTINUE), semantically — never string match. The consolidation is
   appended as a visible turn and **anchors the next round** (agreed points are
   locked out of discussion; open points shrink each round).
3. **Objections reopen the next round** (finish the current pass first); a new
   argument can move a 已达成 point back to 待议.
4. Budget default **12 rounds** without consensus → park for the operator.
5. A dedicated **moderator agent** (not a participant) does the consolidation; it
   has **no** floor-control power and runs under `<roomId>#mod`.
6. Operator re-opens with a new question (reuse `Say`).
7. Later speakers see earlier same-round turns (via `deltaLocked`).
8. The `ahsir room` CLI ships together with this mode (still deferred).
9. The "同意" convention is injected per-turn — no agent system-prompt change.

## Test plan (TDD) — implemented

- **Round structure:** a round is a full pass over a (mock) permutation; a later
  speaker's `deltaLocked` includes earlier same-round turns (and, in round 2+,
  the previous round's consolidation).
- **Consensus:** a CONSENSUS consolidation parks the room with the consolidation
  as the final turn; moderator calls use `<roomId>#mod` and are not appended as
  participant turns (except the consolidation itself).
- **Multi-round:** a CONTINUE consolidation is appended and starts a fresh round.
- **Budget:** N rounds without consensus → park for the operator.
- **Operator continuation:** a new `Say` resets and starts a new cycle.
- **e2e:** a small `mindpowers`-shaped room (2 proposers + 1 critic + moderator)
  reaches consensus and a written summary within the budget.
