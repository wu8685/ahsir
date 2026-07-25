# Codex Custom Provider Env Credentials Design

## Context

Ahsir runs Codex-backed agents by spawning `codex exec --json` with an isolated
`CODEX_HOME` per agent workspace. The current implementation has two unsafe
couplings:

1. It copies the operator's global `~/.codex/auth.json` into every isolated
   agent home.
2. It expands `runtime.apiKey`, maps the resulting secret to `CODEX_API_KEY`,
   and passes that value to the Codex subprocess.

The custom Codex provider work already constructs an ephemeral
`model_provider` with `-c` overrides. That is the correct integration boundary,
but provider authentication must use Codex's native `env_key` contract rather
than copied login state or Ahsir-specific secret remapping.

Hetairoi already declares credentials as an environment-variable name through
`runtime_api_key_env`. The end-to-end path should preserve that name without
resolving the secret until the Codex subprocess reads its inherited
environment.

## Goals

- Support `codex exec` against a custom OpenAI-compatible endpoint such as the
  local Kimi K3 Responses proxy.
- Allow many Ahsir Codex agent processes to reuse one API key through an
  inherited environment variable.
- Prevent API-key material from being written to `auth.json`, agent cards,
  generated Codex config, logs, or Ahsir-managed state.
- Keep per-agent Codex transcripts and session-resume state isolated.
- Preserve Hetairoi's existing `runtime_api_key_env` declaration.
- Fail safely and clearly when old or insecure credential configuration is
  encountered.

## Non-goals

- Supporting literal API keys in Ahsir configuration.
- Supporting command-backed or OS-Keychain-backed provider authentication in
  the Agent card schema.
- Building a secret broker.
- Automatically deleting existing credential files.
- Changing Claude/Anthropic provider authentication.
- Solving Codex ChatGPT-login isolation in this change.

If an operator stores the Kimi key in macOS Keychain, the service launcher is
responsible for exporting it into Ahsir's environment. Once Ahsir starts, the
same env-only contract applies.

## Configuration Contract

The supported Codex custom-provider Agent card is:

```yaml
runtime:
  provider: codex
  model: k3
  baseURL: http://127.0.0.1:18793/v1
  wireAPI: responses
  credential:
    envKey: MOONSHOT_API_KEY
```

### Schema

`RuntimeConfig` gains:

```go
WireAPI    string                  `yaml:"wireAPI" json:"wireAPI"`
Credential RuntimeCredentialConfig `yaml:"credential" json:"credential"`

type RuntimeCredentialConfig struct {
    EnvKey string `yaml:"envKey" json:"envKey"`
}
```

For `provider: codex`:

- `runtime.apiKey` is forbidden, whether it contains a literal or `${VAR}`.
- `runtime.credential.envKey` is an environment-variable name, not a value or
  interpolation expression.
- `envKey` must match `[A-Za-z_][A-Za-z0-9_]*`.
- The named environment variable must exist and be non-empty when the agent
  starts.
- Validation errors name the field or variable but never include its value.
- `baseURL` requires `credential.envKey`.
- `wireAPI` accepts `responses` or `chat`; it defaults to `responses`.
- The generated provider always sets `requires_openai_auth = false`.

The existing `runtime.env` escape hatch remains available for non-secret
settings. A Codex custom provider must not obtain its credential through
`runtime.env`; the credential declaration has one canonical location.

## Runtime Data Flow

```text
watches.yaml
  runtime_api_key_env: MOONSHOT_API_KEY
          |
          v
Hetairoi apply / CMA agent metadata
  passes "MOONSHOT_API_KEY", never its value
          |
          v
Ahsir Agent card
  credential.envKey: MOONSHOT_API_KEY
          |
          v
codex exec arguments
  model_providers.ahsir_runtime.env_key="MOONSHOT_API_KEY"
  model_providers.ahsir_runtime.requires_openai_auth=false
          |
          v
Codex subprocess
  reads MOONSHOT_API_KEY from inherited environment
```

Ahsir validates the variable's presence with `os.LookupEnv`, but it does not
copy the value into another map entry or expose it in an error. The Codex
subprocess inherits the parent environment unchanged.

## Isolated `CODEX_HOME`

Each Codex-backed agent continues to use:

```text
<workspace>/.a2a/codex-home/
```

Initialization changes as follows:

- Never copy `auth.json` from the operator's global Codex home.
- Never create an `auth.json` for custom providers.
- Preserve only the non-credential Codex configuration that Ahsir explicitly
  supports.
- Keep transcript, thread, and resume state inside the isolated home.
- Construct the custom provider through deterministic CLI `-c` overrides so
  the generated home contains no provider secret.

This change deliberately separates agent state isolation from credential
inheritance. A custom provider authenticates exclusively through its declared
environment variable.

## Codex Invocation

For the example card, Ahsir adds equivalent arguments to every initial and
resumed turn:

```text
-c model_provider="ahsir_runtime"
-c model_providers.ahsir_runtime.name="Ahsir runtime"
-c model_providers.ahsir_runtime.base_url="http://127.0.0.1:18793/v1"
-c model_providers.ahsir_runtime.wire_api="responses"
-c model_providers.ahsir_runtime.env_key="MOONSHOT_API_KEY"
-c model_providers.ahsir_runtime.requires_openai_auth=false
```

The actual API key is absent from the arguments. `CODEX_API_KEY` is no longer
generated.

Provider construction remains owned by Ahsir. Agent cards cannot select
`requires_openai_auth`, override the provider ID, or inject credential values
through generic Codex `-c` arguments.

## Hetairoi Integration

Hetairoi's public declaration remains:

```yaml
runtime_provider: codex
runtime_base_url: http://127.0.0.1:18793/v1
runtime_api_key_env: MOONSHOT_API_KEY
model: k3
```

The apply/translation path must emit:

```yaml
runtime:
  provider: codex
  baseURL: http://127.0.0.1:18793/v1
  model: k3
  credential:
    envKey: MOONSHOT_API_KEY
```

No Hetairoi component may call `os.Getenv` for
`runtime_api_key_env` in order to populate the Agent card. Hetairoi only
validates that the declaration is syntactically a variable name. Ahsir owns the
runtime presence check because Ahsir is the process that launches Codex.

## Migration and Compatibility

### Old environment-reference form

```yaml
runtime:
  provider: codex
  apiKey: "${MOONSHOT_API_KEY}"
```

Ahsir rejects this configuration before interpolation with:

```text
runtime.apiKey is no longer supported for provider=codex; use runtime.credential.envKey: MOONSHOT_API_KEY
```

The migration hint may extract and display the variable name only when the
entire value is exactly `$NAME` or `${NAME}`.

### Literal key form

```yaml
runtime:
  provider: codex
  apiKey: "sk-..."
```

Ahsir rejects it with a generic message:

```text
runtime.apiKey must not contain a Codex provider secret; use runtime.credential.envKey
```

The error never echoes the configured value.

### Existing copied credentials

The implementation stops creating new copies but does not silently delete
existing files. Documentation provides an explicit discovery and cleanup
procedure for:

```text
<workspace>/.a2a/codex-home/auth.json
```

Cleanup must be a separate operator action because an existing file could
contain a valid ChatGPT login rather than the Kimi key.

## Security Properties

- Secrets never appear in Agent card YAML.
- Secrets never appear in generated `config.toml`.
- Secrets never appear in Codex CLI arguments.
- Secrets are not renamed to `CODEX_API_KEY` or `OPENAI_API_KEY`.
- Secrets are not copied through Go maps by provider translation.
- `auth.json` is neither read nor copied for custom-provider startup.
- Errors and logs may include the env variable name, never its value.
- Initial and resumed Codex turns use the same provider declaration.
- Generic `runtime.args` cannot override authentication or sandbox-owned
  settings.

Environment variables are still readable by the process and sufficiently
privileged same-user tooling. This design prevents accidental persistence and
credential confusion; it does not claim to provide a hardened secret enclave.

## Validation and Error Handling

Agent startup fails before binding its listen port when:

- `runtime.apiKey` is present for `provider: codex`;
- `credential.envKey` is malformed;
- the named environment variable is missing or empty;
- `baseURL` is present without `credential.envKey`;
- `wireAPI` is unsupported;
- generic Codex args attempt to override provider authentication.

Messages must be actionable while remaining secret-safe. Failed validation
must not partially initialize the isolated Codex home.

## Test Strategy

### Ahsir unit tests

- `runtime.apiKey` literal is rejected without value disclosure.
- exact `$VAR` and `${VAR}` legacy forms are rejected with a migration hint.
- valid `credential.envKey` preserves only the variable name.
- malformed, unset, and empty variables fail safely.
- custom `baseURL` without credentials fails.
- `wireAPI` defaults to `responses`; `chat` is accepted; other values fail.
- provider CLI overrides contain `env_key` and
  `requires_openai_auth=false`.
- provider CLI overrides contain no secret value.
- `CODEX_API_KEY` and `OPENAI_API_KEY` are not synthesized.
- `mirrorCodexHomeForAgent` does not copy `auth.json`.
- initial and resume arguments use the same provider configuration.

### Hetairoi unit tests

- `runtime_api_key_env` remains an identifier through translation.
- malformed identifiers fail without environment lookup.
- generated Agent metadata/card uses `credential.envKey`.
- no test path needs a real API key.

### Integration tests

- A fake `codex` binary captures argv and selected environment variable names,
  proving the secret is absent from argv and generated files.
- A temporary source Codex home containing `auth.json` proves the isolated
  destination does not receive it.
- A local Kimi smoke test runs only when explicitly enabled. It reports
  success or sanitized failure and never prints the key.

## Rollout

1. Add and validate the env-only credential schema in Ahsir.
2. Change custom-provider argument construction and stop copying `auth.json`.
3. Update Hetairoi translation to emit `credential.envKey`.
4. Update examples and operator documentation.
5. Migrate `~/.cma-stack/watches.yaml` and re-apply the fleet.
6. Inspect existing isolated homes and explicitly remove obsolete copied
   `auth.json` files after confirming they are not needed.
7. Run unit suites, fake-Codex integration tests, and one opt-in Kimi smoke
   test.

## Acceptance Criteria

- A Kimi K3 Ahsir agent completes both a fresh and resumed Codex turn.
- Multiple agents reuse `MOONSHOT_API_KEY` from their inherited environment.
- No Kimi key is written to global or per-agent `auth.json`.
- No Ahsir/Hetairoi configuration or generated file contains the key value.
- Old Codex `runtime.apiKey` configurations fail with secret-safe migration
  guidance.
- Existing non-Codex providers continue to pass their test suites unchanged.
