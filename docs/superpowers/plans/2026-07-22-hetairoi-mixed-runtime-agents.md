# Hetairoi Mixed Runtime Agents Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` or `superpowers:executing-plans` to implement this plan task-by-task.

**Goal:** 让 Hetairoi coder 使用 Codex CLI + `gpt-5.6-sol`，reviewer 使用 Claude Code adapter + Kimi Code `k3`，并在真实 smoke turn 通过后原生重试 GitHub issue #29。

**Architecture:** CMA agent 的 `model.id` 继续决定 runtime model；三个可选 metadata 字段覆盖 facade 的全局 runtime defaults。`AgentToCard` 验证 API key 环境变量名并返回 error，`ensureRegistered` fail loud。代码验证后同时部署匹配的 `ahsir`/`ahsir-agent` binaries，通过 login zsh 给 launchd 服务继承现有 `MOONSHOT_API_KEY`，再以不可破坏的 agent version update 应用配置。

**Tech Stack:** Go、CMA facade HTTP API、ahsir scheduler、Codex CLI、Claude Code、Kimi Code Anthropic-compatible API、launchd、zsh、jq

**Design spec:** `docs/superpowers/specs/2026-07-22-hetairoi-mixed-runtime-agents-design.md`

## Global constraints

- 遵循 TDD：测试必须先失败，再写最小实现，再重构。
- 不输出、复制或持久化 `MOONSHOT_API_KEY` 明文；只允许 `${MOONSHOT_API_KEY}` 引用。
- 不删除 CMA agent versions、workspaces、sessions、transcripts 或 Hetairoi event history。
- 不修改 `ahsir-build`、`ahsir-fix`、`ahsir-review` 的 handler routing。
- 不把 reviewer 的 provider 改成 `codex`：`provider: anthropic` 表示 Claude Code adapter，服务与模型仍为 Kimi。
- 不 push 仓库；代码与运行配置完成后只做本地 commits。
- active binary 必须成对部署；禁止只替换 `ahsir` 或只替换 `ahsir-agent`。
- smoke turn 未全部通过前，不 retry issue #29。

## File map

- Modify: `internal/cmagateway/translate/translate_test.go` — runtime override contract tests and signature migration.
- Modify: `internal/cmagateway/translate/translate.go` — validate/apply per-agent runtime metadata.
- Modify: `internal/cmagateway/handlers.go` — propagate card translation failure.
- Modify: `/Users/wu8685/Library/LaunchAgents/com.wu8685.ahsir.plist` — start the unchanged command through login zsh; this is runtime configuration and is not committed.
- Create: timestamped sibling backup of the plist before editing; this is not committed.
- Runtime update only: coder `agent_y5xg4yka5pwqhwz5qzthk3t6wu` and reviewer `agent_4gzyyzf7ee4hh57ijjftcurav4` latest versions through CMA facade `http://127.0.0.1:18790`.

---

## Task 1: Specify runtime override behavior with failing translation tests

**Files:**

- Modify: `internal/cmagateway/translate/translate_test.go`
- Test: `internal/cmagateway/translate/translate_test.go`

- [ ] **Step 1: Migrate existing calls to the error-returning contract**

Add a helper local to the test file:

```go
func mustAgentCard(t *testing.T, a *cma.Agent, d RuntimeDefaults) *ahsir.AgentCard {
	t.Helper()
	card, err := AgentToCard("cma-coder-v1", a, d)
	if err != nil {
		t.Fatalf("AgentToCard() error = %v", err)
	}
	return card
}
```

Import `internal/cmagateway/ahsir`, then replace the existing one-value `AgentToCard` calls with `mustAgentCard`. This is a compile-first migration only; do not change production code yet.

- [ ] **Step 2: Add the complete runtime override table**

Add table-driven tests that assert exact `Provider`, `BaseURL`, `APIKey`, and `Model` values for:

```go
{
	name: "global defaults unchanged",
	metadata: map[string]string{},
	wantProvider: "anthropic",
	wantBaseURL: "https://global.example/",
	wantAPIKey: "${GLOBAL_API_KEY}",
	wantModel: "claude-x",
},
{
	name: "codex provider only",
	metadata: map[string]string{"runtime_provider": "codex"},
	wantProvider: "codex",
	wantBaseURL: "https://global.example/",
	wantAPIKey: "${GLOBAL_API_KEY}",
	wantModel: "gpt-5.6-sol",
},
{
	name: "kimi full override",
	metadata: map[string]string{
		"runtime_provider": "anthropic",
		"runtime_base_url": "https://api.kimi.com/coding/",
		"runtime_api_key_env": "MOONSHOT_API_KEY",
	},
	wantProvider: "anthropic",
	wantBaseURL: "https://api.kimi.com/coding/",
	wantAPIKey: "${MOONSHOT_API_KEY}",
	wantModel: "k3",
},
{
	name: "empty values inherit defaults",
	metadata: map[string]string{
		"runtime_provider": "",
		"runtime_base_url": "",
		"runtime_api_key_env": "",
	},
	// all runtime values remain global defaults
},
```

Set each case's `a.Model.ID` explicitly where the expected model differs. Empty overrides must not erase defaults.

- [ ] **Step 3: Add API key environment-name validation tests**

Add success cases for `_KEY`, `A`, `A1`, and `MOONSHOT_API_KEY`. Add rejected cases for `1KEY`, `KEY-NAME`, `KEY NAME`, `${KEY}`, and `KEY=value`. Each rejected case must assert:

```go
card, err := AgentToCard("cma-coder-v1", a, RuntimeDefaults{})
if err == nil || card != nil {
	t.Fatalf("AgentToCard() = (%v, %v), want (nil, error)", card, err)
}
if !strings.Contains(err.Error(), "runtime_api_key_env") {
	t.Fatalf("error = %q, want metadata key", err)
}
```

- [ ] **Step 4: Run the focused test and capture red**

Run:

```bash
go test ./internal/cmagateway/translate -run 'TestAgentToCard' -count=1
```

Expected: compile/test failure because production `AgentToCard` still returns one value and does not implement overrides. This is the required red evidence.

- [ ] **Step 5: Commit the red tests**

```bash
git add internal/cmagateway/translate/translate_test.go
git commit -m "test: specify per-agent runtime overrides"
```

---

## Task 2: Implement validated per-agent runtime overrides

**Files:**

- Modify: `internal/cmagateway/translate/translate.go`
- Test: `internal/cmagateway/translate/translate_test.go`

- [ ] **Step 1: Add metadata constants and strict env-name validation**

Define unexported constants near `RuntimeDefaults`:

```go
const (
	metadataRuntimeProvider  = "runtime_provider"
	metadataRuntimeBaseURL   = "runtime_base_url"
	metadataRuntimeAPIKeyEnv = "runtime_api_key_env"
)
```

Add a compiled package-level regexp:

```go
var shellEnvNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
```

Import `regexp`. Do not call `os.Getenv`: translation creates a symbolic `${NAME}` reference and runtime expansion remains the wrapper's responsibility.

- [ ] **Step 2: Change `AgentToCard` signature and resolve overrides before card construction**

Change:

```go
func AgentToCard(name string, a *cma.Agent, d RuntimeDefaults) (*ahsir.AgentCard, error)
```

Start from defaults, then apply only non-empty metadata values:

```go
provider, baseURL, apiKey := d.Provider, d.BaseURL, d.APIKey
if value := strings.TrimSpace(a.Metadata[metadataRuntimeProvider]); value != "" {
	provider = value
}
if value := strings.TrimSpace(a.Metadata[metadataRuntimeBaseURL]); value != "" {
	baseURL = value
}
if envName := strings.TrimSpace(a.Metadata[metadataRuntimeAPIKeyEnv]); envName != "" {
	if !shellEnvNameRE.MatchString(envName) {
		return nil, fmt.Errorf("invalid %s %q: must be a shell environment variable name", metadataRuntimeAPIKeyEnv, envName)
	}
	apiKey = "${" + envName + "}"
}
```

Use those resolved variables in `RuntimeConfig`. Return `card, nil` only after the complete card is built.

- [ ] **Step 3: Run focused tests to green**

```bash
go test ./internal/cmagateway/translate -run 'TestAgentToCard' -count=1
```

Expected: all existing metadata mapping tests and all new runtime override tests pass.

- [ ] **Step 4: Refactor comments without changing behavior**

Update the `AgentToCard` doc comment to list the three runtime metadata mappings and explain that `runtime_api_key_env` becomes a symbolic card reference. Keep model mapping and all pre-existing metadata behavior intact.

- [ ] **Step 5: Commit the implementation**

```bash
git add internal/cmagateway/translate/translate.go internal/cmagateway/translate/translate_test.go
git commit -m "feat: support per-agent runtime overrides"
```

---

## Task 3: Propagate translation failures through CMA registration

**Files:**

- Modify: `internal/cmagateway/handlers.go`

The translation package already owns the validation red/green test. This task is the required compile migration at its only production call site; do not introduce a new server fake solely to test a direct error return.

- [ ] **Step 1: Make `ensureRegistered` return translation errors**

Replace the translation call with:

```go
card, err := translate.AgentToCard(ahsirName, agent, s.rt)
if err != nil {
	return fmt.Errorf("translate agent card: %w", err)
}
instances := translate.Instances(agent)
```

`handlers.go` already imports `fmt`; reuse it. Do not mark the versioned name registered after translation or registration failure.

- [ ] **Step 2: Run the CMA gateway suite**

```bash
go test ./internal/cmagateway/... -count=1
```

Expected: all tests pass and no one-value `AgentToCard` call remains:

```bash
rg -n 'AgentToCard\(' internal/cmagateway
```

- [ ] **Step 3: Commit propagation**

```bash
git add internal/cmagateway/handlers.go
git commit -m "fix: surface invalid agent runtime metadata"
```

---

## Task 4: Verify repository behavior and build a matched binary pair

**Files:**

- Verify: all repository Go packages.
- Build only: a new temporary directory; do not overwrite the active installation yet.

- [ ] **Step 1: Run formatting and inspect the diff**

```bash
gofmt -w internal/cmagateway/translate/translate.go internal/cmagateway/translate/translate_test.go internal/cmagateway/handlers.go
git diff --check
git diff --stat
```

If a handler-level test was changed, include it in `gofmt`.

- [ ] **Step 2: Run required verification**

```bash
go test ./internal/cmagateway/... -count=1
go test ./... -count=1
go test -race ./... -count=1
go build ./...
```

Expected: every command exits 0. The race suite is mandatory because the repository Makefile defines it as the trusted pre-merge signal.

- [ ] **Step 3: Review scope and secrets**

```bash
git diff HEAD~3 -- internal/cmagateway docs/superpowers
git grep -n 'MOONSHOT_API_KEY=' -- . ':(exclude)docs/superpowers/plans'
git status --short
```

Expected: no key assignment or token value; only planned source/tests/docs changes.

- [ ] **Step 4: Build both binaries into an isolated directory**

```bash
deploy_dir="$(mktemp -d /tmp/ahsir-mixed-runtime.XXXXXX)"
version="$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' plugin/.claude-plugin/plugin.json | head -1)"
ldflags="-X github.com/wu8685/ahsir/internal/version.Version=${version:-dev}"
go build -ldflags "$ldflags" -o "$deploy_dir/ahsir" ./cmd/ahsir
go build -ldflags "$ldflags" -o "$deploy_dir/ahsir-agent" ./cmd/ahsir-agent
test -x "$deploy_dir/ahsir"
test -x "$deploy_dir/ahsir-agent"
shasum -a 256 "$deploy_dir/ahsir" "$deploy_dir/ahsir-agent"
```

Keep `deploy_dir` in the implementing shell for Task 6. Do not print environment variables.

- [ ] **Step 5: Commit any formatting-only correction**

If `gofmt` changed tracked files after the previous commits:

```bash
git add <exact-files>
git commit -m "style: format runtime override changes"
```

Otherwise do not create an empty commit.

---

## Task 5: Prepare a secret-safe launchd configuration

**Files:**

- Create: `/Users/wu8685/Library/LaunchAgents/com.wu8685.ahsir.plist.pre-mixed-runtime-<timestamp>`
- Modify: `/Users/wu8685/Library/LaunchAgents/com.wu8685.ahsir.plist`

- [ ] **Step 1: Verify prerequisites without revealing secrets**

```bash
/bin/zsh -lc 'test -n "${MOONSHOT_API_KEY:-}"'
test -x /Users/wu8685/.cma-stack/bin/ahsir
test -x /Users/wu8685/.cma-stack/bin/ahsir-agent
launchctl print "gui/$(id -u)/com.wu8685.ahsir" >/dev/null
```

Expected: all exit 0. Never run `env`, `set`, `launchctl print` unfiltered, or `plutil -p` on the whole plist because unrelated secrets may be present.

- [ ] **Step 2: Create a non-clobbering plist backup**

```bash
stamp="$(date +%Y%m%d-%H%M%S)"
plist=/Users/wu8685/Library/LaunchAgents/com.wu8685.ahsir.plist
plist_backup="${plist}.pre-mixed-runtime-${stamp}"
test ! -e "$plist_backup"
cp -p "$plist" "$plist_backup"
cmp -s "$plist" "$plist_backup"
```

- [ ] **Step 3: Replace only `ProgramArguments` using `apply_patch`**

Preserve the current command and every other plist key. Change the argument vector from direct binary invocation to:

```xml
<array>
  <string>/bin/zsh</string>
  <string>-lc</string>
  <string>exec /Users/wu8685/.cma-stack/bin/ahsir start --cma-listen 127.0.0.1:18790 --cma-scheduler http://127.0.0.1:9800 --cma-state /Users/wu8685/.cma-stack/cma-facade-state.json /Users/wu8685/.cma-stack/ahsir.yaml</string>
</array>
```

Do not insert `MOONSHOT_API_KEY` or any expanded value into the plist.

- [ ] **Step 4: Validate the plist and scope**

```bash
plutil -lint "$plist"
/usr/libexec/PlistBuddy -c 'Print :ProgramArguments' "$plist"
diff -u "$plist_backup" "$plist" | sed -E 's/(token|key|secret)[^<]*/\1=REDACTED/Ig'
```

Expected: valid plist; only `ProgramArguments` differs. If unrelated secret-bearing lines appear, stop displaying the diff and inspect only the key path through `PlistBuddy`.

---

## Task 6: Atomically deploy and restart the local ahsir service

**Files:**

- Replace with backups: `/Users/wu8685/.cma-stack/bin/ahsir`
- Replace with backups: `/Users/wu8685/.cma-stack/bin/ahsir-agent`

- [ ] **Step 1: Back up the active matched pair**

Reuse `stamp` and `deploy_dir` from Tasks 4–5:

```bash
cp -p /Users/wu8685/.cma-stack/bin/ahsir "/Users/wu8685/.cma-stack/bin/ahsir.pre-mixed-runtime-${stamp}"
cp -p /Users/wu8685/.cma-stack/bin/ahsir-agent "/Users/wu8685/.cma-stack/bin/ahsir-agent.pre-mixed-runtime-${stamp}"
```

- [ ] **Step 2: Stop the launch agent, install both binaries, and bootstrap**

```bash
domain="gui/$(id -u)"
launchctl bootout "$domain/com.wu8685.ahsir"
install -m 0755 "$deploy_dir/ahsir" /Users/wu8685/.cma-stack/bin/ahsir
install -m 0755 "$deploy_dir/ahsir-agent" /Users/wu8685/.cma-stack/bin/ahsir-agent
launchctl bootstrap "$domain" "$plist"
launchctl kickstart -k "$domain/com.wu8685.ahsir"
```

This is a bounded service restart. If install or bootstrap fails, restore both binary backups and the plist backup before bootstrapping again.

- [ ] **Step 3: Wait for all service surfaces**

Poll for at most 60 seconds:

```bash
for i in {1..30}; do
  curl -fsS http://127.0.0.1:9800/agents >/dev/null \
    && curl -fsS http://127.0.0.1:18790/v1/agents >/dev/null \
    && curl -fsS http://127.0.0.1:18791/v1/eventbus/handlers >/dev/null \
    && curl -fsS http://127.0.0.1:19801/api/agents >/dev/null \
    && break
  sleep 2
done
```

Then run each curl once without suppressing only its HTTP status, not response bodies that may contain credentials.

- [ ] **Step 4: Prove the service inherited the key without printing it**

After the reviewer card is registered in Task 8, a successful Kimi smoke turn is the authoritative proof. Before that, only verify the process came through zsh by inspecting its command path; do not read its environment through `ps e`.

---

## Task 7: Create safe v2 CMA agent configurations

**Runtime API:** `http://127.0.0.1:18790`

- [ ] **Step 1: Snapshot current agents with redacted structural output**

```bash
cma=http://127.0.0.1:18790
coder_id=agent_y5xg4yka5pwqhwz5qzthk3t6wu
reviewer_id=agent_4gzyyzf7ee4hh57ijjftcurav4
curl -fsS "$cma/v1/agents/$coder_id" | jq '{id,version,name,model,metadata,tools_count:(.tools|length),skills_count:(.skills|length),mcp_count:(.mcp_servers|length)}'
curl -fsS "$cma/v1/agents/$reviewer_id" | jq '{id,version,name,model,metadata,tools_count:(.tools|length),skills_count:(.skills|length),mcp_count:(.mcp_servers|length)}'
```

Expected before update: both are version 1 and retain `runtime_timeout=0`, `shell_access=true`. Stop if IDs/names differ from the approved mapping.

- [ ] **Step 2: Update coder by copying the full latest request shape**

Pipe GET → `jq` → POST so no temporary payload contains user prompts or secrets:

```bash
curl -fsS "$cma/v1/agents/$coder_id" \
  | jq '{name,model,system,description,tools,skills,mcp_servers,metadata}
      | .model.id = "gpt-5.6-sol"
      | .metadata.runtime_provider = "codex"
      | del(.metadata.runtime_base_url, .metadata.runtime_api_key_env)' \
  | curl -fsS -X POST -H 'Content-Type: application/json' --data-binary @- "$cma/v1/agents/$coder_id" \
  | jq '{id,version,name,model,metadata}'
```

Expected: same ID, version 2, model `gpt-5.6-sol`, provider `codex`; all old metadata remains.

- [ ] **Step 3: Update reviewer by copying the full latest request shape**

```bash
curl -fsS "$cma/v1/agents/$reviewer_id" \
  | jq '{name,model,system,description,tools,skills,mcp_servers,metadata}
      | .model.id = "k3"
      | .metadata.runtime_provider = "anthropic"
      | .metadata.runtime_base_url = "https://api.kimi.com/coding/"
      | .metadata.runtime_api_key_env = "MOONSHOT_API_KEY"' \
  | curl -fsS -X POST -H 'Content-Type: application/json' --data-binary @- "$cma/v1/agents/$reviewer_id" \
  | jq '{id,version,name,model,metadata}'
```

Expected: same ID, version 2, model `k3`, symbolic env name only; no API key value enters CMA state.

- [ ] **Step 4: Verify non-runtime fields were preserved**

For each ID, compare version 1 and version 2 after deleting expected differences (`version`, timestamps, `model`, and the three runtime metadata keys). Use `jq -S` and `diff`; expected output is empty. Explicitly compare the full `system`, `tools`, `skills`, `mcp_servers`, description, and all unrelated metadata.

- [ ] **Step 5: Verify handlers still reference stable IDs**

```bash
curl -fsS http://127.0.0.1:18791/v1/eventbus/handlers \
  | jq '.data[] | select(.name == "ahsir-build" or .name == "ahsir-fix" or .name == "ahsir-review") | {name,agent_id:.policy.agent_id,env_id:.policy.env_id}'
```

Expected: build/fix still reference coder base ID; review still references reviewer base ID.

---

## Task 8: Register and smoke-test both latest agent versions

- [ ] **Step 1: Resolve a live environment and create a fresh coder session**

```bash
handlers_json="$(curl -fsS http://127.0.0.1:18791/v1/eventbus/handlers)"
environment_id="$(jq -er '.data[] | select(.name == "ahsir-build") | .policy.env_id' <<<"$handlers_json")"
jq -e --arg env "$environment_id" \
  '[.data[] | select(.name == "ahsir-build" or .name == "ahsir-fix" or .name == "ahsir-review") | .policy.env_id] | all(. == $env)' \
  <<<"$handlers_json" >/dev/null
curl -fsS "$cma/v1/environments/$environment_id" | jq -e '.archived_at == null' >/dev/null
coder_session="$(curl -fsS -X POST -H 'Content-Type: application/json' \
  -d "{\"agent\":\"$coder_id\",\"environment_id\":\"$environment_id\",\"title\":\"verify-codex-coder-20260722\"}" \
  "$cma/v1/sessions")"
coder_session_id="$(jq -er '.id' <<<"$coder_session")"
jq '{id,status,agent:{id:.agent.id,version:.agent.version,name:.agent.name,model:.agent.model}}' <<<"$coder_session"
```

Expected: resolved agent version 2, model `gpt-5.6-sol`.

- [ ] **Step 2: Send an exact-token coder turn and poll its event log**

```bash
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"events":[{"type":"user.message","content":[{"type":"text","text":"这是 runtime 连通性测试。不要调用工具，只回复 CODEX_CODER_OK。"}]}]}' \
  "$cma/v1/sessions/$coder_session_id/events" >/dev/null

for i in {1..60}; do
  coder_events="$(curl -fsS "$cma/v1/sessions/$coder_session_id/events")"
  jq -e '.data[] | select(.type == "agent.message") | .content[] | select(.type == "text" and .text == "CODEX_CODER_OK")' \
    <<<"$coder_events" >/dev/null && break
  jq -e '.data[] | select(.error != null)' <<<"$coder_events" >/dev/null && { jq '.data[] | select(.error != null)' <<<"$coder_events"; exit 1; }
  sleep 2
done
jq -e '.data[] | select(.type == "agent.message") | .content[] | select(.type == "text" and .text == "CODEX_CODER_OK")' \
  <<<"$coder_events" >/dev/null
```

Expected exact response:

```text
这是 runtime 连通性测试。不要调用工具，只回复 CODEX_CODER_OK。
```

Expected: exact token `CODEX_CODER_OK`; session/agent metadata resolves to coder v2; no Anthropic subscription error.

- [ ] **Step 3: Inspect the generated coder card without credentials**

Locate the registered v2 card from scheduler state/agent workspace and inspect only:

```yaml
runtime:
  provider: codex
  model: gpt-5.6-sol
```

Expected: no Kimi base URL or API key reference on coder.

- [ ] **Step 4: Create a fresh reviewer session and run an exact-token turn**

Repeat the session creation flow with `$reviewer_id`, title `verify-kimi-reviewer-20260722`, and save `reviewer_session_id`. POST:

```text
这是 runtime 连通性测试。不要调用工具，只回复 KIMI_REVIEWER_OK。
```

Use the same bounded event polling, substituting `KIMI_REVIEWER_OK`. Fail immediately on an event with non-null `error`.

Expected: exact token `KIMI_REVIEWER_OK`; resolved agent name ends in `-v2`; no Anthropic subscription 403.

- [ ] **Step 5: Inspect only reviewer runtime fields**

Expected card fragment:

```yaml
runtime:
  provider: anthropic
  baseURL: https://api.kimi.com/coding/
  apiKey: ${MOONSHOT_API_KEY}
  model: k3
```

Do not expand or echo `apiKey`.

- [ ] **Step 6: Stop on any smoke failure**

If coder or reviewer fails, do not retry #29. Preserve the context ID and sanitized error. Repair or roll back only the failing runtime; do not silently fall back to Anthropic subscription.

---

## Task 9: Retry issue #29 through Hetairoi and verify coding starts

- [ ] **Step 1: Trigger the persisted event with a fresh session**

```bash
curl -fsS -X POST \
  -H 'Content-Type: application/json' \
  -d '{"subject":"29","fresh_session":true}' \
  http://127.0.0.1:18791/v1/eventbus/handlers/ahsir-build/retry | jq .
```

Expected: accepted retry referencing issue subject `29` and a new context/session.

- [ ] **Step 2: Observe bounded progress**

Poll Hetairoi history/status and the new context for up to the configured coder runtime timeout. Expected evidence:

- latest resolved agent is coder v2;
- the turn enters Codex execution;
- transcript contains repository inspection or coding activity;
- no Claude subscription 403.

Do not manufacture a second GitHub label event while the native retry is active.

- [ ] **Step 3: Record the result**

If the coder creates a branch/commit/PR, report the exact URL and state. If it needs review, allow the existing `pr.push` handler to dispatch reviewer v2. Do not merge without the user's explicit request.

---

## Task 10: Final verification, review, and handoff

- [ ] **Step 1: Re-run repository verification**

```bash
git diff --check
go test ./internal/cmagateway/... -count=1
go test -race ./... -count=1
go build ./...
git status --short
```

- [ ] **Step 2: Review against the design spec**

Check every acceptance criterion in `docs/superpowers/specs/2026-07-22-hetairoi-mixed-runtime-agents-design.md`, especially unchanged agents, secret handling, actual provider/model, and native retry behavior.

- [ ] **Step 3: Commit any remaining planned source change**

```bash
git add <exact-planned-files>
git commit -m "feat: configure hetairoi mixed runtimes"
```

Do not commit the user-level plist, CMA state, generated cards, logs, or credentials. Do not create an empty commit.

- [ ] **Step 4: Report evidence and rollback handles**

Report:

- test/build commands and exit status;
- coder/reviewer latest version and sanitized runtime fields;
- both smoke tokens;
- issue #29 retry context and coding/PR outcome;
- local commit hashes;
- timestamped plist and binary backup paths;
- confirmation that nothing was pushed.

## Rollback procedure

1. Stop `com.wu8685.ahsir` with `launchctl bootout`.
2. Restore both timestamp-matched binaries and the saved plist; bootstrap the service again.
3. Through `POST /v1/agents/{id}`, create new versions from the current full payload, restore `model.id=claude-opus-4-8`, and delete only `runtime_provider`, `runtime_base_url`, `runtime_api_key_env` metadata.
4. Preserve every prior agent version and all Hetairoi/CMA history; never use destructive git reset or delete workspace data.
5. If only the source change must be undone, create a normal revert commit after user approval.
