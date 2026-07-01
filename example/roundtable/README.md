# Example: Roundtable (multi-agent group chat)

A **roundtable** lets several agents talk together in one shared conversation —
they see each other's turns and can address one another — with a human (or an
agent) moderating. This example runs two opposing personas, **advocate** and
**critic**, debating a proposal you put on the table.

Unlike `multi-agent/` (where one agent *delegates* to another behind the
scenes), a roundtable is a **visible group thread**: every turn is recorded,
attributed, and shown in order. It builds entirely on the shared-context +
speaker-attribution machinery — the room id *is* the A2A `contextId`, so each
agent keeps its own session under it.

## Layout

```
roundtable/
├── ahsir.yaml                                    # scheduler + advocate + critic
├── workspaces/
│   ├── advocate/.a2a/agent-card.yaml             # optimistic advocate
│   └── critic/.a2a/agent-card.yaml               # skeptical critic
└── README.md
```

Neither agent has filesystem access, so this example runs anywhere — no
host-specific paths.

## Prerequisites

Same as the other examples (see [`../README.md`](../README.md)):

```bash
# From the repo root:
go build -o bin/ahsir ./cmd/ahsir
go build -o bin/ahsir-agent ./cmd/ahsir-agent
export ANTHROPIC_AUTH_TOKEN=<your-anthropic-key>
which claude
```

## 1. Start the scheduler

Run from the **repo root** (the workspace paths in `ahsir.yaml` are relative):

```bash
./bin/ahsir start example/roundtable/ahsir.yaml
```

Confirm both agents are up:

```bash
./bin/ahsir list
# advocate  http://127.0.0.1:9800/a2a/advocate  [advocacy, debate]
# critic    http://127.0.0.1:9800/a2a/critic    [critique, debate]
```

## 2. Drive a roundtable from the web console (recommended)

The console gives you a live group-thread view. In a second terminal:

```bash
./bin/ahsir ui --addr 127.0.0.1:9801 --scheduler http://127.0.0.1:9800
```

Open <http://127.0.0.1:9801>, click **圆桌 (Roundtable)**, then:

1. Pick participants: **advocate** and **critic**.
2. Choose an **organizer** — start with **我 (operator)**.
3. Topic: e.g. *"应该用单体还是微服务起步"*.
4. Opening message — address the first speaker with an `@`:
   *`@advocate 先说说微服务起步的理由，然后 @critic 反驳。`*
5. Send.

You'll see the turns stream in: advocate speaks and `@critic`s a question →
critic responds → … When a turn addresses no one, control returns to the
organizer (here, you) and the room parks in **等待主持 (waiting)** for your next
message. The right rail's **轨迹 (trace)** tab shows each turn's agent, who
addressed it, status, and duration.

To address a participant without typing the mention: hover any agent bubble and
click **直接回复** (drops `@<speaker>` into the composer), or drag a participant
row from the right rail onto the composer. Hover a bubble's **复制** to copy its
text.

## 3. Drive it over HTTP (no UI)

Everything the console does is plain HTTP on the scheduler gateway.

**Create a room** and open it addressing the advocate:

```bash
curl -s localhost:9800/rooms -H 'content-type: application/json' -d '{
  "topic": "单体还是微服务起步",
  "participants": ["advocate", "critic"],
  "organizer": "operator",
  "message": "@advocate 先讲讲微服务起步的理由，然后 @critic 反驳。"
}'
# -> {"id":"<roomId>", "status":"active", "next":"advocate", ...}
```

**Poll the room** (the merged transcript grows as turns complete):

```bash
curl -s localhost:9800/rooms/<roomId> | jq '.status, .speaking, [.transcript[] | {speaker, mentions}]'
```

- `status`: `active` (a turn is scheduled/running), `waiting` (parked for the
  organizer), or `stopped`.
- `speaking`: the agent whose turn is in flight right now (empty when idle).
- `next`: the scheduled next speaker (cleared the instant a turn starts).

**Add your own message** mid-discussion (resets the runaway-chain budget; an
`@` schedules the next speaker):

```bash
curl -s localhost:9800/rooms/<roomId>/say -H 'content-type: application/json' \
  -d '{"text": "聚焦在团队规模 5 人的场景。@critic 你怎么看？"}'
```

**Stop** the room when you're done:

```bash
curl -s -X POST localhost:9800/rooms/<roomId>/stop
```

List all rooms with `curl -s localhost:9800/rooms`.

## 4. Let an agent moderate

Set `organizer` to a participant agent instead of `operator`. Now, whenever a
turn addresses no one, control returns to **that agent** — it receives the
discussion so far and decides who speaks next (by `@`-mentioning someone) or
wraps up. Useful for hands-off discussions.

```bash
curl -s localhost:9800/rooms -H 'content-type: application/json' -d '{
  "topic": "评审这个 API 设计",
  "participants": ["advocate", "critic"],
  "organizer": "advocate",
  "message": "@advocate 你来主持这次评审，先给个开场，然后点名 critic。"
}'
```

(In the console, pick the agent in the **组织者** dropdown.)

## How turn-taking works

- **@-mention** is the addressing primitive, available to the operator *and* the
  agents. The first `@<participant>` in a message becomes the next speaker.
- A mention inside `inline code` or a ``` fenced block ``` is a *quoted* token,
  not a directive — an agent explaining the `@name` syntax won't accidentally
  hand off the turn.
- **No mention → control returns to the organizer.** Operator-organizer → the
  room parks for your next message. Agent-organizer → that agent takes the
  floor (unless it *is* the one who just spoke, to avoid a self-loop).
- **Incremental relay**: each agent's turn is fed only the transcript it hasn't
  seen since it last spoke (a first-time participant gets the whole history).
  So each agent runs the LLM only on its own turn — bounded cost, no broadcast
  storm — while staying in sync. Its own session retains everything up to its
  last turn; the delta fills the gap. No context is lost.
- **Runaway guard**: an autonomous agent↔agent chain is bounded by `maxChain`
  turns *between operator messages* (default 8). Hitting the bound parks the
  room for a human. Every operator `say` resets the budget, so you can drive
  indefinitely while autonomous chains stay bounded. Pass `"maxChain": N` on
  create to tune it.

## Observability

- `./bin/ahsir trace <roomId>` — the invocation timeline (each turn's agent,
  speaker, status, duration). Roundtable turns are tagged `source=roundtable`.
- `./bin/ahsir history <agent> <roomId>` — replay one agent's view of the
  room (what it was sent, what it answered).

## Notes & limits (v1)

- Rooms are **in-memory**: they're lost on scheduler restart. The per-agent
  sessions + transcripts persist under the room `contextId`, so the
  conversations themselves survive — only the room handle is gone.
- Cost scales with participants × rounds (each turn is a full provider call).
  A reasoning-tier model (like `claude-opus-4-8`) is ~20–40s per turn, so a
  2-agent × 3-round discussion is a few minutes. Use `maxChain` and the stop
  button to keep it bounded.

See the design spec at
[`../../docs/superpowers/specs/2026-06-09-roundtable-group-chat.md`](../../docs/superpowers/specs/2026-06-09-roundtable-group-chat.md)
for the full model and rationale.
