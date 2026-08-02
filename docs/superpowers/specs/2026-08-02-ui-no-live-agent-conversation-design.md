# 无可用 Agent 时的新对话交互设计

**日期：** 2026-08-02
**状态：** Approved

## 1. 背景

当前 scheduler 没有 live Agent 时，控制台仍允许点击「新对话」。点击后客户端会生成一个
新的 `contextId`、清空中心区域，并尝试 focus composer；但 composer 已被设置为
`disabled`，浏览器不会让它获得焦点。由于新 context 尚未产生消息，它也不会进入会话列表。

用户最终只能看到标题和 context ID 变化，中心区域近似空白，输入框与发送按钮又缺少明显的
禁用反馈，因而表现得像「按钮没有响应」。

## 2. 目标

1. 没有 live Agent 时，点击「新对话」必须给出明确、持续可见的解释和下一步操作。
2. 不创建无法发送消息的幽灵 context，也不让用户误以为蓝色发送按钮仍可使用。
3. 用户在 scheduler 主机启动 Agent 后，可以从当前页面恢复，不必猜测是否要刷新浏览器。
4. 保持现有 ahsir 控制台的高密度、克制型运维界面；不增加 modal。
5. 区分「没有 Agent」「Agent 已离线/归档」「scheduler 不可达」，不使用同一条含混提示。

## 3. 非目标

- 本设计不改变 Agent 创建方式；控制台仍只生成 `ahsir agent new` 命令，不直接写 scheduler
  主机文件。
- 不改变历史会话、archived Agent 或 CMA Session 的生命周期。
- 不改变正常情况下 context 的创建、发送和持久化语义。
- 不在本设计中增加 scheduler 自动启动能力。

## 4. 设计方向

采用 **inline operational empty state**：问题发生在哪里，就在中心工作区解释并提供恢复入口。
不使用 toast 作为主要反馈，因为 toast 会消失，也无法承载持续存在的系统状态；不使用 modal，
因为没有 live Agent 不是一次性确认，而是整个 chat surface 当前不可用的原因。

页面结构：

```text
┌ 左栏 ───────────┬ 中心工作区 ─────────────────────────┐
│ + 新对话         │ 新对话        等待可用 Agent         │
│ 多 Agent 协同    │                                   │
│ 圆桌             │          ○                        │
│ 新 Agent         │      当前没有可用 Agent             │
│                  │  新对话需要一个正在运行的 Agent。    │
│ 历史会话…        │                                   │
│                  │  [配置新 Agent]  [重新检查]          │
│ scheduler · 0    │                                   │
│                  │ ┌ 启动 Agent 后可开始对话 ────────┐ │
│                  │ │                         ○       │ │
│                  │ └─────────────────────────────────┘ │
└──────────────────┴───────────────────────────────────┘
```

视觉沿用现有 token、边框、圆角和 typography。empty state 不使用大面积卡片或装饰插图，只用
一个低对比度状态圆环、两级文本和两个紧凑按钮。主按钮使用现有 accent，重试使用普通边框按钮。

## 5. 状态与行为

### 5.1 点击「新对话」

| 前置状态 | 行为 |
|---|---|
| 至少一个 live Agent | 保持现有行为：建立 fresh draft、选择当前 live Agent；若当前目标无效，则选择第一个 live Agent，并 focus composer |
| 没有 live Agent | 不生成 `contextId`；进入 `no-agent` empty state；composer 保持禁用 |
| scheduler 不可达 | 不进入 `no-agent`；显示独立的 `scheduler-error` empty state，提供「重新连接」 |

没有 Agent 时，header 显示：

- 标题：`新对话`
- context badge：`等待 Agent`

不展示随机的 `ctx · xxxxxx`，因为该 context 既不能发送，也不会持久化。

### 5.2 `no-agent` empty state

中心文案：

- 标题：`当前没有可用 Agent`
- 说明：`新对话需要一个正在运行的 Agent。你可以先配置一个，或在启动后重新检查。`
- 主操作：`配置新 Agent`
- 次操作：`重新检查`

操作行为：

1. `配置新 Agent` 复用现有 `newAgentForm()`，进入命令生成页面。
2. `重新检查` 调用 `loadAgents()`：
   - 请求期间按钮显示 `检查中…` 并临时禁用；
   - 找到 live Agent 后创建 fresh draft、选中第一个 Agent、启用并 focus composer；
   - 仍为空时保留 empty state，并显示内联状态 `仍未发现运行中的 Agent`；
   - 请求失败时切换为 `scheduler-error`，不能把错误误报为零 Agent。

左侧 archived 会话保持可点击；用户可以离开 empty state 阅读历史记录。

### 5.3 composer 禁用态

禁用必须同时具有语义和视觉反馈：

- `textarea.disabled = true`、`readOnly = true`；
- placeholder 改为 `启动 Agent 后可开始对话`；
- composer wrapper 增加 `.is-disabled`，整体降低对比度，不显示 focus ring；
- textarea 使用 `cursor: not-allowed`；
- Agent selector 显示 `无可用 Agent` 并禁用；
- 发送按钮变为中性灰色，禁用 hover brightness，不能继续显示蓝色可用态；
- wrapper 增加 `aria-disabled="true"`，empty state 的说明通过 `aria-describedby`
  与 composer 建立关联。

只使用 native `disabled` 仍不够，因为它解释不了原因；只使用视觉变灰也不够，因为键盘和
辅助技术仍会把它当成可操作控件。两者必须同时存在。

### 5.4 Agent 恢复

除手动「重新检查」外，chat mode 的轻量轮询应同时刷新 Agent 列表。若页面当前处于
`no-agent`：

1. 检测到第一个 live Agent 后，empty state 原地切换为 fresh draft；
2. toast 显示 `已发现 <agent-name>`；
3. 启用 composer，但不自动发送、不自动创建会话记录；
4. 若用户正在查看 archived 会话、Agent 配置表单、room 或 roundtable，不抢走当前页面。

这使用户在另一个终端执行生成的命令后，无需整页刷新，同时避免后台轮询打断当前工作。

### 5.5 相关状态文案

| 状态 | 中心标题 | 操作 |
|---|---|---|
| scheduler 在线、0 live Agent | `当前没有可用 Agent` | `配置新 Agent`、`重新检查` |
| scheduler 请求失败 | `无法连接 scheduler` | `重新连接` |
| 历史参与者不再 live | `该 Agent 当前离线` | 保持历史只读；不把「新对话」绑定到该 Agent |
| archived Agent | `归档会话 · 只读` | 保持现有行为 |

## 6. 响应式行为

- Desktop：empty state 位于 center surface 的视觉中心，最大宽度 `420px`；composer 固定在底部。
- Mobile：点击左栏「新对话」后必须自动切换到 center surface，否则用户只会看到左栏按钮状态；
  操作按钮纵向排列并占满 empty state 宽度。
- 窄屏下标题、说明和按钮不得被固定 composer 覆盖；empty state 容器保留 composer 高度对应的
  bottom padding。

## 7. 实现边界

前端引入明确的 chat availability renderer，而不是让 `renderDetail()` 顺便决定 composer：

```js
renderChatAvailability({ schedulerState, liveAgents, selectedAgent, mode })
```

该函数统一负责：

- `ready` / `no-agent` / `scheduler-error` / `read-only` 状态；
- composer 的 enabled、placeholder、class 与 ARIA；
- chat empty state；
- context badge 是否显示真实 ID。

`renderDetail()` 只负责右栏详情，避免右栏是否渲染意外控制中心 composer。canonical asset 仍是
`internal/ui/assets/`，修改后同步到 `plugin/src/internal/ui/assets/`，保持 parity contract。

## 8. TDD 顺序

用户确认本规格后，按以下 red → green 顺序实现：

1. **Fake DOM red**：0 Agent 点击「新对话」后不生成 context ID，显示 empty state，composer
   禁用且文案正确。
2. **Fake DOM red**：点击「重新检查」从 0 Agent 变为 1 Agent 后，fresh draft 可写并选中目标。
3. **Fake DOM red**：scheduler 失败显示 error state，不误报为 0 Agent。
4. **Browser red**：验证按钮、disabled 样式、focus、ARIA，以及 Agent 出现后的恢复路径。
5. **Responsive red**：mobile 点击左栏操作后切换到 center surface；empty state 与 composer 无遮挡。
6. 实现最小状态 renderer，再重构 `renderDetail()` 与 Agent polling 的职责。
7. 执行 `make test-ui-fast`、`make test-ui-browser`、asset parity 和相关 Go tests。

## 9. 验收标准

1. scheduler 为 0 Agent 时，点击「新对话」立即出现解释与两个可执行操作。
2. 页面不生成或展示不可用的随机 context ID，左侧也不产生幽灵会话。
3. textarea、selector、发送按钮均呈现明确 disabled 状态；蓝色发送按钮不再误导。
4. `配置新 Agent` 能进入现有命令生成页。
5. Agent 启动后，手动重新检查或轻量轮询均能恢复 fresh draft，且 composer 可 focus、可输入。
6. scheduler 故障、零 Agent、离线历史参与者和 archived Agent 四种状态文案互不混淆。
7. keyboard、screen reader、desktop 与 mobile 行为符合本规格。
8. canonical/plugin 资产一致，UI fast/browser 测试全部通过。

## 10. 需要确认的产品决策

本设计建议：**零 Agent 时点击「新对话」进入 inline empty state，不自动跳转到「新 Agent」表单。**

理由是 scheduler 可能只是尚未启动已有 Agent，直接跳转到创建流程会误导用户重复创建；empty
state 同时提供「配置新 Agent」和「重新检查」，能覆盖两条真实恢复路径。
