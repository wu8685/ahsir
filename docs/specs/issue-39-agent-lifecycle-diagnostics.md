# Issue #39: Agent lifecycle diagnostics

## 1. Goal

Make scheduler diagnostics distinguish a healthy scale-to-zero agent from an
agent that is stopped, misconfigured, crash-looping, or failing health checks.
The same classification must be available to humans and machine consumers.

This change is diagnostic only for idle agents: it must not change their
existing self-exit, activator wake, context reuse, or pooling behavior.

## 2. Public model

The scheduler exposes a read-only `GET /diagnostics/agents` endpoint. Each base
agent has one lifecycle snapshot; pooled instance children remain internal.

```json
{
  "name": "reviewer-codex",
  "state": "restart-backoff",
  "reasonCode": "process-exit",
  "reason": "process exited with status 1; restart attempt 3 scheduled",
  "severity": "error",
  "wakeable": false,
  "restartAttempt": 3,
  "updatedAt": "2026-08-05T12:00:00Z"
}
```

`state` is a closed machine-readable enum:

| State | Meaning | Default severity | Wakeable |
|---|---|---:|---:|
| `online` | Process is registered and its heartbeat is current | `ok` | n/a |
| `idle` | Process exited with `IdleStopExitCode` and remains desired | `info` | yes |
| `stopped` | Configured but not started, or explicitly stopped | `warning` | no |
| `invalid-config` | Agent card/runtime validation failed before provider startup | `error` | no |
| `restart-backoff` | Unexpected process exit; supervisor retry is pending | `error` | no |
| `health-failed` | Health threshold was reached; restart is pending/in progress | `error` | no |

`reasonCode` refines the state without requiring callers to parse `reason`.
Initial codes are `healthy`, `scale-to-zero`, `configured-not-started`,
`operator-stopped`, `invalid-agent-card`, `invalid-runtime`, `process-exit`, and
`health-threshold`.

The endpoint includes configured agents even when no live registry card exists.
Remote registry-only agents are classified from heartbeat state; a stale remote
heartbeat is `health-failed` with reason code `heartbeat-timeout`.

## 3. Lifecycle ownership and transitions

The scheduler, not the heartbeat registry, owns lifecycle state. It records a
snapshot whenever one of these events occurs:

1. Config discovered but not yet spawned → `stopped/configured-not-started`.
2. Runtime validation succeeds and process registers healthy → `online/healthy`.
3. Idle exit code 111 is observed → `idle/scale-to-zero`.
4. Explicit `StopAgent` → `stopped/operator-stopped`.
5. Shared pre-start validation fails → `invalid-config`.
6. Unexpected process exit → `restart-backoff/process-exit`, including attempt
   number and next retry time while known.
7. Health failure threshold is reached → `health-failed/health-threshold`; the
   later process exit must not erase that cause with a generic crash reason.

Successful registration or a successful wake clears prior failure data and
returns the base agent to `online`.

Runtime/card validation is factored into a shared validator used by both the
scheduler preflight and `ahsir-agent` bootstrap, so the two paths cannot drift.
A config error for one configured agent is isolated: the scheduler records that
agent as `invalid-config`, continues starting other agents, and remains
available. Non-agent infrastructure failures such as the scheduler listen
failure remain fatal.

Reasons are generated from typed validation failures. They may name a config
field and a missing environment-variable name, but never include environment
values, credentials, raw command environment, or unrelated agent config.
Human reason strings are single-line and bounded.

## 4. Doctor behavior

`ahsir doctor` reads `/diagnostics/agents` rather than inferring lifecycle from
`/agents` heartbeat status.

- `✓` renders `online`.
- `○` renders `idle` as informational; it neither warns nor changes the exit
  status.
- `⚠` renders `stopped`.
- `✗` renders `invalid-config`, `restart-backoff`, and `health-failed`; any such
  entry makes doctor exit 1.

`ahsir doctor --json` emits a stable JSON envelope containing the existing
config/provider/scheduler/auth checks plus the lifecycle snapshot array and an
overall `ok` boolean. Human and JSON output use the same decoded lifecycle
objects; there is no second classification path in the CLI.

## 5. Compatibility

- Existing `/agents` discovery responses and `ahsir list` remain unchanged.
- Existing scheduler/agent configuration is valid without migration.
- Idle wake behavior and `IdleStoppedAgents` semantics remain unchanged.
- Consumers that do not call the new diagnostics endpoint observe no wire
  change.

## 6. Test contract

Tests are written before implementation and cover:

- online registration and recovery from an earlier failure;
- idle exit classified as informational and wakeable;
- explicit stop and configured-but-not-started reason codes;
- missing env/runtime config classified with an actionable sanitized reason;
- crash backoff with attempt metadata;
- health-threshold failure retaining its cause through process exit;
- endpoint JSON stability and omission of pooled instance children;
- human doctor symbols/severity, JSON parity, and exit status;
- unchanged idle wake and context reuse behavior.
