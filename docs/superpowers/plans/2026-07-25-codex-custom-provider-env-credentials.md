# Codex Custom Provider Env Credentials Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Ahsir run Codex custom providers with a declared environment-variable name, without copying `auth.json`, expanding the secret, or mapping it to `CODEX_API_KEY`.

**Architecture:** Extend the shared Agent card runtime schema with `wireAPI` and `credential.envKey`. Validate Codex credentials before any isolated-home mutation, construct a deterministic Codex custom provider through safe `-c` arguments, and update the CMA facade translation so Hetairoi metadata remains a variable name end to end. The source tree remains authoritative and is synced into `plugin/src` only after source tests pass.

**Tech Stack:** Go 1.23 modules, YAML/JSON Agent cards, Codex CLI `exec --json`, Ahsir CMA facade, Go unit/integration tests.

## Global Constraints

- Codex custom providers allow only `runtime.credential.envKey`; command-backed credentials and literal keys are out of scope.
- `runtime.apiKey` is rejected for `provider: codex`, including exact `$VAR` and `${VAR}` legacy forms.
- The environment-variable name must match `[A-Za-z_][A-Za-z0-9_]*`, exist, and contain a non-empty value at agent startup.
- The secret value must not appear in Agent cards, generated config, CLI arguments, errors, logs, or Ahsir-managed state.
- Do not synthesize `CODEX_API_KEY` or `OPENAI_API_KEY`.
- Codex custom-provider arguments must set `requires_openai_auth=false`.
- Do not copy or create `auth.json`; do not automatically delete existing copies.
- Preserve the existing uncommitted changes in `cmd/ahsir-agent/main.go`, `cmd/ahsir-agent/main_test.go`, `internal/wrapper/server.go`, and `internal/wrapper/server_test.go`.
- Base implementation on `origin/main` plus commit `8c3f5c6` (`feat: support custom Codex Responses endpoints`), then bring in design commit `d43b59f`.
- Run Go commands with `GO111MODULE=on`.
- Run the race-enabled full suite before completion.

---

## File Structure

### Ahsir source of truth

- `internal/wrapper/card.go`: Agent card schema (`WireAPI`, `Credential`).
- `internal/wrapper/runtime.go`: env-name validation, legacy rejection, Codex runtime resolution.
- `internal/wrapper/runtime_test.go`: security and validation behavior.
- `cmd/ahsir-agent/main.go`: isolated `CODEX_HOME` setup and Codex provider `-c` arguments.
- `cmd/ahsir-agent/main_test.go`: no-auth-copy and provider-argument behavior.
- `internal/wrapper/session_codex.go`: block raw `-c` authentication/provider overrides.
- `internal/wrapper/session_codex_test.go`: raw-argument security tests.
- `internal/cmagateway/ahsir/card.go`: facade-side mirror of the Agent card schema.
- `internal/cmagateway/translate/translate.go`: map `runtime_api_key_env` to `credential.envKey`.
- `internal/cmagateway/translate/translate_test.go`: metadata translation tests.
- `README.md`: supported configuration and migration procedure.
- `docs/EVENTBUS-SOURCES.md`: Hetairoi/CMA metadata contract.
- `plugin/src/`: generated source bundle, refreshed only through `make plugin`.

### Hetairoi

No production-code change is expected. Hetairoi already forwards CMA Agent
metadata without resolving `runtime_api_key_env`. Its documentation is updated
only if an existing example still describes `runtime.apiKey`.

---

### Task 1: Add the env-only Codex credential contract

**Files:**
- Modify: `internal/wrapper/card.go`
- Modify: `internal/wrapper/runtime.go`
- Modify: `internal/wrapper/runtime_test.go`

**Interfaces:**
- Consumes: existing `RuntimeConfig`, `RuntimeProvider`, `ResolveProviderEnv`.
- Produces:
  - `RuntimeCredentialConfig{EnvKey string}`
  - `RuntimeConfig.WireAPI string`
  - `RuntimeConfig.Credential RuntimeCredentialConfig`
  - `ResolveCodexProvider(rt RuntimeConfig) (CodexProviderConfig, error)`
  - `CodexProviderConfig{BaseURL, WireAPI, EnvKey string}`

- [ ] **Step 1: Replace the old Codex API-key tests with failing env-only tests**

Add focused tests equivalent to:

```go
func TestResolveCodexProviderAcceptsEnvCredential(t *testing.T) {
    t.Setenv("MOONSHOT_API_KEY_TESTONLY", "secret-must-not-escape")
    got, err := ResolveCodexProvider(RuntimeConfig{
        Provider: "codex",
        BaseURL: "http://127.0.0.1:18793/v1",
        Model: "k3",
        Credential: RuntimeCredentialConfig{EnvKey: "MOONSHOT_API_KEY_TESTONLY"},
    })
    if err != nil {
        t.Fatal(err)
    }
    if got.EnvKey != "MOONSHOT_API_KEY_TESTONLY" || got.WireAPI != "responses" {
        t.Fatalf("got %#v", got)
    }
}

func TestResolveCodexProviderRejectsLiteralAPIKeyWithoutEcho(t *testing.T) {
    const secret = "sk-secret-must-not-appear"
    _, err := ResolveCodexProvider(RuntimeConfig{Provider: "codex", APIKey: secret})
    if err == nil || strings.Contains(err.Error(), secret) {
        t.Fatalf("expected sanitized rejection, got %v", err)
    }
}

func TestResolveCodexProviderRejectsLegacyEnvReferenceWithHint(t *testing.T) {
    _, err := ResolveCodexProvider(RuntimeConfig{
        Provider: "codex",
        APIKey: "${MOONSHOT_API_KEY}",
    })
    if err == nil || !strings.Contains(err.Error(), "credential.envKey: MOONSHOT_API_KEY") {
        t.Fatalf("expected migration hint, got %v", err)
    }
}
```

Also cover malformed env names, missing variables, empty variables, missing
credential with `baseURL`, `wireAPI: chat`, and unsupported wire APIs.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
GO111MODULE=on go test ./internal/wrapper -run 'TestResolveCodexProvider' -count=1
```

Expected: compile failure because the new types and resolver do not exist.

- [ ] **Step 3: Implement the schema and resolver**

Add:

```go
type RuntimeCredentialConfig struct {
    EnvKey string `yaml:"envKey" json:"envKey"`
}

type CodexProviderConfig struct {
    BaseURL string
    WireAPI string
    EnvKey  string
}
```

Implement `ResolveCodexProvider` with this order:

1. reject `runtime.apiKey` before calling `expandStrict`;
2. validate the exact legacy `$NAME`/`${NAME}` form only for a safe hint;
3. expand `baseURL`, but never expand `credential.envKey`;
4. validate the env identifier;
5. require the named variable to exist and be non-empty;
6. default empty `wireAPI` to `responses`;
7. accept only `responses` and `chat`.

Change the Codex case in `ResolveProviderEnv` so it returns only explicitly
declared non-secret `runtime.env` entries and never creates `CODEX_API_KEY`.
Keep Anthropic/Zhipu/DeepSeek behavior unchanged.

- [ ] **Step 4: Run focused and package tests and verify GREEN**

Run:

```bash
GO111MODULE=on go test ./internal/wrapper -count=1
```

Expected: PASS, with all existing non-Codex provider tests unchanged.

- [ ] **Step 5: Commit the contract**

```bash
git add internal/wrapper/card.go internal/wrapper/runtime.go internal/wrapper/runtime_test.go
git commit -m "feat: add env-only Codex provider credentials"
```

---

### Task 2: Generate a safe Codex custom provider and stop copying auth

**Files:**
- Modify: `cmd/ahsir-agent/main.go`
- Modify: `cmd/ahsir-agent/main_test.go`

**Interfaces:**
- Consumes: `wrapper.ResolveCodexProvider`.
- Produces:
  - `appendCodexProviderArgs(args []string, cfg wrapper.CodexProviderConfig) []string`
  - isolated `CODEX_HOME` without `auth.json`.

- [ ] **Step 1: Write failing tests for provider arguments and home isolation**

Update `TestBuildSessionConfig_CodexInjectsIsolatedCodexHome` so a source
`auth.json` exists but the destination must return `os.IsNotExist`.

Add a provider test equivalent to:

```go
func TestBuildSessionConfig_CodexCustomProviderUsesEnvNameNotSecret(t *testing.T) {
    t.Setenv("MOONSHOT_API_KEY_TESTONLY", "secret-must-not-escape")
    cfg, err := buildSessionConfig("coder", wrapper.RuntimeConfig{
        Provider: "codex",
        Command: "codex",
        Model: "k3",
        BaseURL: "http://127.0.0.1:18793/v1",
        Credential: wrapper.RuntimeCredentialConfig{EnvKey: "MOONSHOT_API_KEY_TESTONLY"},
    }, wrapper.FilesystemConfig{}, wrapper.MCPConfig{}, wrapper.StreamingConfig{},
       t.TempDir(), t.TempDir())
    if err != nil {
        t.Fatal(err)
    }
    joined := strings.Join(cfg.Args, " ")
    for _, want := range []string{
        `model_provider="ahsir_runtime"`,
        `model_providers.ahsir_runtime.env_key="MOONSHOT_API_KEY_TESTONLY"`,
        `model_providers.ahsir_runtime.requires_openai_auth=false`,
    } {
        if !strings.Contains(joined, want) {
            t.Fatalf("missing %q in %v", want, cfg.Args)
        }
    }
    if strings.Contains(joined, "secret-must-not-escape") {
        t.Fatal("secret leaked into argv")
    }
}
```

- [ ] **Step 2: Run focused tests and verify RED**

Run:

```bash
GO111MODULE=on go test ./cmd/ahsir-agent -run 'TestBuildSessionConfig_Codex' -count=1
```

Expected: FAIL because `auth.json` is still copied and custom provider arguments
do not yet use `credential.envKey`.

- [ ] **Step 3: Implement safe provider arguments**

Call `ResolveCodexProvider` immediately after resolving `provider` and before
creating the isolated `CODEX_HOME`; invalid credentials must not partially
initialize the agent home. Use the resolved result only for
`provider == codex`. Append quoted TOML overrides for `model_provider`, `name`,
`base_url`, `wire_api`, and `env_key`, plus the unquoted boolean:

```go
args = append(args,
    "-c", `model_provider="ahsir_runtime"`,
    "-c", `model_providers.ahsir_runtime.name="Ahsir runtime"`,
    "-c", "model_providers.ahsir_runtime.base_url="+strconv.Quote(cfg.BaseURL),
    "-c", "model_providers.ahsir_runtime.wire_api="+strconv.Quote(cfg.WireAPI),
    "-c", "model_providers.ahsir_runtime.env_key="+strconv.Quote(cfg.EnvKey),
    "-c", `model_providers.ahsir_runtime.requires_openai_auth=false`,
)
```

Do not add provider overrides when `baseURL` is empty; that path continues to
represent the user's normal Codex configuration.

Remove only the `auth.json` copy from `mirrorCodexHomeForAgent`; retain the
existing filtered non-credential `config.toml` behavior.

- [ ] **Step 4: Verify package tests GREEN**

Run:

```bash
GO111MODULE=on go test ./cmd/ahsir-agent -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit provider construction**

```bash
git add cmd/ahsir-agent/main.go cmd/ahsir-agent/main_test.go
git commit -m "fix: isolate Codex custom provider authentication"
```

---

### Task 3: Block raw Codex authentication overrides

**Files:**
- Modify: `internal/wrapper/session_codex.go`
- Modify: `internal/wrapper/session_codex_test.go`

**Interfaces:**
- Consumes: existing `sanitizeCodexExecArgs` and
  `codexOverrideBypassesPolicy`.
- Produces: `codexOverrideOwnedByRuntime(kv string) bool`, covering provider
  and authentication keys.

- [ ] **Step 1: Add failing sanitizer tests**

Add table cases proving all of these are removed from `runtime.args`:

```text
-c model_provider="evil"
-c model_providers.evil.env_key="OPENAI_API_KEY"
-c model_providers.evil.experimental_bearer_token="literal"
-c model_providers.evil.requires_openai_auth=true
--config=model_providers.evil.base_url="https://evil.example"
```

Also retain a benign override such as `-c model_verbosity="low"`.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
GO111MODULE=on go test ./internal/wrapper -run 'TestSanitizeCodexExecArgs' -count=1
```

Expected: FAIL because current filtering blocks sandbox/approval keys only.

- [ ] **Step 3: Extend the owned-key filter**

Reject:

- exact `model_provider`;
- any key below `model_providers`;
- `openai_base_url`;
- credential-store/login overrides that could reactivate `auth.json`.

Keep the existing sandbox and approval checks. Do not inspect or log the value
portion of rejected overrides.

- [ ] **Step 4: Run package tests and verify GREEN**

```bash
GO111MODULE=on go test ./internal/wrapper -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit argument hardening**

```bash
git add internal/wrapper/session_codex.go internal/wrapper/session_codex_test.go
git commit -m "fix: reserve Codex provider auth configuration"
```

---

### Task 4: Preserve the env variable name through CMA translation

**Files:**
- Modify: `internal/cmagateway/ahsir/card.go`
- Modify: `internal/cmagateway/translate/translate.go`
- Modify: `internal/cmagateway/translate/translate_test.go`

**Interfaces:**
- Consumes: CMA metadata key `runtime_api_key_env`.
- Produces: facade card
  `Runtime.Credential.EnvKey == metadata["runtime_api_key_env"]`.

- [ ] **Step 1: Add failing translation tests**

Add:

```go
func TestAgentToCard_CodexCredentialPreservesEnvName(t *testing.T) {
    a := baseAgent()
    a.Metadata["runtime_provider"] = "codex"
    a.Metadata["runtime_base_url"] = "http://127.0.0.1:18793/v1"
    a.Metadata["runtime_api_key_env"] = "MOONSHOT_API_KEY"
    card, err := AgentToCard("cma-coder-v1", a, RuntimeDefaults{})
    if err != nil {
        t.Fatal(err)
    }
    if card.Runtime.Credential.EnvKey != "MOONSHOT_API_KEY" {
        t.Fatalf("credential=%#v", card.Runtime.Credential)
    }
    if card.Runtime.APIKey != "" {
        t.Fatalf("legacy apiKey populated: %q", card.Runtime.APIKey)
    }
}
```

Keep the malformed-name test and assert that it does not consult the named
environment variable.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
GO111MODULE=on go test ./internal/cmagateway/translate -run 'TestAgentToCard_.*Runtime|TestAgentToCard_CodexCredential' -count=1
```

Expected: compile failure because the facade mirror lacks `Credential`.

- [ ] **Step 3: Implement translation**

Mirror `RuntimeCredentialConfig` and `WireAPI` in
`internal/cmagateway/ahsir/card.go`. Change `AgentToCard` so
`runtime_api_key_env` populates `Credential.EnvKey` directly, not
`APIKey = "${...}"`. Set `WireAPI` from metadata key `runtime_wire_api` when
present; otherwise leave it empty so Ahsir applies the `responses` default.

Do not call `os.Getenv` or `os.LookupEnv` in the translation package.

- [ ] **Step 4: Run CMA and full source tests**

```bash
GO111MODULE=on go test ./internal/cmagateway/... -count=1
GO111MODULE=on go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit CMA translation**

```bash
git add internal/cmagateway/ahsir/card.go internal/cmagateway/translate/translate.go internal/cmagateway/translate/translate_test.go
git commit -m "fix: preserve Codex credential env names through CMA"
```

---

### Task 5: Document migration and sync the plugin source

**Files:**
- Modify: `README.md`
- Modify: `docs/EVENTBUS-SOURCES.md`
- Modify: `plugin/src/**` through `make plugin`

**Interfaces:**
- Consumes: completed source implementation from Tasks 1–4.
- Produces: user-facing env-only examples and a buildable marketplace plugin
  source bundle.

- [ ] **Step 1: Update documentation examples**

Replace Codex custom-provider examples with:

```yaml
runtime:
  provider: codex
  model: k3
  baseURL: http://127.0.0.1:18793/v1
  wireAPI: responses
  credential:
    envKey: MOONSHOT_API_KEY
```

Document that `runtime.apiKey` is invalid for Codex and that the named variable
must be exported in the Ahsir service environment. Add a non-destructive
discovery command for old files:

```bash
find <agent-workspace-root> -path '*/.a2a/codex-home/auth.json' -print
```

State that deletion is manual because a file may contain a ChatGPT login.

- [ ] **Step 2: Verify docs contain no insecure Codex examples**

Run:

```bash
rg -n 'provider:[[:space:]]*codex|CODEX_API_KEY|runtime\\.apiKey|auth\\.json' README.md docs example
```

Expected: every Codex credential example uses `credential.envKey`; remaining
`runtime.apiKey` and `auth.json` occurrences are migration warnings only.

- [ ] **Step 3: Refresh the generated plugin bundle**

Run:

```bash
make plugin
```

Expected: `plugin/src` is regenerated from `cmd`, `internal`, `go.mod`, and
`go.sum`, including the same env-only implementation and tests.

- [ ] **Step 4: Verify source and bundled plugin**

Run:

```bash
GO111MODULE=on go test ./... -count=1
(cd plugin/src && GO111MODULE=on go test ./... -count=1)
```

Expected: both PASS.

- [ ] **Step 5: Commit docs and generated bundle**

```bash
git add README.md docs/EVENTBUS-SOURCES.md plugin/src
git commit -m "docs: migrate Codex agents to env credentials"
```

---

### Task 6: Verify security invariants and perform opt-in Kimi smoke test

**Files:**
- Modify only if verification exposes a defect in files from Tasks 1–5.
- Do not modify `~/.cma-stack/watches.yaml` or delete credential files without
  a separate explicit operator action.

**Interfaces:**
- Consumes: complete implementation.
- Produces: evidence that tests pass, secrets remain absent, and Kimi can serve
  fresh and resumed turns.

- [ ] **Step 1: Run formatting and static checks**

```bash
gofmt -w internal/wrapper/card.go internal/wrapper/runtime.go internal/wrapper/runtime_test.go \
  cmd/ahsir-agent/main.go cmd/ahsir-agent/main_test.go \
  internal/wrapper/session_codex.go internal/wrapper/session_codex_test.go \
  internal/cmagateway/ahsir/card.go internal/cmagateway/translate/translate.go \
  internal/cmagateway/translate/translate_test.go
GO111MODULE=on go vet ./...
git diff --check
```

Expected: no output from `gofmt`/`git diff --check`; `go vet` exits 0.

- [ ] **Step 2: Run the mandatory race suite**

```bash
GO111MODULE=on go test -race -count=1 ./...
```

Expected: PASS.

- [ ] **Step 3: Run secret-persistence checks with sentinel data**

Run tests using a sentinel value such as `secret-must-not-escape`, then search
only generated test workspaces and tracked source—not the real key or global
credential files:

```bash
rg -n 'secret-must-not-escape|CODEX_API_KEY' cmd internal plugin/src README.md docs
```

Expected: sentinel occurrences are test assertions/fixtures only;
`CODEX_API_KEY` occurs only in migration/security assertions, never production
mapping.

- [ ] **Step 4: Run an opt-in Kimi fresh/resume smoke test**

Preconditions:

```bash
test -n "$MOONSHOT_API_KEY"
test -x "$(command -v codex)"
```

Use a temporary agent workspace and the existing local Responses proxy. Run one
fresh turn, capture only the Codex `thread_id`, then run one resume turn. Do not
print the environment or HTTP headers. Verify:

- both commands exit 0;
- both replies are non-empty;
- the temporary `.a2a/codex-home` has no `auth.json`;
- argv/log output contains `MOONSHOT_API_KEY` only as a name;
- argv/log output does not contain the key value.

If the local proxy or key is unavailable, record the smoke test as skipped; do
not weaken unit/integration acceptance.

- [ ] **Step 5: Record final status without committing deployment mutations**

Report:

- source and plugin test commands;
- race result;
- fresh/resume smoke result or exact skip reason;
- discovered old per-agent `auth.json` paths as cleanup candidates, without
  deleting them;
- the required follow-up to rebuild/restart the deployed Ahsir stack and
  re-apply `watches.yaml`.
