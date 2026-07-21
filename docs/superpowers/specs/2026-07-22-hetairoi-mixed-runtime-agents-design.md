# Hetairoi 异构 Runtime Agent 配置设计

日期：2026-07-22

## 背景

Hetairoi 的 `ahsir-build`、`ahsir-fix` 和 `ahsir-review` handlers 当前引用两个 CMA agents：

- `ahsir-coder`：同时承担 build 与 fix；
- `ahsir-reviewer`：承担独立评审。

两者目前都由 CMA facade 的全局 `RuntimeDefaults` 生成 `provider: anthropic`、`model: claude-opus-4-8` 的 ahsir agent card。2026-07-22，issue #29 被正常摄入并触发，但 coder 在启动后因 Claude Code subscription 被组织禁用而返回 403，未进入 coding。

用户选择将两者改为异构模型：

- coder：Codex CLI + `gpt-5.6-sol`；
- reviewer：Claude Code 执行器 + Kimi Code `k3`。

## 目标

1. 允许同一个 CMA facade 下的不同 CMA agents 覆盖全局 runtime provider、base URL 与 API key 环境变量引用。
2. 不改变未配置 override 的现有 agents 行为。
3. 不把 API key 明文写入 CMA state、agent card、launchd plist、日志或仓库。
4. 将 Hetairoi coder 与 reviewer 迁移到上述异构 runtime。
5. 验证两个 agents 均可完成真实最小 turn，然后重放 issue #29。

## 非目标

- 不拆分第二套 scheduler 或 CMA facade。
- 不改变 coder/reviewer 的 system prompt、工具权限、filesystem、skills、MCP、timeout 或 handler routing。
- 不删除旧 agent versions、workspace、sessions 或 transcripts。
- 不修改 Hetairoi 的 source/handler 协议。
- 不自动 push ahsir 或 brain-spark 仓库。

## 现状约束

`internal/cmagateway/translate.AgentToCard` 目前总是从进程级 `RuntimeDefaults` 复制：

- `Provider`
- `BaseURL`
- `APIKey`

只有模型来自 CMA agent 的 `model.id`。因此直接编辑生成的 `.a2a/agent-card.yaml` 不可靠：下一次 fresh session 调用 `ensureRegistered` 时，inline registration 会按全局 defaults 重新生成并覆盖 card。

## 设计

### 1. Per-agent runtime metadata

CMA agent `metadata` 新增三个可选键：

| metadata key | 作用 | 示例 |
|---|---|---|
| `runtime_provider` | 覆盖全局 provider | `codex`、`anthropic` |
| `runtime_base_url` | 覆盖全局 base URL | `https://api.kimi.com/coding/` |
| `runtime_api_key_env` | 指定 API key 所在环境变量名 | `MOONSHOT_API_KEY` |

模型继续由标准字段 `model.id` 提供，不新增重复的 metadata key。

优先级：

1. 非空 per-agent metadata override；
2. CMA facade 的 `RuntimeDefaults`；
3. ahsir wrapper 自身的 provider 默认行为。

`runtime_api_key_env` 只接受 shell 环境变量名格式 `[A-Za-z_][A-Za-z0-9_]*`。合法值 `NAME` 被翻译为 agent card 中的 `${NAME}`；不得接受或持久化明文 token。非法变量名必须 fail loud，使 agent 注册失败并返回清晰错误，不得静默回退到全局 key。

为支持 fail-loud，`AgentToCard` 改为返回 `(*AgentCard, error)`，调用方必须传播 validation error。

### 2. Agent 映射

#### `ahsir-coder`

- `model.id`: `gpt-5.6-sol`
- metadata 新增：
  - `runtime_provider=codex`
- 不设置 base URL 或 API key override，使用本机 Codex CLI 登录态。
- 保留现有 `runtime_timeout=0`、`shell_access=true` 与完整 system prompt。

#### `ahsir-reviewer`

- `model.id`: `k3`
- metadata 新增：
  - `runtime_provider=anthropic`
  - `runtime_base_url=https://api.kimi.com/coding/`
  - `runtime_api_key_env=MOONSHOT_API_KEY`
- `provider: anthropic` 在这里仅表示 Claude Code / Anthropic-compatible transport；底层模型与服务是 Kimi Code K3，不使用 Anthropic subscription。
- 保留现有 `runtime_timeout=0`、`shell_access=true` 与完整 system prompt。

两个 agent 都通过 `POST /v1/agents/{id}` 创建新 version；请求必须复制旧 version 的所有非 runtime 字段，再只修改 model 与上述 metadata。handlers 继续引用稳定的 base agent ID，fresh session 自动解析到 latest version。

### 3. launchd 环境传递

当前交互 shell 与 login shell 都能取得 `MOONSHOT_API_KEY`，但 launchd 的 `com.wu8685.ahsir` 环境中没有该变量。

为避免把 key 明文写入 plist，`com.wu8685.ahsir.plist` 的 `ProgramArguments` 改为通过 `/bin/zsh -lc` 执行原有 ahsir 启动命令。login shell 负责加载用户现有的安全环境配置，随后 `exec` ahsir；原有 plist `EnvironmentVariables`、working directory、日志路径和 KeepAlive 保持不变。

变更前必须创建带时间戳的 plist 备份。加载后用只检查“存在/缺失”的方式验证 ahsir 进程可展开 `${MOONSHOT_API_KEY}`，任何输出都不得包含 key 值。

### 4. 数据流

1. Hetairoi handler 根据稳定 CMA agent ID 创建 fresh session。
2. CMA facade 解析该 ID 的 latest agent version。
3. `AgentToCard` 以 global defaults 为基础，应用该 agent 的 metadata overrides。
4. inline registration 写入 versioned ahsir agent card：coder 为 Codex；reviewer 为 Claude Code + Kimi K3。
5. scheduler 启动对应 agent process 并执行 turn。
6. issue #29 使用 Hetairoi 原生 retry 从持久化 last payload 重放，不修改 GitHub label/comment 来制造新事件。

## 错误处理

- metadata 环境变量名非法：注册失败，错误明确指出 `runtime_api_key_env` 非法。
- reviewer 启动环境缺少 `MOONSHOT_API_KEY`：ahsir `expandStrict` fail loud；不回退到 Anthropic subscription。
- Kimi endpoint 认证失败：停止 retry，保留失败证据，不输出 key。
- Codex CLI 不可用或未登录：停止 retry，不把 coder 临时切回 Anthropic。
- 任一 smoke test 失败：不触发 issue #29 retry；先回滚 runtime 配置或修复对应 provider。
- launchd 重启失败：恢复 plist 备份并重新 bootstrap/kickstart原服务。

## 测试设计

遵循 TDD，先写失败测试，再实现。

### 单元测试

覆盖 `internal/cmagateway/translate`：

1. 无 metadata override：输出与当前 global defaults 完全一致。
2. Codex override：只覆盖 provider，model 仍来自 `model.id`。
3. Kimi override：覆盖 provider/base URL，并把 `MOONSHOT_API_KEY` 转成 `${MOONSHOT_API_KEY}`。
4. 部分 override：未提供的字段继续继承 global defaults。
5. 非法 `runtime_api_key_env`：返回错误且不生成 card。
6. 合法边界：变量名允许下划线、字母开头与后续数字。

更新受 `AgentToCard` 签名影响的 handler/translation tests，并运行：

- `go test ./internal/cmagateway/...`
- `go test ./...`
- `go build ./...`

### 运行时验证

1. plist 可解析，ahsir/Hetairoi/UI 端口恢复监听。
2. coder latest card：`provider=codex`、`model=gpt-5.6-sol`，不含 Kimi/Anthropic credential。
3. reviewer latest card：`provider=anthropic`、`baseURL=https://api.kimi.com/coding/`、`apiKey=${MOONSHOT_API_KEY}`、`model=k3`。
4. coder 独立 context 只回复约定 smoke token。
5. reviewer 独立 context 只回复另一约定 smoke token。
6. 两个 smoke 均通过后，以 `fresh_session=true` 原生 retry issue #29 的 persisted key。
7. 新 context transcript 出现 coder 的实际 coding 输出，而非 Claude subscription 403。

## 回滚

1. 恢复 launchd plist 备份并重载 `com.wu8685.ahsir`。
2. 对两个 CMA agents 各创建一个新 version，恢复原 `claude-opus-4-8` model，并移除三个 runtime override metadata；不删除已经产生的 versions。
3. 重启/重新注册对应 agents；保留所有 workspace、session、transcript 与 issue #29 状态。
4. 若代码实现本身需要回滚，使用普通反向 commit；不使用 destructive git reset。

## 验收标准

- 未配置 override 的 CMA agents 行为不变。
- coder 的真实 turn 由 Codex `gpt-5.6-sol` 完成。
- reviewer 的真实 turn 由 Claude Code + Kimi Code `k3` 完成，且不会访问 Anthropic subscription。
- secret 不出现在仓库、CMA state、agent metadata、plist、日志或回复中。
- Hetairoi 成功原生 retry issue #29，并进入实际 coding。
