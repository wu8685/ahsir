# Issue #40: Dispatch filesystem compatibility preflight

## 1. Goal

Before an agent is awakened or receives a turn, reject a dispatch whose declared
filesystem inputs are outside that agent card's effective filesystem allowlist.
The check prevents structurally incapable dispatches; it never widens the card's
permissions and does not replace the provider sandbox.

Free-form prompt text is not parsed for paths in this change.

## 2. Request contract

Callers declare zero or more required filesystem paths explicitly:

- Scheduler chat gateway: `requiredPaths: ["/abs/project", "./input.json"]` in
  `POST /agents/{name}/chat`.
- Native A2A gateway: `message.metadata.requiredFilesystemPaths` using the same
  JSON string-array shape for both `message/send` and `message/stream`.
- CLI: repeatable `--require-path PATH` on `ahsir chat`.

The scheduler client and internal A2A client gain requirement-aware methods;
existing methods delegate with an empty path list so their wire shape and
behavior remain unchanged.

CLI non-streaming dispatch sends `requiredPaths` through `/chat`. When
`--stream` is combined with required paths, the SSE request uses the scheduler
`/a2a/{agent}` proxy and carries A2A metadata, ensuring the same preflight runs
before the direct provider-facing request. Streaming without requirements keeps
its existing behavior.

Malformed metadata (not an array, non-string/empty elements) is rejected as an
invalid request rather than silently ignored.

## 3. Effective allowlist

For a scheduler-managed base agent, preflight loads the same validated
`agent-card.yaml` that runtime startup uses.

- `filesystem.enabled: false` permits no declared path.
- Relative `filesystem.allowed_paths` entries resolve against the agent's
  effective workdir (`AgentConfig.Workdir`, falling back to
  `AgentConfig.Workspace`), matching provider startup.
- An enabled filesystem with no explicit paths retains the existing default
  allowlist of `.`.
- The configured upload directory is an implicit allowed root when filesystem
  access is enabled, matching the existing runtime `--add-dir` behavior.
- Every required path must be contained by at least one allowed root. Exact root
  equality counts as contained; all declared paths must pass.
- A pooled instance is checked against its base card policy.
- If the scheduler cannot prove the policy for a remote/registry-only agent, a
  dispatch with requirements fails closed with `filesystem_policy_unavailable`.

## 4. Canonicalization and containment

Both required paths and allowed roots are canonicalized before comparison:

1. Resolve a relative path against the same effective workdir.
2. Convert to absolute form and clean `.` / `..` components.
3. Resolve filesystem symlinks with `filepath.EvalSymlinks`.
4. Require the referenced path/root to exist; a resolution failure is an
   actionable invalid-path error.
5. Use `filepath.Rel(root, required)` for component-aware containment. Permit
   `.` and descendants; reject `..`, `../…`, absolute relative results, and
   siblings that only share a string prefix.

Consequences pinned by tests:

- `/allowed/project/sub` is allowed under `/allowed/project`.
- `/allowed/project` exactly is allowed.
- `/allowed/project-evil` is denied.
- `/allowed/project/../secret` resolves and is denied.
- a symlink inside an allowed root pointing outside is denied;
- a symlink outside pointing to a target inside an allowed root is allowed,
  because access is evaluated against the canonical target.

The provider's filesystem sandbox remains the enforcement boundary after
dispatch. A filesystem change racing the preflight cannot cause ahsir to add a
new allowed root or relax sandbox configuration.

## 5. Ordering and errors

Preflight runs before:

1. `ensureAwake` / pooled-instance startup;
2. invocation-ledger `in_flight` recording;
3. any A2A forwarding or provider process invocation for the turn.

On failure the gateway returns a structured 403 response:

```json
{
  "error": "agent reviewer-codex cannot access /workspace/cosmos",
  "code": "filesystem_access_denied",
  "agent": "reviewer-codex",
  "path": "/workspace/cosmos"
}
```

Other stable codes are `filesystem_disabled`, `filesystem_path_invalid`, and
`filesystem_policy_unavailable`. The response identifies only the selected
agent and incompatible requested path. It does not return the complete
allowlist or unrelated card fields. A concise remediation tells the caller to
select or reconfigure an appropriate project-scoped agent.

## 6. Compatibility

- An absent or empty required-path list preserves today's dispatch behavior.
- Existing A2A callers, CLI commands, cards, and provider arguments require no
  migration.
- There is no prompt path inference and no automatic permission widening.
- A denied dispatch does not wake an idle agent and does not create a provider
  task.

## 7. Test contract

Tests are written before implementation and cover:

- exact roots, descendants, multiple required paths, and multiple allowed roots;
- denied siblings and `..` traversal;
- symlink-to-outside denial and symlink-to-inside allowance;
- disabled filesystem access and unavailable remote policy;
- relative path resolution against effective workdir;
- implicit upload-directory compatibility;
- malformed gateway/A2A request metadata;
- `/chat` sync/async and `/a2a` send/stream rejection before wake/forward;
- repeatable CLI flags and stream proxy routing;
- structured error fields without unrelated configuration leakage;
- byte-compatible behavior for callers that omit required paths.
