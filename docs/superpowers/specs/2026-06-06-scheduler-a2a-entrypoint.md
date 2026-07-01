# AHSIR: Scheduler-Owned A2A Entrypoint

**Status:** Implemented; scheduler ledger and restart continuation now build on this entrypoint
**Date:** 2026-06-06
**Version:** 0.1.0

## 1. Motivation

Recovery ledger design is fragile if callers can bypass the scheduler and send
A2A JSON-RPC directly to each `ahsir-agent` port. The scheduler needs to be the
observable request boundary so it can later record invocations before
forwarding and mark them complete after the upstream agent responds.

## 2. Goals

- Expose a scheduler-hosted A2A endpoint for every registered local agent.
- Rewrite public Agent Cards so `url` points to the scheduler endpoint, not the
  internal agent port.
- Keep the agent's own A2A HTTP server as an internal execution target.
- Route agent-to-agent calls through the scheduler-visible A2A entrypoint.
- Preserve existing CLI `/agents/{name}/chat` and task-status gateway behavior.
- Keep direct agent ports available for health checks and local debugging.
- Require a scheduler-issued internal token for local agent A2A traffic.

## 3. Non-Goals

- Fully hiding localhost ports. Agent health/debug endpoints stay reachable on
  the internal port.
- Implementing the recovery invocation ledger itself. The ledger and
  continuation prompt are covered by later recovery specs.
- Rewriting remote agent cards. Remote URLs are already outside scheduler-owned
  process supervision and should remain as registered.

## 4. Public vs Internal Agent Cards

The registry stores internal cards:

```text
teacher.url = http://127.0.0.1:9801/
```

The scheduler returns public cards from its HTTP API:

```text
teacher.url = http://127.0.0.1:9800/a2a/teacher
```

Internally, scheduler forwarding still uses the stored internal card so it can
reach the local agent process.

## 5. Scheduler A2A Proxy

New endpoint:

```text
POST /a2a/{agentName}
```

Behavior:

1. Look up `{agentName}` in the internal registry.
2. Reject unknown agents with 404.
3. Reverse-proxy the request body, headers, and response to the internal
   `AgentCard.url`.
4. Add the scheduler-issued `X-Ahsir-Internal-Token` header for local managed
   agents.
5. Preserve streaming responses by copying the upstream response body directly.

This endpoint accepts native A2A JSON-RPC such as `message/send`,
`message/stream`, and `tasks/get`.

## 6. Internal Agent Token

For local agents started by the scheduler:

1. Scheduler generates a random internal token.
2. Scheduler starts `ahsir-agent` with `--internal-token <token>`.
3. `ahsir-agent` accepts `/healthz`, `/readyz`, and well-known Agent Card
   requests without the token.
4. `ahsir-agent` rejects A2A JSON-RPC on `/` unless
   `X-Ahsir-Internal-Token` matches.
5. Scheduler `/a2a/{agentName}` injects the header when proxying to the
   internal agent.

## 7. Agent-to-Agent Calls

`RegistryAgentCaller` should no longer look up a card and then direct-connect
to the target agent's internal URL. It should use the scheduler registry API's
public card, whose `url` points back to `/a2a/{agentName}`. This keeps
cross-agent invocations visible to the scheduler entrypoint.

## 8. Acceptance Tests

- `GET /agents/{name}` returns a card whose URL points to `/a2a/{name}`.
- `GET /agents` returns public URLs for local cards.
- `POST /a2a/{name}` forwards native A2A `message/send` to the internal agent.
- Scheduler proxy injects the internal token for local managed agents.
- Local agent A2A requests without the internal token are rejected.
- Existing `POST /agents/{name}/chat` still works.
- `RegistryAgentCaller` sends A2A traffic through the scheduler card URL rather
  than the internal agent URL.
