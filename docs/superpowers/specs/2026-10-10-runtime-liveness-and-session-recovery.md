# Runtime 存活检测与 Session 恢复规格

**日期：** 2026-10-10

**状态：** Implemented — 2026-10-11

**关联 Issue：** [wu8685/ahsir#43](https://github.com/wu8685/ahsir/issues/43)

## 1. 问题陈述

Hetairoi 触发的多个 GitHub issue 已经进入 ahsir，但长期停在 CMA `running`：

- `wu8685/me#18`、`#19`、`#20`；
- `wu8685/ahsir#36`、`#43`。

这些 Session 的事件流都停在中间 tool event，没有 `agent.message`、`session.error` 或
`session.status_terminated`；对应 provider 子进程已经不存在，CMA Session 却仍保持
`running`。因此上游只能一直等待，GitHub outcome adapter 既不能报告失败，也不能为后续
人工重试提供明确入口。

当前实现存在两个独立缺口：

1. `ClaudeSession` 主要依赖 stdout EOF 识别 runtime 退出。虽然 transport 暴露了
   `wait()`，Session 并未把 process completion 作为状态机输入；进程、pipe、reader 任一环节
   未正常收敛时，turn 可能永不产生 terminal event。
2. CMA store 会持久化 `running`，但启动时没有 execution lease/reconciliation。进程重启后，
   内存中的 turn goroutine 和 A2A task 已不存在，磁盘状态仍可能永久声称任务正在执行。

线上 CMA turn timeout 默认为 720 小时。这个值允许长任务运行，但不能替代 runtime liveness
检测；`runtime_timeout: 0` 也只表示不设置工作时限，不能表示忽略进程死亡。

## 2. 目标

1. provider 根进程退出、stdout 异常结束、context 取消和显式 Close 都能在有界时间内收敛。
2. 每个 turn 恰好产生一个 terminal outcome，不重复关闭 channel，不发生 send-on-closed。
3. runtime 意外退出时保留已产生的文本、tool event 和 provider resume identity。
4. CMA 在 ahsir 重启后将失去 execution owner 的 `running` Session 确定性地结算为失败。
5. 上游得到持久化的 typed failure，Hetairoi 可据此执行 GitHub source 自己的失败通知策略。
6. 无限 work timeout 与存活检测正交：活着的任务可无限运行，死亡的任务不能无限 `running`。
7. 不自动重放可能已经产生外部副作用的业务 turn；重试必须创建一个明确的新 attempt。

## 3. 非目标

- 本规格不实现 GitHub issue 评论；它只保证 ahsir 可靠产出 terminal truth。评论属于 Hetairoi
  GitHub Event Source/outcome adapter。
- 不在 ahsir 中引入 GitHub、DataHub 或其他业务 source 协议。
- 不以固定短 timeout 代替 liveness detection。
- 不宣称 provider 已完成语义交付；没有 durable result 就必须报告失败。
- 不自动重放原始 prompt，以免重复 push、评论、建 PR 或执行其他不可逆操作。
- #43 中完整的 initialize/finalize lifecycle 仍可后续扩展；本规格先修复其依赖的 terminal
  safety 与 restart recovery 基线。

## 4. 核心不变量

### 4.1 Turn terminal safety

一个已接受 turn 必须且只能以以下一种结果结束：

- `completed`：收到并持久化 canonical provider result；
- `canceled`：调用方明确取消；
- `timed_out`：配置的工作时限到期；
- `runtime_exited`：provider 进程在 canonical result 前退出；
- `runtime_io_failed`：协议 IO/reader 失败；
- `runtime_interrupted`：ahsir 重启导致原 execution owner 丢失。

terminal arbitration 必须是原子的。正常 result、process wait、stdout EOF、timeout 和 cancel
之间无论怎样竞态，只有一个路径获得终结权。

### 4.2 Session health

- canonical result 后，runtime 仍健康时 Session 回到 `ready`；
- 非正常 runtime terminal 后 Session 进入 `evicted`，保留已知 `session_id`，后续新 turn 可通过
  pool 的 `--resume` 路径创建新进程；
- `closed` 永远不可恢复；
- `IsHealthy()` 不得在根进程已退出后继续返回 true。

### 4.3 Persisted running state

`running` 表示“当前进程持有一个活的 execution owner”，不能表示历史意图。CMA state 文件不
持久化可跨进程恢复的 goroutine/A2A task，因此新进程加载到旧 `running` 时必须将其结算为
`runtime_interrupted`，而不是继续展示 `running` 或伪造 `idle`。

## 5. Runtime supervisor 设计

### 5.1 双信号监督

`ClaudeSession` 同时监督：

1. protocol reader：解析 stdout NDJSON 和 canonical `result`；
2. process waiter：消费 `cmd.Wait()` 的唯一结果。

process waiter 不再只是防 zombie 的后台 goroutine；它把 exit status 送入 Session 状态机。
若进程在 canonical result 前退出，Session 必须触发 `runtime_exited`。若 reader 先发现 EOF，
EOF 和 wait 结果共同用于错误诊断，但仍只结算一次。

### 5.2 收敛顺序

为避免 process exit 与 stdout 尾部缓冲竞态：

1. `wait()` 返回后记录 exit result；
2. 给 protocol reader 一个很短且有上限的 drain grace，让已经写入 pipe 的 canonical result
   有机会被解析；
3. grace 内读到 canonical result，则正常完成；
4. grace 到期仍无 result，则关闭 transport output/终止残留 process group，使 reader 解阻塞；
5. terminal arbiter 以 `runtime_exited` 结算。

transport 应显式提供 process-done 与 output-close 能力；不得依赖某个 OS/CLI 一定按预期关闭
所有继承的 FD。drain grace 是 IO 收尾时限，不是业务 turn timeout。

### 5.3 单一 terminal arbiter

Session 增加 turn-scoped terminal arbiter，负责：

- 校验事件仍属于当前 turn；
- 原子夺取 terminal ownership；
- 从 Session 脱离当前 event channel；
- 在 mutex 外发送一个 `EventTurnDone` 并关闭 channel；
- 按 terminal kind 设置 `ready` 或 `evicted`。

所有终结路径统一调用该 arbiter。reader 发送普通事件时也必须检查 turn identity，确保 supervisor
结算后不会向已关闭 channel 写入。

### 5.4 错误类型

wrapper 增加可由 `errors.Is/As` 识别的 runtime terminal error，至少包含：

- kind；
- provider/process exit 摘要；
- 是否存在 canonical result；
- 已知 session/resume identity。

错误文本必须有界，不包含环境变量、token、完整 stderr 或无上限 provider output。

## 6. CMA restart reconciliation

### 6.0 A2A terminal 完整性

`message/stream` 的 HTTP/SSE body 正常 EOF 不等于 Agent turn 成功。只有收到 A2A terminal
`task` frame，`ChatStream` 才能返回成功、失败或取消；若连接在 terminal frame 前结束，必须
保留 partial text 并返回 truncated-stream error。CMA 随后沿 `terminateTurn` 持久化失败，禁止
生成空 `agent.message` 后回到 `idle`。

### 6.1 启动恢复

CMA facade 开始监听前执行一次 store reconciliation：

1. 枚举未 archive 且状态为 `running` 的 Session；
2. 为每个 Session 追加一个 `session.error`，错误 kind 为 `runtime_interrupted`，
   `retry_status.type` 为 terminal；
3. 追加 `session.status_terminated`；
4. 更新 Session status/updated_at；
5. 将整个 reconciliation 结果持久化后才开放 HTTP listener。

该操作必须幂等：只有 `running` 能被结算；已 `terminated` 的 Session 再次启动不得重复追加事件。
若持久化失败，facade 启动失败，不能对外暴露一半恢复的状态。

### 6.2 活进程内的 runtime failure

wrapper 的 `runtime_exited` 通过 executor → A2A failed task → CMA `ChatStream` 返回 error，随后沿
现有 `terminateTurn` 路径持久化 `session.error` 与 `session.status_terminated`。必须保留失败前
已经持久化的 tool/thinking/span 事件。

### 6.3 Retry 语义

- ahsir 不自动 replay 原 turn；
- Hetairoi 收到 terminal failure 后，由 source-specific outcome adapter 在 GitHub issue 留下失败
  说明与可重试提示；
- 操作者或明确的 retry policy 触发新 attempt；新 attempt 可以复用业务 key，但必须有新的
  turn/attempt identity；
- retry 前不得把旧 Session 改写为成功或 idle。

## 7. Timeout 语义

| 机制 | `0` 的含义 | 负责解决 |
|---|---|---|
| Agent `runtime_timeout` | 不限制活的业务 turn 时长 | Agent 级工作时限 |
| CMA `TurnTimeout` | 使用部署默认值；后续应支持显式 infinite，而非魔法大值 | facade 请求上限 |
| process liveness | 不可关闭 | 根进程/协议死亡 |
| process drain grace | 必须为短且有界的正值 | 读取退出前尾部结果 |

本次修复不得通过把 720 小时简单改成几分钟来掩盖 bug。部署配置可另行选择合理的 facade
deadline，但即使 deadline 无限，runtime 死亡也必须立即收敛。

## 8. Observability

日志使用稳定字段记录：agent、context ID、provider session ID、root PID、turn ID、terminal kind、
exit status、drain grace 是否超时。不得打印 prompt、token 或完整 stderr。

至少提供以下可诊断事实：

- root process 何时退出；
- terminal ownership 被哪条路径取得；
- 是否保留了 provider session ID；
- startup reconciliation 结算了多少个遗留 `running` Session。

## 9. TDD 测试顺序

实现必须先写失败测试，再逐步写生产代码：

1. **process wait 基线**：stdout 不 EOF、`wait()` 已返回时，turn 在 drain grace 后以
   `runtime_exited` 结束。
2. **尾部 result 竞态**：`wait()` 先返回、grace 内 reader 收到 result，turn 只成功一次。
3. **EOF/wait 竞态**：两者任意顺序均只产生一个 `EventTurnDone`。
4. **cancel/timeout/wait 竞态**：高并发重复运行，无 panic、无 channel double-close，race test 通过。
5. **空闲退出**：无 in-flight turn 时进程退出，Session 变为 `evicted`，不产生伪造 turn event。
6. **resume identity**：异常退出后保留 session ID，下一 turn 走 `--resume`。
7. **infinite timeout**：`runtime_timeout: 0` 的活进程不会因计时器终止；进程死亡仍立即失败。
8. **A2A/CMA 传播**：runtime failure 产生 failed task，CMA 持久化 typed `session.error` 和
   terminated status。
9. **A2A 截断**：SSE clean EOF 但没有 terminal task 时仍判失败，并保留 partial text。
10. **startup reconciliation**：加载 persisted `running` 后追加一次 interrupted error + terminate；
   第二次启动不重复追加。
11. **恢复持久化失败**：facade 拒绝启动，不暴露不一致状态。
12. **现有回归**：正常多轮、显式 cancel、timeout、Close、pool eviction、Codex provider 行为不变。

测试至少运行：

```sh
GO111MODULE=on go test ./internal/wrapper ./internal/cmagateway/...
GO111MODULE=on go test -race ./internal/wrapper ./internal/cmagateway/...
GO111MODULE=on go test -short ./...
GO111MODULE=on go vet ./...
```

## 10. 发布与现存任务恢复

1. 完成上述测试后构建并 codesign 新的 `ahsir`、`ahsir-agent` 到临时路径。
2. 备份 `~/.cma-stack/cma-facade-state.json`，原子替换部署二进制。
3. 重启 LaunchAgent。startup reconciliation 应将当前 5 个 zombie Session 结算为
   `runtime_interrupted`，从而让 Hetairoi 观察到 terminal failure。
4. 验证 GitHub outcome adapter 对各 issue 发布失败说明；不得包含 token、路径中的私密信息或
   无界日志。
5. 通过 source 的原生事件协议重触发 ME `#18/#19/#20` 与 ahsir `#36/#43`，每个 issue 创建
   新 attempt，不复用旧 `running` turn。
6. 观察至少一个完整的 coder → PR/失败通知路径，并确认 CMA 不再存在无 live execution owner
   的 `running` Session。
7. Ariel 当前 GitHub API 返回零 issue；没有可安全重触发的对象。若 UI 中确有 issue，需取得其
   精确 URL/编号后再核对 repository identity、可见性与 source filter。

## 11. 验收标准

1. provider 根进程在 turn 中退出后，CMA 在 drain grace + 正常传播延迟内变为 terminated。
2. 任意 terminal 竞态下恰好一个 terminal event，`go test -race` 无数据竞争或 channel panic。
3. `runtime_timeout: 0` 的长任务仍可运行；杀死 provider 后不会继续显示 `running`。
4. 重启 ahsir 后，所有遗留 `running` Session 被一次性结算，第二次重启不重复写事件。
5. 当前 5 个 zombie Session 均不再是 `running`，且 GitHub 侧可看到 source-specific 失败通知。
6. 重触发后的任务能完成 PR 或明确失败，不再无声悬挂。
7. ahsir 内部不包含 GitHub-specific 通知逻辑；Hetairoi adapter 只消费 typed terminal outcome。
