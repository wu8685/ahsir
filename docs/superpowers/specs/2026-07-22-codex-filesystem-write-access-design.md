# Codex Filesystem Write Access Mapping

日期：2026-07-22

## 背景

Hetairoi coder v2 已成功使用 Codex CLI + `gpt-5.6-sol` 完成只读 smoke turn，但原生 retry issue #29 时无法创建 `/tmp/ahsir-29`，因此没有进入 coding。

生成的 Agent Card 已明确声明：

```yaml
filesystem:
  enabled: true
  write_access: true
  shell_access: true
```

实际 Codex 命令仍固定带 `--sandbox=read-only`。原因是 `internal/wrapper/session_codex.go` 的 `buildCodexExecArgs` 无条件选择只读 sandbox；`cmd/ahsir-agent/main.go` 仅为 Claude Code 把 `FilesystemConfig.WriteAccess` 转换成 tool allow-list，没有把这个字段传给 Codex session。

## 目标

1. Codex provider 尊重 Agent Card 的 `filesystem.write_access`。
2. 未显式开启 write access 的 Codex agents 继续保持 `read-only`。
3. raw `runtime.args` 仍不能覆盖 sandbox 或 approval policy。
4. root source 与 plugin shipping source 保持一致。
5. 修复后 Hetairoi coder 能在受限 `workspace-write` sandbox 中完成 issue #29 coding。

## 非目标

- 不支持 `danger-full-access`、`--yolo` 或绕过 approvals/sandbox。
- 不允许 `runtime.args` 决定权限。
- 不改变 Claude Code provider 的 `allowedTools` 行为。
- 不因为 `shell_access=true` 自动获得更宽的 filesystem sandbox；write access 仍由独立字段控制。
- 不开放额外 network policy；Codex CLI 的现有登录与网络行为保持不变。

## 设计

### 1. Provider-neutral session capability

`wrapper.SessionConfig` 新增：

```go
WriteAccess bool
```

它表示 card 已授权当前 session 修改允许范围内的 filesystem。该字段不是 CLI flag passthrough，也不接受字符串 sandbox level。

`buildSessionConfig` 只在以下条件同时成立时设为 `true`：

```text
filesystem.enabled && filesystem.write_access
```

这样关闭 filesystem 时，即使残留 `write_access: true` 也不会获得写权限。

### 2. Codex sandbox selection

`buildCodexExecArgs` 从 `SessionConfig.WriteAccess` 派生唯一两种值：

| WriteAccess | Codex sandbox |
|---|---|
| `false` | `read-only` |
| `true` | `workspace-write` |

`runCodexExec` 把 session capability 传入 args builder。不得增加可表达 `danger-full-access` 的通用字符串字段。

### 3. Security boundary remains card-owned

`sanitizeCodexExecArgs` 继续删除：

- `--sandbox` / `-s` 的所有形式；
- `--dangerously-bypass-approvals-and-sandbox`、`--yolo`、`--full-auto`；
- 修改 `sandbox_mode`、`approval_policy` 或 `sandbox_workspace_write.*` 的 config overrides。

最终 sandbox flag 只能由可信的 card schema → `SessionConfig.WriteAccess` 路径生成。

### 4. Shipping source synchronization

以下 root files 的行为变更必须同步到 `plugin/src/` 对应文件：

- `internal/wrapper/session_types.go`
- `internal/wrapper/session_codex.go`
- `internal/wrapper/session_codex_test.go`
- `cmd/ahsir-agent/main.go`
- `cmd/ahsir-agent/main_test.go`

root 与 plugin bundle 已存在历史 source drift，因此不要求整文件 byte-identical，也不运行会带入全部 drift/vendor 的 `make plugin-src`。在两侧分别落同一权限契约和测试，使本次行为一致，同时保持提交范围可审查。

## TDD 验收

先写失败测试，再实现：

1. Codex session 未获 write access 时仍生成 `--sandbox=read-only`。
2. Codex session 获得 write access 时生成 `--sandbox=workspace-write`。
3. user runtime args 即使请求 `danger-full-access`，最终仍只能得到 card 派生的值。
4. `buildSessionConfig` 仅在 `enabled && write_access` 时设置 session write capability。
5. Claude provider 既有 permission tests 不变。
6. root 与 plugin 的 write-access mapping tests 使用同一组输入/期望并分别通过。
7. `go test ./internal/wrapper ./cmd/ahsir-agent`、`go test -race ./...` 与 `go build ./...` 全部通过。

## 运行时验收

1. 重新部署匹配的 `ahsir` / `ahsir-agent` binary pair。
2. 重启或重新注册 coder v2，使新 wrapper 生效。
3. 用一次受控 turn 创建、读取并删除其 workspace 内的临时文件，证明 `workspace-write` 生效；不操作用户文件。
4. 再次原生 retry issue #29。
5. transcript 必须出现实际 clone/edit/test 活动，而不是 read-only blocker；后续由现有 Hetairoi workflow 创建 PR。

## 回滚

- 恢复时间戳匹配的 binary pair 并重启 launchd service。
- 不改动 coder/reviewer v2 配置，因为 card 的 write access 在旧 binary 下仍会退化为只读。
- 保留失败与成功 session/event history，不删除 workspace。
