# AHSIR Session Abstraction — Step 1: Interface Extraction (Behavior-Preserving Refactor)

**Status:** Draft
**Date:** 2026-05-12
**Version:** 0.1.0
**Spec:** [`docs/superpowers/specs/2026-05-12-session-abstraction.md`](../specs/2026-05-12-session-abstraction.md)

## 1. Scope

引入 `Session` 接口 + `Event` 类型 + `OneshotSession` 实现，把 `Executor` 从直接持有 `SendPrompt` 闭包改成持有 `OpenSession` 工厂。**行为完全等价**——每次 `Turn` 仍然 fork 一个新的 `claude -p` 进程，`formatPriorHistory` 仍然在 Executor 里跑。

目的：让 Step 2 只需要换一个 `Session` 实现（`ClaudeSession` + `SessionPool`）就能切换到长驻模式，不动 Executor 主流程。

## 2. Out of Scope（留给 Step 2）

- `ClaudeSession` 的 stream-json 实现
- `SessionPool` / idle TTL / `--resume`
- 删除 `formatPriorHistory` / `LookupHistory`
- agent-card.yaml 配置项调整

## 3. 文件改动清单

| 文件 | 改动类型 | 内容 |
|---|---|---|
| `internal/wrapper/session_types.go` | **新建** | `Event` interface + `EventText` / `EventToolUse` / `EventTurnDone` + `TurnStats` + `Session` interface |
| `internal/wrapper/session_oneshot.go` | **新建** | `OneshotSession` 实现，内部组合 `SessionManager`，`Turn` 委托给 `SessionManager.Send` |
| `internal/wrapper/session_oneshot_test.go` | **新建** | OneshotSession 契约测试 |
| `internal/wrapper/session.go` | 不动 | `SessionManager` 保留原样 |
| `internal/wrapper/executor.go` | 改 | `ExecutorConfig.SendPrompt` → `OpenSession func(contextID string) (Session, error)`；`interact` 内部调 `session.Turn` |
| `internal/wrapper/executor_test.go` | 改 | 测试用 fake `OpenSession` 替代 fake `SendPrompt` |
| `cmd/ahsir-agent/main.go` | 改 | 构造 `OpenSession` 闭包：拿 `SessionConfig` 构造 `OneshotSession` 返回 |
| 其他 *_test.go | 检查并修 | 编译错误 / 行为快照不变 |

## 4. TDD 步骤

### 4.1 Step 1a — Session 接口 + Event 类型 + OneshotSession

**Red 1**：写 `session_oneshot_test.go`，对一个尚未存在的 `OneshotSession` 写四条契约测试

| 测试 | 断言 |
|---|---|
| `TestOneshotSession_Turn_HappyPath` | mock `Send` 返回 `"hello"`，`Turn(ctx, "ping")` 返回 `"hello", nil` |
| `TestOneshotSession_Turn_Error` | mock `Send` 返回 `err`，`Turn` 返回 `"", err`（保留 partial output） |
| `TestOneshotSession_Stream_EmitsAndCloses` | `Stream` channel 依次收到 `EventText{"hello"}` → `EventTurnDone{Err:nil}` → channel close |
| `TestOneshotSession_Stream_PropagatesError` | mock err，channel 收到 `EventTurnDone{Err:err}` → close（无 `EventText`） |
| `TestOneshotSession_SessionID_Empty` | `SessionID()` 返回 `""`（oneshot 没有持久 sessionID 概念） |
| `TestOneshotSession_Close_Idempotent` | `Close()` 调两次不 panic / 不返错 |

为了让 OneshotSession 可测，引入 internal seam：构造函数接收一个 `sender func(ctx, prompt) (string, error)`，**生产代码**里用 `SessionManager.Send`，测试用 mock function。

**Green 1**：实现 `session_types.go` 和 `session_oneshot.go`：

```go
// session_types.go
type Event interface{ isEvent() }
type EventText struct{ Text string }
type EventToolUse struct{ Name string; Input json.RawMessage }
type EventTurnDone struct{ Err error; Stats TurnStats }
type TurnStats struct{ /* ... */ }

func (EventText) isEvent()     {}
func (EventToolUse) isEvent()  {}
func (EventTurnDone) isEvent() {}

type Session interface {
    Stream(ctx context.Context, userText string) (<-chan Event, error)
    Turn(ctx context.Context, userText string) (string, error)
    SessionID() string
    Close() error
}

// session_oneshot.go
type OneshotSession struct {
    sender func(ctx context.Context, prompt string) (string, error)
    mu     sync.Mutex
    closed bool
}

func NewOneshotSession(sm *SessionManager) *OneshotSession { ... }

func (s *OneshotSession) Stream(...) (<-chan Event, error) {
    // goroutine: call sender → push EventText (if non-empty) → push EventTurnDone → close
}
func (s *OneshotSession) Turn(...) (string, error) {
    // drain Stream, aggregate text, return
}
func (s *OneshotSession) SessionID() string { return "" }
func (s *OneshotSession) Close() error { ... }
```

**验证**：`go test ./internal/wrapper/... -run TestOneshotSession` 全绿。

### 4.2 Step 1b — Executor 切到 Session 抽象

**Red 2**：修改 `executor_test.go` 里所有 fake `SendPrompt` 闭包 → fake `OpenSession`，期望 `Executor.Execute` 行为不变（已有断言都成立）。预期一开始编译失败，因为 `ExecutorConfig` 还是老字段。

**Green 2**：改 `ExecutorConfig`：

```go
type ExecutorConfig struct {
    OpenSession   func(ctx context.Context, contextID string) (Session, error)
    ListAgents    func() []*a2a.AgentCard
    CallAgent     func(ctx context.Context, agentName string, task string) (string, error)
    LookupHistory func(contextID string) []*a2a.Task
    MaxDepth      int
    BasePrompt    string
}
```

`Executor.Execute` 内部：

1. 调用 `e.openSession(ctx, task.ContextID)` 拿 session
2. `defer session.Close()`（Step 1 行为：用完即关）
3. `interact` 里把原本的 `e.sendPrompt(ctx, prompt)` 改成 `session.Turn(ctx, prompt)`
4. `formatPriorHistory` 仍然跑，结果仍然拼到 prompt 前

**为什么 `defer session.Close()`**：保持 Step 1 行为等价（每次 Execute 都是孤立的）。Step 2 把这层 `defer Close` 拿掉，Session 由 Pool 管生命周期。

### 4.3 Step 1c — 调用方接线

**Green 3**：改 `cmd/ahsir-agent/main.go`：

```go
exec := wrapper.NewExecutor(wrapper.ExecutorConfig{
    OpenSession: func(ctx context.Context, contextID string) (wrapper.Session, error) {
        sm := wrapper.NewSessionManager(sessionCfg)
        if err := sm.Start(ctx); err != nil {
            return nil, err
        }
        return wrapper.NewOneshotSession(sm), nil
    },
    // ... 其他字段不变
})
```

`contextID` 参数 Step 1 不用，Step 2 才用上。

### 4.4 验证

- `go test ./...` 全绿（含 wrapper / scheduler / registry / mcp 各包）
- `go build ./...` 通过
- 手动跑 example workspace（teacher / student）→ 行为与改造前一致

## 5. Definition of Done

- [ ] `Session` interface + `Event` 类型定义完成
- [ ] `OneshotSession` 通过 6 条契约测试
- [ ] `Executor` 切换到 `OpenSession` 工厂，所有现有测试不改断言只改 wiring 后通过
- [ ] `cmd/ahsir-agent/main.go` 接线完成
- [ ] `go test ./...` 全绿
- [ ] commit message：`refactor(wrapper): extract Session interface (oneshot impl)`

## 6. 风险 & Mitigation

| 风险 | 影响 | 应对 |
|---|---|---|
| Session 接口签名后续 Step 2 还要改 | Step 2 改接口又得动 Executor | 接口已按 spec 充分设计；如真要改，作为 Step 2 一部分顺手改 |
| `OneshotSession.Close()` 语义弱（什么都不做） | 文档不清晰会迷惑读者 | Doc comment 明确："oneshot 没有需要释放的资源；Close 只为接口实现而存在" |
| Executor `defer session.Close()` 引入性能开销 | 实际 Close 是 no-op，无影响 | 不处理 |
| 测试用 `time.Sleep` 等 channel 关闭 | flaky | 用 `for ev := range ch` 阻塞读到 close 为信号，不 sleep |

## 7. PR 边界

单 PR，单 commit（避免 step 1a/1b/1c 在 main 上中间态可见）。预估 diff 规模：

- 新增 ~150 行（types + oneshot + tests）
- 改 ~50 行（executor + main + 现有 tests 的 wiring）
- 删 0 行

## 8. Step 2 预告（不在本 PR 范围）

Step 2 PR 在本 PR 合入后开新分支：

- 新文件：`internal/wrapper/session_claude.go`（stream-json 实现）+ `session_pool.go`（LRU + TTL）
- 改 `Executor`：去掉 `defer session.Close()`；session 生命周期由 pool 管
- 改 `formatPriorHistory` → 删
- 改 `cmd/ahsir-agent/main.go`：注入 `SessionPool` 作为 `OpenSession` 工厂
- 默认走 stream，配置回退 oneshot 模式
