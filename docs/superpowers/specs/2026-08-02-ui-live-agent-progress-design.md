# UI 实时展示 Agent 执行过程设计

**日期：** 2026-08-02
**状态：** Approved

## 1. 背景

Hetairoi 经 CMA facade 启动 Codex Agent 后，ahsir UI 会立即从 invocation
ledger 得到一条 context，因此左栏能看到会话；但主区域只读取 Agent 的
`/history`。运行中的轮次尚未进入 transcript，于是主区域显示“还没有对话”。

当前还存在两处放大问题：

1. `CodexSession` 虽然逐行读取 `codex exec --json`，却先把 tool events
   累积到 `codexRunResult`，等进程结束后才批量发送；上层收到的并非实时事件；
2. Agent scale-to-zero 后仍留在 registry，history 路径会直连已经退出的本地
   端口；连接失败又被 UI 吞掉并渲染为空 transcript，导致完成后的会话也不可见。

本设计同时修复“运行时空白”“执行过程不可见”“完成后仍空白”三类问题。

## 2. 目标

1. 外部调用方创建 invocation 后，UI 无需由自身提交消息，也能立即显示用户任务和
   Agent 的运行状态。
2. Codex 执行过程中，command/tool 开始、结束及失败事件按实际发生顺序逐条抵达 UI，
   不等整个 turn 完成后批量回放。
3. 完成、失败、取消和恢复中的 invocation 都有明确界面状态。
4. 最终回复进入 transcript 后，UI 用权威 transcript 替换临时运行视图，不重复消息。
5. Agent scale-to-zero、删除或离线后，保留期内的 transcript 仍可直接从受管工作区读取。
6. history 或 live-event 接口失败时显示真实错误，不再伪装成“还没有对话”。
7. 不暴露模型隐藏 reasoning，不将 tool event 全量持久化为新的 conversation store。

## 3. 非目标

- 不承诺 Codex 的逐 token 打字效果；v1 提供步骤级实时展示。
- 不展示或推断隐藏 chain-of-thought；`thinking` 只作为无内容的状态标记。
- 不改变 CMA Session/outcome 的业务语义。
- 不把 invocation ledger 扩展成完整会话存储。
- 不为断线前的无限历史建立新的永久事件数据库；最终结果仍以 transcript 为准。

## 4. 用户可见状态

一个 context 的中心区域按以下优先级渲染：

| 状态 | UI 行为 |
|---|---|
| 从未产生 invocation，history 为空 | 显示“还没有对话” |
| `queued` | 显示用户任务与“排队中” |
| `in_flight` / `recovering` | 显示用户任务、运行时长和实时步骤 |
| `completed` 且 transcript 可读 | 显示 transcript；移除临时步骤视图 |
| `failed` / `recovery_failed` | 保留用户任务与已收到步骤，显示错误 |
| `canceled` | 保留用户任务与已收到步骤，显示“已取消” |
| history 请求失败 | 显示“无法读取会话记录”及安全化错误摘要 |

“没有对话”只允许表达真正的空状态，不能再表达网络、进程或存储错误。

## 5. 后端设计

### 5.1 CodexSession 真正逐事件发送

将 Codex runner 从“返回一个装满事件的 `codexRunResult`”改为“解析 JSONL 时通过
callback/channel 立即发送事件，结束时只返回 turn 的最终摘要”。

映射规则：

| Codex JSONL | wrapper event |
|---|---|
| `thread.started` | 立即持久化 thread ID |
| command/tool `item.started` | `EventToolUse` |
| command/tool `item.completed` | `EventToolResult`；若缺少 started，先补发 use |
| reasoning item | `EventThinking`，不携带 reasoning 内容 |
| `agent_message` completed | 保留为本轮最终文本候选，不作为隐藏 reasoning 暴露 |
| `turn.completed` | `EventTurnDone` + usage |
| `turn.failed` / protocol error | `EventTurnDone{Err: ...}` |

tool use/result 使用 Codex item ID 关联。命令结果文本须限制大小；超过上限时按 UTF-8
边界截断并标注。已有 ClaudeSession 行为保持不变。

如果 Codex 版本没有发出 `item.started`，在 `item.completed` 到达时补发一组相邻的
use/result，保证旧版本仍有可见进度。

### 5.2 Scheduler live-event hub

Scheduler 在代理 `message/stream` 时解析 A2A SSE 的副本，同时保持原始字节、顺序和
flush 行为不变。识别到的 status update 写入内存 live-event hub：

```json
{
  "id": "live-123",
  "invocationId": "inv-232",
  "contextId": "ctx_...",
  "agentName": "cma-...",
  "type": "tool_use",
  "at": "2026-08-02T08:47:10Z",
  "name": "command_execution",
  "toolUseId": "item_...",
  "input": {"command": "go test ./..."}
}
```

支持的 `type`：

- `status`
- `text_delta`
- `tool_use`
- `tool_result`
- `thinking`
- `span_start`
- `span_end`
- `terminal`

提供两个只读接口：

- `GET /context-events?contextId=<id>`：返回该 context 当前内存窗口的快照；
- `GET /context-events/stream?contextId=<id>&after=<event-id>`：SSE live tail，支持游标重连。

约束：

- 每个 context 最多保留 256 个事件；全局最多保留 128 个 context；
- 单个序列化事件最大 32 KiB，tool result 文本最大 16 KiB；
- 慢订阅者使用有界队列；溢出时断开并让客户端用快照重连，不能阻塞 Agent；
- terminal 后保留内存窗口 30 分钟，然后回收；
- hub 不写 invocation ledger，也不落盘；scheduler 重启后 live window 可丢失，最终
  transcript 和 invocation 状态不能丢失；
- 不转发 reasoning 内容、环境变量、认证 header 或 provider 原始 envelope。

无法识别的 SSE frame 原样代理给调用方，但不进入 live-event hub。

### 5.3 Invocation summary

`/api/contexts` 为每个 context 增加当前/最近 invocation 的以下字段：

- `invocationId`
- `userText`
- `speaker`
- `lastStatus`
- `startedAt`
- `durationMs`
- `error`

`userText` 继续遵守 ledger 现有的 512-byte UTF-8 安全截断；它只用于 history 尚未完成时
的 provisional bubble，不代替最终 transcript。

### 5.4 scale-to-zero history

history 属于受管工作区的只读文件，不需要为了读取它唤醒 LLM runtime。

处理规则：

1. Agent 处于 idle-stopped 时，直接从其受管 workspace 的 TranscriptStore 读取；
2. Agent 已从 registry 删除但 workspace 存在时，沿用 archived history；
3. Agent 正常运行时优先读取 runtime `/history`；若发生明确的本地连接失败，并且目标是
   scheduler 管理的 workspace，则回退磁盘读取；
4. 不对外部注册、非受管 workspace 的 Agent 猜测路径或做磁盘回退；
5. 保持 retention、路径穿越防护及 internal-token 边界。

读取 history 不唤醒 scale-to-zero Agent，避免仅浏览 UI 就产生冷启动成本。

## 6. 前端设计

### 6.1 打开 context

打开会话时并行请求：

1. transcript history；
2. invocation/context summary；
3. live-event snapshot。

若 invocation 尚未终结，先渲染 provisional user bubble 和状态卡，再建立 EventSource。
每个 live event 按 event ID 去重后追加。terminal 到达后刷新 history；拿到 transcript 后
原子替换 provisional 视图。

### 6.2 实时步骤展示

实时步骤在 Agent 气泡中以折叠列表展示：

- 默认展示最新步骤、状态和运行时长；
- command/tool name 与安全化 input 可展开；
- tool result 默认折叠，错误结果突出显示；
- `thinking` 仅显示“Agent 正在思考”，不显示内容；
- 同一 tool use/result 合并成一项；
- 自动滚动仅在用户已经接近底部时发生，用户向上阅读时不得抢滚动位置。

#### 6.2.1 Streaming delta 合并（现场验证修订，2026-08-02）

现场验证发现 provider 的 `text_delta` 通常只有一个 token 或短语。前端不得把每个
delta 渲染成独立块，否则 Markdown、空白和自然段会被拆成“一词一行”。

UI 在展示前先把 live event 归一化为稳定的 display blocks：

1. 连续的 `text_delta` 按到达顺序拼接到同一个 text block，保留原始空白；
2. 遇到 `tool_use`、`tool_result`、status/span 或 terminal 时结束当前 text block；
3. 增量到达时更新该 text block 的既有 DOM 节点，并对累计文本重新执行安全 Markdown
   渲染；不得为每个 delta 新建 `<div>`，也不得重建整个 thread；
4. Markdown 尚未闭合时允许暂时按当前累计文本展示，后续 delta 到达后自然修正；
5. `tool_use` 与相同 `toolUseId` 的 `tool_result` 合并为同一个可折叠步骤，result 到达前
   显示运行态，到达后更新为成功或失败；没有 ID 的旧事件按顺序独立展示；
6. `thinking` 只维持单个“Agent 正在思考”状态，不按事件数重复追加；
7. 仅当更新前滚动位置接近底部时跟随新内容；用户主动向上阅读时保持当前位置；
8. terminal 后仍以最终 transcript 原子替换 provisional 内容，最终历史不重复 user/reply。

归一化只属于 UI display state，不修改 scheduler live-event wire，也不把增量事件写入
transcript。

### 6.3 错误与重连

- history 失败：显示独立错误态和“重试”，不调用 `renderThread([])`；
- SSE 断线：浏览器自动重连并携带最后 event ID；服务端窗口已淘汰时重新取 snapshot；
- context 切换或离开 chat mode 时关闭旧 EventSource，防止串会话和泄漏；
- terminal 后关闭 EventSource，并刷新 contexts、trace 和 history。

### 6.4 root/plugin 资产一致性

`internal/ui/assets/` 仍是 canonical source。所有前端修改同步到
`plugin/src/internal/ui/assets/`，现有 parity test 必须继续通过。

## 7. 兼容性

- 现有 `/invocations`、`/history`、A2A `message/send` 与 `message/stream` wire shape
  保持兼容；新增字段和 endpoint 均为 additive。
- Claude、Codex custom provider 与非 streaming Agent 均可使用 UI；没有步骤事件时仍显示
  invocation 状态和最终 transcript。
- 直接从 UI 发起的 async chat 继续工作，但改为复用统一 live-event 路径，不保留另一套
  仅靠 task polling 的渲染逻辑。

## 8. TDD 顺序

实现必须严格按以下 red → green 顺序分批进行：

1. **History red**：idle-stopped Agent 的 history 当前返回 connection refused；新增测试先
   固定“磁盘读取且不唤醒”。
2. **Codex red**：用阻塞 JSONL fixture 证明第一条 command event 必须在 `turn.completed`
   前到达；覆盖 started 缺失、result 截断、失败和取消。
3. **Hub red**：覆盖 snapshot、SSE 顺序、after 游标、慢订阅者、容量回收、terminal TTL、
   context 隔离与敏感字段不进入事件。
4. **Proxy red**：证明 SSE 被观测后转发字节和 flush 时序不改变，异常/未知 frame 不破坏
   上游响应。
5. **UI state red**：fake-DOM 覆盖 external invocation 的 provisional bubble、live merge、
   terminal→history 替换、history 错误、切换 context 关闭订阅。
6. **Browser red**：Playwright fixture 延时发出 tool events，断言页面在 terminal 前逐步
   出现内容；覆盖重连和 scale-to-zero history 回放。
7. 全量执行 `make test-ui-fast`、`make test-ui-browser`、`go test ./...`、`go build ./...`。

每一步先保留可复现的 red 证据，再写最小实现，最后重构。

## 9. 验收标准

使用一个耗时至少 5 秒、包含两次 command execution 的 Codex fixture：

1. invocation 创建后 1 秒内出现用户任务与运行状态；
2. 第一次 command 完成后、整个 turn 结束前，页面已经显示该步骤；
3. 第二次 command 与第一次顺序一致，tool result 不串 ID；
4. terminal 到达后最终 transcript 可见，用户任务和最终回复各只出现一次；
5. reload/切换 context 后不会重复事件或串到其他 context；
6. Agent scale-to-zero 后重新打开，history 仍可见，且没有唤醒 Agent；
7. history 故障显示错误而不是“还没有对话”；
8. hidden reasoning、认证 header 和环境变量未出现在 live-event API/UI；
9. root/plugin parity、UI 快测、Chromium E2E、全量 Go test/build 全部通过。
10. 将 `**Step`、` 3`、`:**`、` done` 等碎片化 delta 连续发送时，运行中页面只产生
    一个 text block，累计结果等价于 `**Step 3:** done`，不得一词一行；
11. 同一 `toolUseId` 的 use/result 在运行中只占一个步骤卡片，result 到达后原位更新；
12. Browser E2E 在 terminal 前截图/读取 DOM，验证流式 Markdown、自然段和滚动行为，
    不能只检查最终 history。

## 10. 交付边界

本修复以一组原子提交交付：spec、后端测试与实现、前端测试与实现、文档更新。除非用户
另行要求，本地完成后不自动 push。
