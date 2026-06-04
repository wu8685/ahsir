# AHSIR: Session Abstraction for Long-Running Agent Runtimes

**Status:** Implemented (Step 1 + Step 2 landed in commits `e0fb3f2` / `22f7a2e`; see §8 for status of open questions)
**Date:** 2026-05-12 (last updated 2026-06-05)
**Version:** 0.1.0

## 1. Motivation

ahsir 当前 agent 执行机制（`internal/wrapper/session.go`）：

- 每条 A2A 请求 `exec.CommandContext` 拉起一个 `claude -p`，prompt 从 stdin 喂入，stdout 收回作为回复
- 跨轮历史靠 wrapper 自己拼：`Executor` 调用 `LookupHistory(contextID)` 拉前几轮 task，`formatPriorHistory` 把它们渲染成纯文本，拼在 system prompt 之后再发给 claude

存在的问题：

- **token 成本线性增长**：每轮都全量重放历史，无 prompt cache 命中
- **历史模拟脆弱**：纯文本拼装的"对话假装"在多轮工具调用场景下保真度不足
- **进程开销**：每条请求都是一次 fork + 初始化（model 加载、MCP 启动、hooks）
- **难以扩展到 streaming**：oneshot exec 拿到完整 stdout 才返回，无法做增量 SSE

迁移目标：用**单进程长驻 + stream-json IO** 替代，让 claude 进程自己管历史；同时把 Session 抽象成 provider 中立接口，未来可以塞 codex / gemini-cli / 其他 SDK。

## 2. Goals / Non-Goals

**Goals**

- 抽象出 `Session` 接口，stream-json claude 是首个实现
- 每个 A2A `contextID` 对应一个长驻 claude 进程；进程内由 claude 自己管历史
- idle TTL 回收闲置进程，同 contextID 再次访问时自动 `--resume <sessionID>` 续接
- 流式 Event 作为底层 API；阻塞 `Turn` helper 给当前 Executor 调用
- TDD：每个组件先写测试，再实现
- 两步走：先抽接口保持 oneshot 行为不变 → 再加 stream-json impl

**Non-Goals (本 spec 不涵盖)**

- 单进程多会话多路复用（明确不需要）
- `---A2A_CALL---` 文本标记协议替换（独立工单，未来可以换成 tool_call）
- ahsir-agent 进程跨重启的 contextID→sessionID 持久化（默认内存级，未来工单）
- A2A SSE 流式回吐客户端（基础设施先就位，路由层后做）
- 替换 `formatPriorHistory` 的 TaskStore 历史落盘逻辑（Step 2 才动）

## 3. Protocol Reference (Claude stream-json)

`--input-format stream-json` 在官方文档未发布稳定 schema（[GH#24594](https://github.com/anthropics/claude-code/issues/24594)），以下基于 `anthropics/claude-agent-sdk-python` 源码 + 本地实测固化。

### 启动参数

```
claude -p --input-format stream-json --output-format stream-json --verbose [--resume <sessionID>] [--add-dir=...] [--allowedTools=...]
```

不加 `--include-partial-messages` → 默认拿到**整段 assistant message**，不是 token delta（更适合当前架构）。

### Input (stdin)

每行一个 JSON：

```json
{"type":"user","message":{"role":"user","content":"<text>"},"session_id":"<sessionID>","parent_tool_use_id":null}
```

- `\n` 结尾，无额外 framing
- `session_id` 在每条 user message 都要带（与启动时 init 事件的 sessionID 一致）
- 多轮：直接写下一行，无特殊分隔

### Output (stdout, NDJSON)

事件类型（按出现顺序）：

| 事件 | `type` 字段 | 含义 / 处理 |
|---|---|---|
| 启动钩子 | `system` + `subtype=hook_started\|hook_response` | **drop**（噪音） |
| 初始化 | `system` + `subtype=init` | 从这里拿 `session_id` |
| 助手输出 | `assistant` | `message.content[]` 数组，逐 part：`text` / `tool_use` 等 |
| 轮次结束 | `result` | 本轮终结信号；含 `is_error`, `api_error_status`, `usage`, `total_cost_usd`, `session_id` |

**⚠️ 启动时序修正（2026-05-15 实测发现）**：claude 在 stream-json 模式下**不会自动 emit `system/init`** ——它会一直等 stdin 的第一条 user message 才开始产事件流。所以握手不能在构造期阻塞等 init，得在第一个 turn 的事件流里被动捕获 session_id。这一点和 Python SDK 反推出来的描述不一致；实现以实测为准。

**关键约束**：caller 必须读完一轮的 `result` 才能发下一条 user message（Python SDK 同样这么做）。

**错误语义**：

- `result.subtype == "success"` 表示**协议交互完成**，**不代表** LLM 调用成功
- 真正的成功判定：`is_error == false && api_error_status == 0`
- LLM 错误时进程**不退出**，可以继续下一轮

### 引用

- 多轮 stdin write：`claude_agent_sdk/client.py:297-311`
- result 边界判定：`claude_agent_sdk/_internal/query.py:297-310`
- hook 事件 drop 默认行为：`claude_agent_sdk/_internal/message_parser.py:57-73`

## 4. Session Interface

```go
// Event is a turn-level event from an agent runtime. Provider-neutral.
type Event interface{ isEvent() }

// EventText is a chunk of assistant text. In non-streaming mode (default),
// the entire assistant text part arrives as a single EventText per turn.
type EventText struct{ Text string }

// EventToolUse is an informational signal that the runtime invoked a
// built-in tool (Read/Bash/MCP/...). Wrapper does not need to act on it —
// tool execution is internal to the runtime.
type EventToolUse struct {
    Name  string
    Input json.RawMessage
}

// EventTurnDone is the last event delivered before the channel closes.
// Err is non-nil for LLM/runtime errors (result.is_error). The channel
// closing is the canonical "turn finished" signal.
type EventTurnDone struct {
    Err   error
    Stats TurnStats
}

type TurnStats struct {
    InputTokens, OutputTokens int
    CostUSD                   float64
    DurationMS                int64
}

// Session is a long-running conversation with one agent runtime instance.
// Implementations must serialize Stream calls: caller has to drain the
// previous turn's channel before calling Stream again.
type Session interface {
    // Stream sends one user turn, returns a channel of events. The channel
    // is closed after EventTurnDone is delivered.
    Stream(ctx context.Context, userText string) (<-chan Event, error)

    // Turn blocks until the turn completes, returning aggregated assistant text.
    Turn(ctx context.Context, userText string) (string, error)

    // SessionID returns the runtime's session identifier (e.g. for --resume).
    // Returns "" if the session has not initialized.
    SessionID() string

    Close() error
}
```

## 5. ClaudeSession Lifecycle

```
                 init OK
   [NEW] -----> [READY] <-----+
     |            |           |
     | init fail  | Stream    | turn drained
     v            v           |
   [CLOSED]   [IN_FLIGHT] ----+
                  |
                  | proc crash / Close
                  v
              [CLOSED]
                  ^
                  | idle TTL
   [READY] --> [EVICTED]
                  |
                  | next Stream call → spawn w/ --resume
                  v
              [READY]   (preserves sessionID)
```

### 状态语义

| 状态 | claude proc | 可调用 |
|---|---|---|
| NEW | 未启动 | 内部使用 |
| READY | 活跃，待命 | Stream / Close |
| IN_FLIGHT | 活跃，正在处理一轮 | Close（强杀）；Stream 拒绝 |
| EVICTED | 已 kill，sessionID 已记录 | Stream（触发 resume）/ Close |
| CLOSED | 已 kill，不可恢复 | 无 |

### 关键流程

**新建**：

1. `exec.CommandContext` 起进程，args 含 `--input-format stream-json --output-format stream-json --verbose`
2. 启动 stdout reader goroutine
3. **不阻塞**等 init——claude 直到收到第一条 user message 才会 emit。状态直接转 READY，session_id 在第一个 turn 的事件流里被动捕获
4. drop 任何 `hook_started/response` 噪音（出现在 init 前后都可能）

**Resume（EVICTED → READY）**：

1. 同"新建"，但 args 多 `--resume <sessionID>`
2. 第一个 turn 的 init 事件返回的 sessionID 应等于原 sessionID；不等 → 当前 turn 的 `EventTurnDone.Err` 携带 resume mismatch 错误（不在构造期拒绝，因为构造期不读 stdout）

**Stream 一轮**：

1. 状态检查（必须 READY）→ 转 IN_FLIGHT
2. 构造 user message，写入 stdin（含原 sessionID）
3. reader goroutine 把后续事件分发到 channel
   - `assistant` → 拆 `content` 数组，按 part 分发 `EventText` / `EventToolUse`
   - `result` → `EventTurnDone`（`Err` 来自 `is_error`），close channel，转 READY
   - process EOF / 异常 → `EventTurnDone{Err}`，close channel，转 EVICTED

**Close**：kill 进程，转 CLOSED。

## 6. SessionPool

```go
type SessionPool struct {
    factory func(ctx context.Context, contextID, resumeID string) (Session, error)
    idleTTL time.Duration   // default 30min
    // ...
}

type pooledSession struct {
    session   Session
    contextID string
    sessionID string        // 缓存，用于 EVICTED 后 resume
    lastUsed  time.Time
    state     poolState     // ACTIVE / EVICTED
}

func (p *SessionPool) LookupOrCreate(ctx context.Context, contextID string) (Session, error)
```

### 行为

- **首次访问 contextID**：factory 新建 Session（无 resume）；记 sessionID
- **再次访问 ACTIVE entry**：返回现有 Session；`lastUsed = now()`（**sliding TTL refresh**）
- **再次访问 EVICTED entry**：factory 新建 Session，传 `resumeID = entry.sessionID`；状态转 ACTIVE
- **后台 reaper**：每 1min tick 扫描 ACTIVE entry，`now - lastUsed > idleTTL` 的 kill 掉，转 EVICTED，**保留 sessionID**

### 持久化

V1：内存。ahsir-agent 重启 → pool 清空 → 同 contextID 下次访问视作新会话。

V2（未来工单）：序列化 `contextID → sessionID` 映射到磁盘（可复用 TaskStore 文件或独立 JSON）。

## 7. Migration Plan

### Step 1 PR：抽接口，行为不变

- 新增 `Session` 接口、`OneshotSession` 实现（包装当前 `SessionManager.Send`）
- `Executor.SendPrompt` 闭包改为 `Session.Turn`
- `formatPriorHistory` / `LookupHistory` **保留**（仍然 prompt-stuffing 历史）
- 测试：新接口契约测试 + 所有现有 wrapper/scheduler/registry 测试通过
- **行为完全等价**，commit message 标 refactor

### Step 2 PR：stream-json 长驻 + Pool

- 新增 `ClaudeSession`（stream-json 实现）+ `SessionPool` + reaper
- `Executor` 改：拿 contextID 走 `pool.LookupOrCreate`
- **删除** `formatPriorHistory` + `LookupHistory` 调用链（claude 进程自己管历史）
- agent-card.yaml `runtime.args` 不再需要手写 `-p --output-format text`；改由 wrapper 内部强制加 stream-json flag
- 测试：ClaudeSession 协议解析单测、SessionPool TTL/resume 单测、端到端 e2e（需要真实 claude 二进制，build-tag 隔离）

### 兼容性

- `OneshotSession` 在 Step 2 后保留为 fallback / 测试桩，不删除
- agent-card 配置可选字段 `runtime.mode: oneshot|stream`（默认 stream）→ 留个回退开关

## 8. Open Questions

Resolution status as of 2026-06-05:

1. **`--include-partial-messages`** — **Deferred (still open)**. Confirmed the choice: integrate when wiring A2A SSE streaming. No work this iteration.
2. **进程异常恢复策略** — **Resolved: no auto-retry**. Mid-turn crash fails the current turn + EVICTED; next request triggers transparent recreate-with-resume via `SessionPool` (see `IsHealthy()` on the Session interface, added 2026-06-05). Caller still owns the retry decision per turn.
3. **资源上限** — **Deferred (still open)**. Pool has no max capacity. Adding LRU is the planned mitigation if process count becomes a real problem; not done yet.
4. **EVICTED entry GC** — **Resolved ✅**. 24h secondary TTL implemented in `SessionPool.reapOnce` (`evictedTTL` field). Persistence layer (`<workspace>/.a2a/sessions.json`) also respects this — the file is rewritten on GC.
5. **`--resume` 在第三方 gateway 下是否生效** — **Resolved ✅ for DeepSeek**. End-to-end verified on 2026-06-04: a cross-restart resume against `api.deepseek.com/anthropic` correctly recalls prior conversation state. Zhipu was the original target but the project moved to DeepSeek; if zhipu support is needed again, re-validate. The mechanism is provider-agnostic in principle (claude's local `~/.claude/projects/` holds the history), so other Anthropic-compatible providers should also work.

### Beyond the original §8 (capabilities delivered after spec freeze)

- **Persistence** (originally a "V2-future" item per the Step 2 plan): `FilePersistence` writes `contextID → sessionID` to `<workspace>/.a2a/sessions.json`, atomic tmp+rename. Cross-restart resume works.
- **Self-healing on SIGKILL** (not in original spec): pool probes `Session.IsHealthy()` on the hot path. When a `claude` subprocess is externally killed, the next request transparently recreates with `--resume=<prior sessionID>`.
- **contextID propagation through A2A_CALL**: when student delegates to teacher, the student's `task.ContextID` flows to teacher so the callee's pool reuses a session across multiple delegations within one conversation.
- **Inter-agent logging**: `[X] receive`, `[X → Y] A2A_CALL`, `[X ← Y] reply` log markers for cross-agent traffic.

## 9. Test Plan (TDD outline)

### Step 1

- `TestOneshotSession_TurnHappyPath`：mock SendPrompt → 验证 `Turn` 返回文本
- `TestOneshotSession_TurnError`：mock SendPrompt err → 验证 `Turn` 返回 err
- `TestOneshotSession_Stream_EmitsAndCloses`：验证 Stream channel 行为
- Executor 已有测试全部通过（行为不变）

### Step 2

- `TestClaudeSession_ParseInitEvent`：协议解析，验证 sessionID 提取
- `TestClaudeSession_DropsHookNoise`：`hook_started/response` 不进 channel
- `TestClaudeSession_StreamMultiTurn`：用 fake stdin/stdout pipe 喂入两轮 NDJSON，验证 `EventText` / `EventTurnDone` 顺序
- `TestClaudeSession_MidTurnCrash`：模拟 process EOF，验证 `EventTurnDone{Err}` + 状态转 EVICTED
- `TestSessionPool_LookupOrCreate_New`
- `TestSessionPool_LookupOrCreate_Reuse_RefreshesTTL`
- `TestSessionPool_IdleEviction`：fake clock 推进 > TTL，验证 evict
- `TestSessionPool_ResumeAfterEvict`：evict 后 `LookupOrCreate` 验证 factory 收到 resumeID
- E2E（build tag `e2e`）：真起 claude，两轮对话验证上下文连续
