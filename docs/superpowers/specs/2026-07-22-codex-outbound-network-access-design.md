# Codex outbound network access 设计规格

## 背景

Hetairoi coder 已能通过 `filesystem.write_access` 获得 Codex `workspace-write` sandbox，但 Codex 的 workspace-write sandbox 默认禁止 outbound network。Issue #29 的执行因此无法 clone/push GitHub 仓库；通过 raw `runtime.args` 注入 `sandbox_workspace_write.network_access=true` 又会被现有安全过滤器正确拦截。

## 目标

- 在 Agent Card 中增加明确、类型化、默认关闭的 outbound network capability。
- 仅当 card 同时授予 workspace write 与 outbound network 时，Codex CLI 才收到可信的 `sandbox_workspace_write.network_access=true` override。
- CMA agent 可通过 metadata 为指定 agent 打开此 capability。
- raw runtime args 仍不能修改 sandbox、approval policy 或 network capability。
- Claude Code/Kimi 的现有行为不变。

## 非目标

- 不提供 `danger-full-access`。
- 不允许 Agent 自行通过任意 Codex config override 扩权。
- 不改变本机 proxy 配置，也不在 card 中存储 proxy 或凭据。
- 不为所有 agent 默认开放网络。

## Schema 与映射

Agent Card 的 `network` 新增：

```yaml
network:
  outbound_access: true
```

CMA facade 读取 metadata `network_access=true`，映射为 `network.outbound_access: true`。只有字符串精确等于 `true` 时开启；缺失、空值或其他值均为 false。

`buildSessionConfig` 将 card capability 转成 `SessionConfig.NetworkAccess`。为避免出现“只联网但不能形成可工作的 GitHub checkout”的模糊状态，Codex 的有效 network capability 定义为：

```text
filesystem.enabled && filesystem.write_access && network.outbound_access
```

## Codex CLI 行为

`buildCodexExecArgs` 的可信输入增加 `networkAccess bool`：

| WriteAccess | NetworkAccess | sandbox | trusted config override |
| --- | --- | --- | --- |
| false | 任意 | read-only | 无 |
| true | false | workspace-write | 无 |
| true | true | workspace-write | `-c sandbox_workspace_write.network_access=true` |

trusted override 由 wrapper 在 sanitize 完成后追加。用户提供的 `-c/--config sandbox_workspace_write.*` 仍全部删除，因此不能伪造授权或关闭 wrapper 决定的策略。

## 测试要求

- Card YAML/JSON 能 round-trip `network.outbound_access`，默认 false。
- CMA translation 只在 `network_access=true` 时开启。
- SessionConfig 仅在 filesystem write 与 outbound network 均开启时得到 NetworkAccess。
- Codex args 覆盖上述三种组合；trusted override 只出现一次且位于 prompt 之前。
- raw network override 继续被 sanitize。
- root 与 plugin bundle 的相关行为及测试保持一致。

## 验收

1. root/plugin 的 focused tests、全量 tests、race tests 与 build 通过。
2. 更新本地 `ahsir` / `ahsir-agent` binary 并重启 launchd 服务。
3. Hetairoi coder card 显式显示 outbound access；reviewer 保持默认关闭。
4. coder 能在受控 smoke 中访问 GitHub（不输出凭据）。
5. 重新触发 issue #29，观察 Hetairoi 创建实现分支/PR；若仍失败，报告新的具体阻塞点，不再重复盲重试。
