# Auth Baseline: Control-Plane Token + Token-Leak Fix

## Background

The scheduler has no ingress authentication. Any process that can reach the
scheduler's HTTP port (loopback today) can invoke every operation. The
2026-06-07 adversarial review (docs/reviews/2026-06-07-adversarial-review.md)
listed this as a P1 with three concrete holes:

1. `/admin/agents` (POST start / DELETE stop) has no auth — any local process
   can start or stop any workspace agent (`internal/scheduler/gateway.go`).
2. Registry `POST /agents` has no auth and same-name registration silently
   overwrites — any process can replace an agent's card, including its `URL`
   (`internal/registry/registry.go:37` Register, case-insensitive overwrite).
3. The scheduler attaches its internal token to requests whose destination
   comes from the registry card's `URL`. Since (2) lets an attacker overwrite
   that URL, the scheduler can be induced to send the internal token to an
   attacker-controlled endpoint (`gateway.go` handleA2AProxy, `scheduler.go`
   ChatWithAgentAs / ChatWithAgentAsync / AgentHistory).

The just-landed shared-context work made this more urgent: `speaker` is
self-claimed and the transcript is full-content on disk, with several spec
seams ("ACL attaches when auth lands") that are inert until a principal exists.

The shared-context spec's busy/queue work, the ledger, and the transcript all
assume a trust boundary; this spec establishes the first real one.

## Trust model

The trust boundary is **the OS user who started the scheduler**. Concretely:
"can read the `0600` admin-token file" ≡ "is the same OS user." This is the
right boundary for local-first on a shared/multi-user machine — a different OS
user, or any process that cannot read that user's file, is untrusted.

Out of scope for this baseline (explicitly deferred):
- Non-loopback / cross-machine access (network.bind stays loopback-only).
- Multiple principals, roles, RBAC. One privileged identity.
- Authenticating the chat / read data plane (`/agents/{name}/chat`,
  `/history`, `/tasks`, `/invocations`, `/config/timeouts`, `/agents` GET).
  Those stay open in this baseline; the code is structured so a later phase
  can gate them when a non-loopback bind is introduced.

## Goals

- **One privileged token** ("admin token") gates the control plane: the
  `/admin/agents` lifecycle endpoints and the registry write path
  (`POST /agents`, `DELETE /agents/{name}`).
- **Auto-generated `0600` token file**, on by default, zero-friction for the
  same-user case:
  - `ahsir start` generates a 32-byte hex token and writes it to
    `<config-dir>/admin-token` at `0600` if absent; reuses it if present.
  - `<config-dir>` is the directory containing the resolved `ahsir.yaml`
    (so a project-local config and `~/.ahsir/` each get their own token,
    matching how the rest of the runtime state is already scoped).
  - `AHSIR_ADMIN_TOKEN` env var overrides the file entirely (CI / container
    use where writing a file is undesirable). When set, no file is read or
    written.
- **CLI auto-discovers** the token (same file / same env override) and
  attaches it to control-plane requests, so same-user usage is unchanged.
- **Forbid unauthorized same-name overwrite** in the registry: a write
  without a valid token is rejected (401); the silent-overwrite hole closes
  as a consequence of gating the write path.
- **Token-leak fix** (independent of the token, pure correctness): the
  scheduler sends its per-agent internal token only to the local address it
  recorded when it spawned the agent, never to the registry card's `URL`.
- `ahsir doctor` reports auth status (enforced / token source).

## Non-Goals

- No data-plane auth (chat/read) — see Trust model.
- No token rotation command in this baseline (delete the file + restart is the
  manual path; documented).
- No change to the per-agent internal token mechanism beyond the destination
  fix — the internal token still authenticates scheduler→agent A2A.
- No multi-token / per-role split (single privileged token; the review listed
  two, collapsed here, splittable later).

## Design

### Admin token store (new)

A small loader in `internal/scheduler` (e.g. `admintoken.go`):

- `LoadOrCreateAdminToken(configPath string) (token string, source string, err error)`:
  1. If `AHSIR_ADMIN_TOKEN` is set and non-empty → return it, source `"env"`,
     touch no file.
  2. Else compute `tokenPath := filepath.Join(filepath.Dir(configPath), "admin-token")`.
     - If it exists and is non-empty → read, return, source `"file"`.
     - Else generate 32 random bytes hex-encoded (reuse the existing
       `newInternalToken` generator), write `0600` (dir `0700` via MkdirAll),
       return, source `"file (generated)"`.
- Corrupt/empty file → regenerate (the token is rebuildable state; a wedged
  file must not block startup). Log the regeneration.
- The token is held on the `Scheduler` (e.g. `s.adminToken`) and consulted by
  the gateway middleware and passed to spawned agents.

### Enforcement: gateway control plane

A middleware/helper `requireAdminToken` in the gateway (mirrors
`wrapper.requireInternalToken`): compares `X-Ahsir-Admin-Token` against
`s.adminToken`; on mismatch, `401` with a JSON error naming the header.

Applied to:
- `POST /admin/agents`, `DELETE /admin/agents/{name}` (handleAdmin).
- The registry write path. The registry HTTP handler is constructed by the
  scheduler (`registry.NewHTTPHandler`), and the gateway already wraps it
  (`g.registry.ServeHTTP`). Gate `POST /agents` and `DELETE /agents/{name}`
  at the gateway layer before delegating to the registry, so the registry
  package stays auth-agnostic (it remains a pure store) and the token check
  lives in one place. `GET /agents` (list / single) stays open.

Empty `s.adminToken` (should not happen — always generated) means "auth
disabled": middleware passes through. This keeps tests that construct a bare
scheduler working and is the natural degenerate case.

### Agent heartbeat registration

Agents POST their card to the registry every ~10s (`wrapper.AgentWrapper`
heartbeat). With the write path gated, agents must present the token:

- The scheduler passes the admin token to each spawned agent via a new
  `--admin-token` flag (mirrors `--internal-token`).
- `cmd/ahsir-agent` plumbs it into `AgentWrapperConfig`; the heartbeat POST
  attaches `X-Ahsir-Admin-Token`.
- Externally-started / remote agents (no scheduler-issued token) cannot
  register — acceptable at this trust level; documented. (Local-first agents
  are scheduler-spawned.)

### Token-leak fix (independent correctness fix)

The scheduler must route token-bearing requests to the address it assigned the
agent, not to `card.URL` from the registry:

- `agentProcess` already knows the local address it spawned the agent on
  (host + port). Add/confirm a `localURL()` accessor.
- `handleA2AProxy`, `ChatWithAgentAs`, `ChatWithAgentAsync`, `AgentHistory`:
  when the agent is a scheduler-managed local process, build the target URL
  from `agentProcess` local address, not `card.URL`. Fall back to `card.URL`
  only for agents the scheduler did not spawn (e.g. a future remote agent),
  and in that case do **not** attach the internal token.
- Net effect: overwriting `card.URL` can no longer redirect a token-bearing
  request to an attacker. The registry card's URL becomes advisory for
  discovery/display, not a trust input for token delivery.

### CLI auto-discovery

A shared helper in `cmd/ahsir` (e.g. `resolveAdminToken(configPath)`) mirrors
the scheduler loader's *read* half (env → file), but never generates: if
neither exists, return empty (the command will get a 401 with a clear message
telling the user the scheduler isn't enforcing or the file is unreadable).

Attached by the commands that hit the control plane:
- `ahsir agent new` / `ahsir agent delete` (POST/DELETE `/admin/agents`).
- Any future direct registry-write CLI.

`ahsir chat / list / status / trace / history` are data-plane → no token
needed (matches the scope decision).

### Doctor

`ahsir doctor` gains an "auth" line: enforced (token present) + source
(env / file path), or a warning if the scheduler is reachable but the CLI
cannot find a token (cross-user / missing file).

## Acceptance Criteria

- Unit: `LoadOrCreateAdminToken` — generates a `0600` file when absent (dir
  `0700`), reuses an existing one, honours `AHSIR_ADMIN_TOKEN` without touching
  the file, regenerates on a corrupt/empty file.
- Unit: gateway `requireAdminToken` — `/admin/agents` POST/DELETE and registry
  `POST`/`DELETE /agents` return 401 without the header, succeed with it;
  `GET /agents`, `/agents/{name}/chat`, `/history`, `/tasks`, `/invocations`,
  `/config/timeouts` stay open.
- Unit: registry write with a valid token can register; same-name overwrite
  with a valid token still works (scheduler heartbeat); without token → 401,
  and the existing card is unchanged (overwrite blocked).
- Unit: token-leak fix — with a registry card whose `URL` points elsewhere, a
  scheduler-managed agent's chat / history / proxy request goes to the
  scheduler-recorded local address and carries the internal token; a
  non-managed (card-only) agent gets `card.URL` and **no** internal token.
- Unit/integration: agent heartbeat with `--admin-token` registers
  successfully against a gated registry.
- Unit: CLI `resolveAdminToken` — env wins over file; missing both → empty.
- E2E: `ahsir agent new` against an enforcing scheduler succeeds (CLI
  auto-discovers); a request with a wrong/absent token is rejected 401.
- `make test` (race) passes.

## Implementation Order

1. **Slice A** — admin token store (`LoadOrCreateAdminToken` + env override),
   wired onto the Scheduler at Start. No enforcement yet.
2. **Slice B** — token-leak fix (route token to local address). Independent;
   lands value even before enforcement.
3. **Slice C** — enforcement middleware on `/admin/agents` + registry write;
   `--admin-token` to agents for heartbeat; forbid unauthorized overwrite.
4. **Slice D** — CLI auto-discovery + `ahsir doctor` auth line.
5. **e2e + README + real-run verification.**
