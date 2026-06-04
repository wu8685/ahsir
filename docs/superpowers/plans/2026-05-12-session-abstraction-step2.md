# AHSIR Session Abstraction — Step 2: Long-Running ClaudeSession + Pool

**Status:** Draft
**Date:** 2026-05-12
**Version:** 0.1.0
**Spec:** [`docs/superpowers/specs/2026-05-12-session-abstraction.md`](../specs/2026-05-12-session-abstraction.md)
**Prerequisites:** Step 1 ([plan](./2026-05-12-session-abstraction-step1.md)) merged

## 1. Scope

实现 `ClaudeSession`（单进程长驻 + stream-json）和 `SessionPool`（按 contextID 池化 + sliding TTL + EVICTED → resume），切换 `Executor` 从 OneshotSession 到 Pool。删除 `formatPriorHistory` / `LookupHistory` 调用链——历史改由 claude 进程自己持有。

Spec §8 open questions 处置：
- #1 partial-messages：本期不打开
- #2 mid-turn crash：**不**自动 retry，fail 当前 turn + EVICTED 等下次 resume
- #3 pool max cap：不加 LRU，本期不限容量
- #4 EVICTED 二级 TTL：**24h** GC 彻底删除（spec §8 默认采纳）
- #5 zhipu `--resume`：留到端到端实测验证，代码先按"应该可用"实现

## 2. Out of Scope

- A2A SSE 流式回吐客户端
- ahsir-agent 跨重启的 contextID→sessionID 持久化（V2 工单）
- `--include-partial-messages` 增量 delta
- 多会话进程内多路复用
- pool 容量 LRU

## 3. 文件改动清单

| 文件 | 改动类型 | 内容 |
|---|---|---|
| `internal/wrapper/session_claude.go` | **新建** | `ClaudeSession` 实现：进程生命周期 + stream-json 协议解析 |
| `internal/wrapper/session_claude_test.go` | **新建** | 协议解析 + 状态机单测（用 io.Pipe 注入，不依赖真 claude） |
| `internal/wrapper/session_pool.go` | **新建** | `SessionPool` + reaper goroutine + 24h EVICTED GC |
| `internal/wrapper/session_pool_test.go` | **新建** | Pool 行为单测（fake clock） |
| `internal/wrapper/session_claude_e2e_test.go` | **新建** | build tag `e2e`，真起 claude，验证两轮上下文连续 |
| `internal/wrapper/executor.go` | 改 | 删 `LookupHistory` / `formatPriorHistory` 调用；删 `defer session.Close()`（Pool 管生命周期） |
| `internal/wrapper/executor_test.go` | 改 | 删 `TestExecutorInjectsPriorHistory` / `TestExecutorNoHistoryWhenLookupNil`（行为已转移） |
| `internal/wrapper/wrapper.go` | 改 | `SetupExecutor` 签名简化：去掉 `LookupHistory` 来源 |
| `internal/wrapper/wrapper_test.go` | 改 | `TestAgentWrapperContextMemoryAcrossRequests` 改为验证"同 contextID 复用同一 Session"而非 prompt 拼接 |
| `cmd/ahsir-agent/main.go` | 改 | 构造 SessionPool 替代 per-request OneshotSession 工厂；启停 reaper |
| 示例 agent-card.yaml | 改 | `runtime.args` 不再手写 `-p --output-format text`；wrapper 内部加 stream-json flag |
| `internal/wrapper/runtime.go` | 可能要改 | 暴露 stream-json args 拼装函数（参考 Step 1 `buildSessionConfig`） |

## 4. 关键设计抉择

### 4.1 ClaudeSession 内部 seam

为了协议解析能在不依赖真 claude 的情况下单测，构造函数接受一个 `transportFactory`：

```go
type claudeTransport struct {
    stdin  io.WriteCloser
    stdout io.Reader
    wait   func() error          // 阻塞直到进程退出，返回退出错误
    kill   func() error
}

type transportFactory func(ctx context.Context, resumeID string) (*claudeTransport, error)

// 生产工厂：exec.CommandContext claude with stream-json args
// 测试工厂：返回 io.Pipe 对 + 可控的 wait/kill stub
```

`NewClaudeSession(ctx, cfg, factory)` 暴露给生产代码；测试用 `newClaudeSessionWithTransport(transport)` 直接注入 pipe，跳过 exec。

### 4.2 状态机实现

```go
type sessionState int
const (
    stateNew sessionState = iota
    stateReady
    stateInFlight
    stateEvicted
    stateClosed
)

type ClaudeSession struct {
    sessionID  string
    state      sessionState
    transport  *claudeTransport
    factory    transportFactory
    cfg        SessionConfig
    mu         sync.Mutex
    eventsCh   chan Event           // 当前 turn 的输出通道（IN_FLIGHT 期间非 nil）
    readerDone chan struct{}        // reader goroutine 退出信号
}
```

reader goroutine 唯一职责：scan 行 → JSON decode → 分发到 `eventsCh` → `result` 事件后 close `eventsCh` 并把状态从 IN_FLIGHT 转回 READY。

### 4.3 Pool sliding TTL & 二级 GC

```go
type pooledEntry struct {
    contextID    string
    sessionID    string         // 缓存以便 EVICTED 后 resume
    session      *ClaudeSession // EVICTED 后置 nil
    state        poolEntryState // ACTIVE / EVICTED
    lastUsed     time.Time      // 每次 Stream 完成时刷新
    evictedAt    time.Time      // 转 EVICTED 时记录，用于 24h GC
}

type SessionPool struct {
    factory      func(ctx context.Context, contextID, resumeID string) (*ClaudeSession, error)
    idleTTL      time.Duration // default 30min
    evictedTTL   time.Duration // default 24h
    now          func() time.Time // 测试可注入 fake clock
    entries      map[string]*pooledEntry
    mu           sync.Mutex
    reaperDone   chan struct{}
}
```

reaper 1min tick：

1. 扫 ACTIVE entry，`now - lastUsed > idleTTL` → kill session, 转 EVICTED, 记 evictedAt
2. 扫 EVICTED entry，`now - evictedAt > evictedTTL` → 从 map 删除

### 4.4 stream-json args 强制注入

无论 agent-card.yaml 怎么写，wrapper 内部在 `buildSessionConfig` 出口处统一追加：

```
--input-format stream-json --output-format stream-json --verbose
```

如果 `runtime.args` 里已经有 `-p` 就保留；没有就加上。`--resume <sessionID>` 在 ClaudeSession 重启路径单独追加。

**不保留 oneshot fallback**：`main.go` 始终走 Pool/ClaudeSession 路径；万一出问题用 `git checkout` 回到 Step 1 commit。`OneshotSession` 代码保留在 tree 里，但角色降级为"Session 接口的轻量 impl / 测试 fake"，不再被生产代码 wiring 调用。

## 5. TDD 步骤

### Step 2a — ClaudeSession 协议解析（不起进程）

**Red 1**：`session_claude_test.go` 用 `io.Pipe` 构造 fake transport，逐条 NDJSON 喂进 stdout pipe，验证 Stream channel 输出。

| 测试 | NDJSON 输入 | 期望输出 |
|---|---|---|
| `Test_DropsHookNoise` | hook_started → hook_response → init → result | 仅 EventTurnDone（不含 EventText） |
| `Test_ParsesInitSessionID` | init(session_id=X) → result | session.SessionID() == X |
| `Test_AssistantTextEmitsEvent` | init → assistant(content=[{type:text,text:"hi"}]) → result | EventText("hi") → EventTurnDone(nil) |
| `Test_AssistantToolUseEmitsEvent` | init → assistant(content=[{type:tool_use,name:"Bash",input:{...}}]) → result | EventToolUse → EventTurnDone |
| `Test_AssistantMixedTextAndToolUse` | init → assistant(content=[text, tool_use, text]) → result | 3 个 event 顺序正确 |
| `Test_ResultIsErrorPropagated` | init → result(is_error=true, result="boom") | EventTurnDone(Err 含 "boom") |
| `Test_MalformedJSONIgnored` | 几行垃圾 → init → result | 不 crash，正常完成 turn |

**Green 1**：实现 `session_claude.go` 的 NDJSON 解析 + state machine 单 turn 路径。先 hardcode 一个 turn 就退出（不支持多轮）。

**Red 2**：多轮测试

| 测试 | 场景 |
|---|---|
| `Test_StreamSerial_TwoTurns` | 调 Stream(t1) 喂 result，drain；再 Stream(t2) 喂 result，drain；验证 stdin 收到两条 user message |
| `Test_StreamRejectsConcurrent` | Stream(t1) 未 drain 又 Stream(t2) → error |
| `Test_StdinIncludesSessionID` | 解析写入 stdin 的 NDJSON，验证 `session_id` 字段是 init 返回的值 |

**Green 2**：扩展 state machine，IN_FLIGHT → READY 转换，stdin 写入逻辑。

### Step 2b — 进程崩溃 & resume

**Red 3**：

| 测试 | 场景 |
|---|---|
| `Test_MidTurnEOF_FailsTurnAndEvicts` | Stream 中途 stdout pipe 关闭 → EventTurnDone(Err: io.EOF or similar) → 状态 EVICTED |
| `Test_InitTimeout` | transport 起来但首个 init 事件迟到超过 init timeout → NewClaudeSession 返回 error |
| `Test_ResumeUsesSessionID` | 用 fake factory，第二次构造时 resumeID 非空 → 验证 factory 收到正确 sessionID |

**Green 3**：实现 EOF 处理、init timeout、Stream() 在 EVICTED 时拒绝（让 Pool 来重建）。

### Step 2c — SessionPool

**Red 4**：

| 测试 | 场景 |
|---|---|
| `Test_LookupOrCreate_New` | 首次访问 → factory 调用 resumeID="" → 返回 session |
| `Test_LookupOrCreate_ReuseRefreshesTTL` | 二次访问同 contextID → 同一 session 返回 → lastUsed 已刷新 |
| `Test_IdleEviction` | fake clock 推进 > idleTTL → reaper 转 EVICTED → session.Close 被调 |
| `Test_ResumeAfterEvict` | EVICTED 后 LookupOrCreate → factory 调用 resumeID=<原sessionID> |
| `Test_EvictedSecondaryGC` | EVICTED 后 fake clock 推进 > evictedTTL → entry 从 map 删除 → 再次 LookupOrCreate 视为新会话 |
| `Test_ConcurrentLookupSameContextID` | 100 goroutine 同时 LookupOrCreate(ctx-1) → factory 只调用一次 |

**Green 4**：实现 Pool + reaper。注意并发：`LookupOrCreate` 必须串行化 per-contextID（用 entry-level mutex 或 sync.Once 模式）。

### Step 2d — Executor 切换 & 删历史拼接

**Red 5**：改 `executor_test.go`

- 删 `TestExecutorInjectsPriorHistory`（行为已转移到 claude 进程内部，wrapper 不再 prompt-stuffing）
- 删 `TestExecutorNoHistoryWhenLookupNil`（LookupHistory 字段消失）
- 改 `TestAgentWrapperContextMemoryAcrossRequests`：断言"两次 message/send 到同 contextID 都拿到同一 Session 实例"

**Green 5**：

- `ExecutorConfig` 去掉 `LookupHistory`
- `Executor.Execute` 去掉 `formatPriorHistory` 调用 + `defer session.Close()`
- `executor.go` 同时删除 `formatPriorHistory` 函数

### Step 2e — main.go 接线

**Green 6**：`cmd/ahsir-agent/main.go`

```go
poolFactory := func(ctx context.Context, contextID, resumeID string) (*wrapper.ClaudeSession, error) {
    return wrapper.NewClaudeSession(ctx, sessionCfg, wrapper.ProductionClaudeTransport(resumeID))
}
pool := wrapper.NewSessionPool(poolFactory, 30*time.Minute, 24*time.Hour)
defer pool.Stop()

openSession := pool.LookupOrCreate
w.SetupExecutor(openSession, listAgents, callAgent, maxCalls, basePrompt)
```

**移除** Step 1 的 `wrapper.NewSessionManager` + `OneshotSession` 启动路径——生产代码不再 fork-per-request。`session.Start()` / `defer session.Stop()` 也跟着删掉，因为长驻进程的生命周期归 Pool 管。

### Step 2f — E2E

**E2E**：`session_claude_e2e_test.go`，build tag `e2e`

- 跳过条件：`claude` 不在 PATH，或 `AHSIR_E2E_CLAUDE=1` 未设
- 流程：起 ClaudeSession → Turn("我叫昊天") → Turn("我叫什么") → 验证回答里含"昊天"
- 这步顺带验证 §8 #5（zhipu `--resume`）的实际可用性——视实测情况补充 plan

## 6. Definition of Done

- [ ] ClaudeSession 通过所有协议 + 状态机 + EOF + resume 单测
- [ ] SessionPool 通过所有 TTL + resume + 并发 + GC 单测
- [ ] Executor 切到 Pool；`formatPriorHistory` / `LookupHistory` 删除
- [ ] `cmd/ahsir-agent/main.go` 接线 Pool，加 reaper 启停
- [ ] 示例 agent-card.yaml 更新：`runtime.args` 可以删空，由 wrapper 内部强制注入 stream-json flags
- [ ] `cmd/ahsir-agent/main.go` 删除老的 SessionManager Start/Stop wiring
- [ ] `go test ./...` 全绿（不含 e2e tag）
- [ ] e2e tag 测试在本地手动跑通一次，结果贴到 PR 描述
- [ ] `go build ./...` 通过
- [ ] commit message：`feat(wrapper): long-running stream-json claude session + pool`

## 7. 风险 & Mitigation

| 风险 | 影响 | 应对 |
|---|---|---|
| stream-json schema 与 SDK 反推有偏差 | 解析失败 | 解析器写得宽松：unknown event type drop，unknown field 忽略；首次接入加详细 log |
| reader goroutine 泄漏 | 每个 EVICTED session 残留 goroutine | Close() 等 readerDone channel；Pool 反复 evict 不出 goroutine 泄漏靠 stress 测试 |
| Pool 并发 race（同 contextID 同时建） | 起两个 claude 进程，session_id 不一致 | 用 entry-level lock 或 `singleflight` 模式串行化 LookupOrCreate |
| zhipu gateway `--resume` 不工作 | EVICTED 后 resume 失败，"忘记"之前对话 | E2E 实测前不投产；如果失败，spec §8 #5 升级为 known limitation，并临时缩短 idleTTL 让 EVICTED 罕见到不影响体验，直到协议层修好 |
| 真 claude 启动慢 (~1-3s) | 首次访问 contextID 延迟陡增 | 接受现状；未来可以加 warmup pool（不在本期） |
| `formatPriorHistory` 删除后某场景行为退化 | 跨 ahsir-agent 重启的会话"失忆" | 已在 Non-goals 标记；V2 工单解决持久化 |
| Pool 内存增长（每个 contextID 一个进程） | 100+ 活跃会话 → 100+ claude 进程 | idle TTL = 30min 兜底；如成问题再加 LRU |

## 8. PR 边界

单 PR，但**可拆 commit**：

1. `feat(wrapper): claude stream-json session (single-turn)` — Step 2a
2. `feat(wrapper): claude session multi-turn + crash recovery` — Step 2b
3. `feat(wrapper): session pool with idle TTL and resume` — Step 2c
4. `refactor(wrapper): executor uses session pool, drop prompt-stuffing` — Step 2d + 2e
5. `test(wrapper): claude session e2e` — Step 2f

预估 diff 规模：

- 新增 ~700-900 行（session_claude + session_pool + 测试 + e2e）
- 改 ~100 行（executor / wrapper / main）
- 删 ~80 行（formatPriorHistory + 相关测试）

## 9. 跨期遗留

完成 Step 2 后，独立工单清单：

- **V2-持久化**：contextID → sessionID 落盘，跨 ahsir-agent 重启可续接
- **V2-SSE 流式**：打开 `--include-partial-messages`，把 EventText 增量推给 A2A 客户端
- **V2-资源控制**：pool LRU 容量上限 + 超载时拒绝/排队策略
- **V2-A2A_CALL → tool_call**：从文本标记换成 MCP tool 调用协议
- **V2-多 provider**：基于 Session 接口加 `CodexSession` / `GeminiSession` 实现
