# Agent Process Lifecycle Hardening

## Background

Local ahsir deployments run a scheduler process, one or more `ahsir-agent`
processes, and provider subprocesses such as `claude` or `codex exec`. Recent
manual inspection showed orphan `ahsir-agent` processes with `PPID=1`, stale
agents still listening on configured ports, and Codex notification helper
processes outliving the provider turn.

## Goals

- Ensure scheduler-owned local agents are stopped as complete process trees.
- Ensure provider subprocesses are stopped as complete process trees when an
  agent session is closed, canceled, or timed out.
- Ensure scheduler startup handles stale local agents that already occupy the
  configured workspace or port.
- Ensure an agent exits by itself when its scheduler registry is unavailable
  for a sustained period.
- Ensure Codex provider turns do not trigger Codex global turn notification
  hooks by default.

## Non-Goals

- Do not change A2A request or response schemas.
- Do not introduce a remote distributed process manager.
- Do not remove Codex or Claude local session persistence.
- Do not push commits to a remote repository.

## Design

### Process Ownership

Scheduler-spawned `ahsir-agent` commands run in their own process group. Stop,
unhealthy restart, and scheduler shutdown terminate the process group first and
fall back to the direct process when needed.

Provider subprocesses spawned by wrapper sessions also run in their own process
group. `ClaudeSession.Close` and `CodexSession` context cancellation terminate
the provider process tree, not just the direct process.

### Stale Local Agent Eviction

Before starting a configured local agent, the scheduler checks whether the
target TCP port is already occupied by an `ahsir-agent` process. If the process
matches the configured workspace or executable name, the scheduler treats it as
stale local state and terminates its process group before binding the new
agent. If the port is occupied by an unrelated process, startup fails with a
clear error.

### Agent Self-Exit

An `ahsir-agent` with a registry URL monitors registry reachability. If
registration fails continuously for a grace period, the agent cancels its main
context and exits normally. This handles hard scheduler crashes where the
scheduler cannot send a signal to its children.

### Codex Notify Isolation

Codex provider subprocesses receive an isolated workspace-scoped `CODEX_HOME`
at `<agent workspace>/.a2a/codex-home` by default unless the runtime explicitly
provides `CODEX_HOME`. This preserves Codex thread resume within the agent while
preventing global Codex notify hooks from running inside ahsir agent turns.

## Acceptance Criteria

- Unit tests verify scheduler and provider commands are process-group managed.
- Unit tests verify scheduler stop/restart paths use process-group termination.
- Integration-style tests verify stale ahsir-agent processes are terminated
  before startup and unrelated port listeners are not killed.
- Unit tests verify agent registry monitor cancels after sustained failures.
- Unit tests verify Codex runtime environment injects isolated `CODEX_HOME`
  when absent and preserves explicit `CODEX_HOME` when present.
- `go test ./...` passes.
