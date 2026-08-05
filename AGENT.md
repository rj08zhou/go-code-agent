# AGENT.md

本文件由 **go-code-agent 运行时**自动加载：放在 `--workdir` 根目录时，会注入到该会话的 system prompt（`## Project Instructions`）。

查找顺序（取第一个存在的文件）：`AGENT.md` → `CLAUDE.md` → `AGENTS.md`。

下面是本仓库（go-code-agent）的项目说明；在其他项目里应改成**那个项目**的约定。

## 项目概要

Go 终端编码 Agent（CLI/REPL）。模块路径 `go-code-agent`，Go 版本以 `go.mod` 为准。

## 常用命令

```bash
go build -o agent ./cmd/agent
go test ./...
go test ./internal/<pkg>/ -run TestName -v
```

## 改代码时

- 组合根在 `internal/application`；业务逻辑不要堆在 `cmd/`。
- Agent 私有状态用 `store.AtomicWritePrivate`；用户工作区文件用 `store.AtomicWrite`。
- 文件路径走 `security.SecurePath`；不要削弱沙箱 / SSRF / 脱敏。
- 行为变更时同步更新同包测试。
- 风格跟邻近文件一致；不要无关重构。

## 不要做

- 不要擅自 commit / push（除非用户明确要求）。
- 不要提交密钥或会话数据。
- 不要为了「完整」去加 HTTP API 层。
