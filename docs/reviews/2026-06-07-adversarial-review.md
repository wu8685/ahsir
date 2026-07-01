# Ahsir 对抗式双 Codex Review（2026-06-07）

由两个 Codex-backed ahsir agent 对本项目做对抗式 review：

| Agent | 角色 | 立场 |
|---|---|---|
| `reviewer-codex` | 怀疑派 Critic | 只看当下风险：bug、race、安全、过度设计 |
| `visionary-codex` | 远见派 Architect | 只看潜力：改进点、生态定位、feature 提案 |

流程：各自独立 review → 互换报告交叉辩论 → 用户质疑「CLI+skill 够了」→ visionary 修订 roadmap。
会话 context：`ahsir-self-review-critic` / `ahsir-self-review-visionary`（可继续追问）。

---

## 一、共识缺陷清单（双方辩论后无争议）

### P1 —— 阻塞性问题

1. **Ingress 无 auth + registry 劫持**
   - `/admin/agents` 无鉴权，可 start/stop 任意 workspace agent（`internal/scheduler/gateway.go:82,350`）
   - registry `POST /agents` 无鉴权，同名注册直接覆盖（`internal/registry/registry.go:132,40`）
   - scheduler 按 registry card 发请求并附带 internal token，可被劫持诱导泄露（`internal/scheduler/scheduler.go:586,601`）

2. **确定性 data race**
   - registry `Get`/`List` 在 `RLock` 下写 `entry.status`（`internal/registry/registry.go:48,62`）
   - SessionPool `enforceCap` 无锁读其他路径持 `entry.mu` 写的字段；注释把 race 合理化为 "soft limit"（`internal/wrapper/session_pool.go:430` vs `311,623`）

3. **Claude `runtime.timeout` 未生效**
   - `SessionConfig.Timeout` 被解析但 `ClaudeSession.Stream` 没建 per-turn deadline（`internal/wrapper/session_claude.go:264`）
   - Codex 路径有 turn-level `context.WithTimeout`（`internal/wrapper/session_codex.go:119`），provider 语义不一致

4. **启动生命周期不可判定**
   - wrapper `ListenAndServe` 错误被 goroutine 吞掉（`internal/wrapper/wrapper.go:114`）
   - port allocator 只递增不探测可 bind 性（`internal/scheduler/config.go:141`）
   - scheduler Start 中途失败不清理已启动 server（`internal/scheduler/scheduler.go:141,155`）

5. **Sandbox / permission 可被 `runtime.args` 绕过**
   - `runtime.args` 原样进入 session args（`cmd/ahsir-agent/main.go:219`）
   - Codex 已有 `--sandbox` 时不强制 read-only，sanitizer 不剥离 danger 类 flag（`internal/wrapper/session_codex.go:232,271`）
   - Claude 只剥离 stream-json 冲突 flag（`internal/wrapper/session_claude.go:160`）

6. **Executor 失败路径不推进 depth**
   - 非法 call、无 caller、sub-call 失败均用原 depth 递归，靠 timeout 兜底（`internal/wrapper/executor.go:155,160,178`；streaming 路径 `:339`）
   - 修复方向：attempt/error budget

7. **Ledger 隐私与输入上限**
   - 完整 `UserText` 以 `0644` 落盘，与 session persistence 的 `0600` 不一致（`internal/scheduler/invocation_ledger.go:187,316`）
   - A2A proxy `io.ReadAll` 无上限，各 handler 无 `MaxBytesReader`（`internal/scheduler/gateway.go:218,264`）

### P2

- `network.bind` 配置不控制真实监听地址，card 语义与运行行为不一致（`internal/wrapper/card.go:207` vs `internal/wrapper/wrapper.go:109`）
- config edit "atomic rename" 的 temp file 在系统 tmp，跨 filesystem 时 `os.Rename` 失败（`internal/scheduler/configedit.go:95,116`）

### 辩论轮新挖出的证据

- `agent new` 把 agent-card 写成 `0644`；literal `apiKey` 明文落普通可读文件（`cmd/ahsir/agent.go:438,466`）
- A2A proxy 在 upstream status < 500 时即标记 complete，SSE 中途断流可能记为完成——trace 语义不可靠（`internal/scheduler/gateway.go:245,254`）
- `StreamWithAgent` 注释说 direct-to-agent，实际本地 stream 已走 scheduler 改写后的 `/a2a/{name}`——边界语义混乱（`internal/schedulerclient/client.go:140,165,176`）

---

## 二、定位共识

- **ahsir = local-first multi-agent control plane**：把 Claude/Codex CLI 变成 A2A-compatible runtime（Claude 长进程 session `internal/wrapper/session_claude.go`、Codex thread resume `session_codex.go`、SessionPool 统一抽象 `session_pool.go`）
- **不做 MCP 替代品**；不复制云端 agent platform——差异化是 local-first、CLI-native、可恢复、可审计
- Critic 的限定：这是「潜力」，不是「可放心扩展的 foundation」——hardening 先行

## 三、产品路线决策（2026-06-07，经用户确认）

> **CLI + skill + hardened scheduler + trace/doctor**

用户判断：「CLI + orchestrator skill 够了」。visionary concede 并连带重估。

### Now —— hardening 前置包

- **Auth 基线**：admin token + registry write token + 禁止未授权同名覆盖 + internal token 只发给 scheduler-owned local URL
- **Race-free 基线**：修 registry / SessionPool race；`go test -race ./internal/...` 进可信门槛
- **Runtime policy**：Claude per-turn timeout + Codex/Claude arg sanitizer（sandbox/permission denylist）
- **Lifecycle 可判定**：listener 错误同步回传、port probe/reserve、Start 失败清理
- **Hygiene**：`MaxBytesReader`、ledger `0600` + UserText redaction、agent-card `0600`

### Next —— CLI-first observability

- `ahsir trace <contextId>`：基于 ledger 输出 agent-call DAG、耗时、状态（前置：ledger hygiene + SSE completion 语义修复）
- `ahsir doctor`：检查 scheduler / agent readiness / provider env / sessions / ledger 可写性

### Later

- Persistent TaskStore + `tasks/resubscribe`（等出现外部 A2A client 或长任务恢复需求）
- Typed capability selection（等 fleet 复杂度超过 prompt + skill list 承载能力：agent 数量多、delegation 误选频繁、需要 policy/cost routing）
- Agent template marketplace（前置：arg sanitizer + policy schema + card 权限修复）
- Multi-scheduler federation（前置：全部 auth / trust / bind semantics；现有 `Remote` 字段只是配置占位）

### Backlog（trigger-based，非高优）

- **MCP server mode**（`ahsir mcp` 暴露 `agent_list` / `agent_chat` / `agent_trace`）
  - 决策：从主 roadmap 移除，不作承诺功能；仅当以下信号出现时重提——
    - A. ≥2 个 MCP-only client 的真实需求（Claude Desktop / IDE host / 第三方 runtime）
    - B. CLI 文本协议成为故障源（agent 频繁误解析输出、需要严格 tool schema 校验）
    - C. 需要把 Agent Card / trace / session 状态暴露为 MCP Resources（超出 Tools 范畴）
    - D. 集成方明确要求「只注册 MCP server、不装 skill」
  - 硬前置：scheduler ingress auth 完成
