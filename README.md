# go-code-agent

一个使用 Go 编写的终端自主编程 Agent。它通过 LLM 多轮推理与工具调用完成代码阅读、项目分析、文件修改、命令执行、测试、任务管理和团队协作，并提供会话恢复、长期记忆、MCP、HITL 审批和多层安全控制。

> 当前项目是 CLI/REPL 应用，不是 HTTP 服务，也不是 IDE 插件。

## 目录

- [主要能力](#主要能力)
- [快速开始](#快速开始)
- [日常使用](#日常使用)
- [命令行参数](#命令行参数)
- [REPL 命令](#repl-命令)
- [配置参考](#配置参考)
- [架构设计](#架构设计)
- [工具系统](#工具系统)
- [安全与审批](#安全与审批)
- [多 Agent 设计](#多-agent-设计)
- [MCP 集成](#mcp-集成)
- [会话、历史与持久化](#会话历史与持久化)
- [目录设计](#目录设计)
- [开发与扩展](#开发与扩展)
- [测试与评估](#测试与评估)
- [故障排查](#故障排查)
- [当前限制](#当前限制)


## 主要能力

### 代码与终端

- 读取文件、列出目录；
- 按文件名或文件内容搜索；
- 创建、编辑、插入、删除文件；
- 执行前台和后台 Shell 命令；
- 文件变更 diff preview；
- 可选的 git snapshot 和失败回滚；
- 工具输出截断和敏感信息脱敏。

### Agent 运行时

- Lead Agent 多轮工具调用；
- Explore Subagent 隔离只读探索；
- Teammate 持久协作和独立 git worktree；
- 规划门控、失败反思和重复调用检测；
- 自动和手动上下文压缩；
- 可选 LLM-as-Judge；
- 短期 Todo 和持久化任务 DAG；
- 跨会话长期记忆。

### 模型与扩展

- OpenAI API；
- Anthropic API；
- OpenAI-compatible API；
- 流式文本输出；
- LLM 重试、调用超时、角色级并发限制和 Usage 记录；
- MCP stdio server 动态工具注册；
- 工作区 `skills/**/SKILL.md` 技能加载；
- `web_fetch` 和多后端 `web_search`。

## 快速开始

### 1. 环境要求

- Go `1.25.3`，版本要求以 `go.mod` 为准；
- 一个可用的 OpenAI 或 Anthropic API Key；
- macOS、Linux 或其他支持 Go 和 `sh` 的系统；
- 建议安装 Git；Teammate worktree 和 snapshot 功能依赖 Git；
- 使用 MCP 时，需要额外安装对应 MCP server 命令，例如 Node.js、Python 或自定义二进制。

### 2. 获取和编译

```bash
git clone https://github.com/rj08zhou/go-code-agent.git
cd go-code-agent

go mod download
go build -o go-code-agent ./cmd/agent
```

也可以不生成二进制，直接运行：

```bash
go run ./cmd/agent
```

### 3. 配置模型

#### Anthropic

默认 `MODEL_ID` 是 `claude-opus-4.7`。使用 Anthropic 时：

```bash
export ANTHROPIC_API_KEY="your-api-key"
export MODEL_ID="claude-opus-4.7"

./go-code-agent
```

#### OpenAI

使用 OpenAI 时应显式设置与 Provider 匹配的模型名：

```bash
export OPENAI_API_KEY="your-api-key"
export MODEL_ID="gpt-4o"

./go-code-agent
```

#### OpenAI-compatible API

```bash
export LLM_PROVIDER="openai"
export OPENAI_API_KEY="your-api-key"
export OPENAI_BASE_URL="https://your-provider.example/v1"
export MODEL_ID="your-model-id"

./go-code-agent
```

`OPENAI_BASE_URL` 会直接传给 OpenAI Go SDK。它是否需要 `/v1` 后缀取决于兼容服务商的接口约定。

> 启动时必须至少有一个可注册的 Provider。仅设置 `MODEL_ID` 而没有 API Key，应用会在初始化 Gateway 时失败。模型名也应与实际 Provider 匹配。

### 4. 指定目标项目

如果当前目录不是要操作的项目：

```bash
./go-code-agent --workdir /path/to/your/project
```

指定独立状态目录：

```bash
./go-code-agent \
  --workdir /path/to/your/project \
  --data-dir /path/to/config-root
```

实际项目状态会写入：

```text
<data-dir>/go-code-agent/<workdir 的 basename>/
```

默认配置根目录是 `$XDG_CONFIG_HOME`；未设置时使用 `$HOME/.config`。

### 5. 第一次会话

启动后可以直接输入自然语言任务，例如：

```text
分析这个项目的架构，先不要修改代码
```

```text
修复登录模块中的空指针问题，修改前先说明原因，完成后运行测试
```

建议先检查当前安全状态：

```text
/approve
/permissions
/session
/help
```

默认启动姿态是 HITL 开启、`safe-only` 模式：常规安全操作自动继续，高风险 Shell 和文件变更会经过审查或 diff preview。

## 日常使用

### 恢复和创建会话

默认会恢复项目的 active session；没有 active session 时自动创建：

```bash
# 恢复默认 active session
./go-code-agent --workdir /path/to/project

# 恢复指定 session
./go-code-agent --workdir /path/to/project --session <session-id>

# 强制创建新 session
./go-code-agent --workdir /path/to/project --new-session
```

REPL 内可以使用：

```text
/session list
/session new
/session switch <session-id>
/session rename <title>
/session archive
```

切换、创建或归档会话时，当前 `SessionRuntime` 会先关闭，再为目标会话重新组装工具、MCP、后台任务、团队和历史服务。

### 取消和退出

- `Ctrl-D`：退出 REPL；
- `/exit` 或 `/quit`：正常退出；
- 第一次 `Ctrl-C`：取消运行时上下文并开始关闭当前 CLI；
- 第二次 `Ctrl-C`：强制退出进程。

当前实现的第一次 `Ctrl-C` 不只是暂停当前模型调用，而是结束当前 REPL。历史在每轮后同步，启动恢复时还会清理不完整的消息尾部。

### 推荐审批姿态

```text
/approve safe
```

这是默认模式。仅在完全可信的工作区和环境中临时使用：

```text
/approve danger
```

恢复严格人工审查：

```text
/approve off
```

注意：`/approve off` 表示关闭自动批准，不是关闭 HITL。真正关闭审批提示的命令是 `/hitl off`。

## 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `--workdir` | 当前目录 | Agent 的项目工作目录和文件沙箱根目录 |
| `--data-dir` | `$XDG_CONFIG_HOME` 或 `$HOME/.config` | 状态存储的配置根目录 |
| `--session` | 空 | 恢复指定 session ID |
| `--new-session` | `false` | 忽略 active session 并创建新会话 |
| `--human` | `false` | 将启动 HITL 模式切换为 `interactive` |
| `--human-mode` | 空，即默认 `safe-only` | 显式设置 `interactive`、`safe-only`、`auto-approve`、`auto-reject` 或 `notify-only` |

示例：

```bash
./go-code-agent --workdir "$PWD" --human
./go-code-agent --human-mode auto-reject
./go-code-agent --new-session --human-mode safe-only
```

无效的 `--human-mode` 会打印警告并保留默认模式。

## REPL 命令

### 帮助与退出

| 命令 | 说明 |
|---|---|
| `/help` | 显示基础帮助 |
| `/exit`、`/quit` | 退出 |

### 会话

| 命令 | 说明 |
|---|---|
| `/session` | 显示当前 session |
| `/session list` | 列出项目下全部 session |
| `/session new` | 创建新 session |
| `/session switch <id>` | 切换 active session |
| `/session rename <title>` | 重命名当前 session |
| `/session archive` | 保存会话摘要到记忆、归档并创建新 session |

### 任务

| 命令 | 说明 |
|---|---|
| `/tasks` | 显示当前进程内的 Todo 列表 |
| `/dag` | 显示持久化任务 DAG 和进度 |
| `/task clear` | 隐藏已经完成的持久化任务 |
| `/task reset` | 删除当前 session 的持久化任务数据 |

`TodoWrite` 管理短期 Todo；`task_*` 工具管理磁盘上的任务和依赖图。两者不是同一套存储。

### 审批、安全与审计

| 命令 | 说明 |
|---|---|
| `/approve` | 显示审批姿态 |
| `/approve off` | 关闭自动批准，启用交互审查和 diff preview |
| `/approve safe` | 安全操作自动继续，高风险操作审查；默认 |
| `/approve danger` | 自动批准所有 HITL review，跳过 diff preview |
| `/hitl` | 切换 HITL 开关 |
| `/hitl off`、`/hitl on` | 关闭或重新开启 HITL |
| `/hitl <mode>` | 设置完整 HITL 模式 |
| `/permissions` | 显示已加载权限规则 |
| `/permissions reload` | 从磁盘重新加载权限规则 |
| `/decisions` | 显示最近的工具执行决策 |
| `/security` | 显示安全能力摘要 |
| `/security test-bash <cmd>` | 当前仍是占位命令，尚未执行真实校验 |

### MCP

| 命令 | 说明 |
|---|---|
| `/mcp` | 列出活动 MCP server |
| `/mcp pending` | 列出工作区待审批 server |
| `/mcp approve <name>` | 启动待审批 server |
| `/mcp connect <name> <command> [args...]` | 连接临时 stdio server |
| `/mcp disconnect <name>` | 停止并移除活动 server |

### 团队

| 命令 | 说明 |
|---|---|
| `/team` | 列出 teammate |
| `/team spawn <name> <role> <prompt>` | 创建 teammate 和独立 worktree |
| `/team shutdown <name>` | 请求 teammate 关闭 |
| `/team message <name> <content>` | 向 teammate 发消息 |
| `/team inbox`、`/inbox` | 读取 Lead 收件箱 |

### 运行状态

| 命令 | 说明 |
|---|---|
| `/search <query>` | 直接调用 Web Search |
| `/judge` | 切换当前 session 的 Judge 开关 |
| `/usage` | 显示当前 session 的 LLM Usage |
| `/compact` | 手动压缩当前对话 |
| `/memory` | 显示长期记忆统计 |

## 配置参考

配置在进程启动时从环境变量读取。除 `/permissions reload` 和部分 REPL 开关外，修改环境变量后需要重启程序。

### LLM

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `MODEL_ID` | `claude-opus-4.7` | Lead、Subagent 和 Teammate 默认模型 |
| `LLM_PROVIDER` | 按模型名推断 | 强制使用 `openai` 或 `anthropic` |
| `OPENAI_API_KEY` | 空 | OpenAI/OpenAI-compatible 凭据 |
| `OPENAI_BASE_URL` | SDK 默认值 | OpenAI-compatible Base URL |
| `ANTHROPIC_API_KEY` | 空 | Anthropic 凭据 |
| `ANTHROPIC_BASE_URL` | SDK 默认值 | Anthropic Base URL |
| `CONTEXT_WINDOW_TOKENS` | 按模型推断 | 覆盖上下文窗口估算 |
| `LLM_MAX_CONCURRENCY` | `4` | 角色级 LLM 并发容量 |
| `LLM_MAX_QPS` | `4.0` | 当前会被解析，但尚未接入 Gateway 限流 |
| `LLM_MAX_BURST` | `8` | 当前会被解析，但尚未接入 Gateway 限流 |

Provider 推断规则：

- `claude-*`、`claude.*` → `anthropic`；
- `gpt-*`、`o1*`、`o3*` → `openai`；
- `gemini-*`、`gemini.*` → `gemini`；
- 无法推断时尝试回退到已注册的 OpenAI Provider。

应用组合根目前只根据 API Key 注册 OpenAI 和 Anthropic。虽然源码中有 Gemini Provider 文件，但当前启动流程没有注册它。

### Judge

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `JUDGE_ENABLED` | `false` | 启动时启用 Judge |
| `JUDGE_MODEL` | 空 | Judge 模型；空值时使用 Lead 模型 |
| `JUDGE_MIN_SCORE` | `7` | 低于该分数时要求 Runner 重试 |
| `JUDGE_PROVIDER` | 按 Judge 模型推断 | 选择已注册 Provider |
| `JUDGE_API_KEY` | 空 | 已解析的预留字段 |
| `JUDGE_BASE_URL` | 空 | 已解析的预留字段 |

Judge 错误采用宽松回退：模型调用失败或响应无法解析时，不会永久阻塞主任务。

### Web

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `WEB_SEARCH_PROVIDER` | fallback chain | 可选 `tavily` 或 `brave` |
| `WEB_SEARCH_API_KEY` | 空 | Tavily/Brave API Key |
| `SEARXNG_URL` | 空 | 指定一个 SearXNG 实例 |
| `SEARXNG_INSTANCES` | 内置实例列表 | 逗号分隔的 SearXNG 实例 |
| `WEB_ALLOW_PRIVATE_IPS` | `false` | 允许部分私有网络目标；link-local/metadata 仍然阻止 |

搜索后端选择：

1. 配置了 `tavily` 或 `brave` 且有 Key 时使用指定后端；
2. 否则使用 `SEARXNG_URL`；
3. 否则依次尝试配置或内置的公共 SearXNG；
4. 最后回退 DuckDuckGo Lite。

### 安全和运行时

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `SNAPSHOT_ENABLED` | `false` | 值为 `1` 时启用危险工具前的 git snapshot |
| `HITL_NON_TTY_FALLBACK` | `reject` | 非 TTY 审批回退；设为 `approve` 可自动批准 |
| `MCP_SERVERS` | 空 | 启动时自动连接的 MCP server JSON 数组 |
| `XDG_CONFIG_HOME` | 空 | 默认状态根目录 |
| `HOME` | 系统值 | `XDG_CONFIG_HOME` 未设置时使用 |

## 架构设计

### 总体分层

```text
┌─────────────────────────────────────────────────────────────┐
│ cmd/agent                                                   │
│ CLI 参数、Readline、REPL、Session 切换                       │
└──────────────────────────┬──────────────────────────────────┘
                           │
┌──────────────────────────▼──────────────────────────────────┐
│ internal/application                                       │
│ Application 进程级组合根 + SessionRuntime 会话级组合根       │
└──────────────┬──────────────────────────────┬───────────────┘
               │                              │
┌──────────────▼──────────────┐  ┌────────────▼──────────────┐
│ internal/agent              │  │ internal/model            │
│ Runner / Subagent / Team    │  │ Gateway / Provider        │
│ Plan / Reflect / Compact    │  │ Retry / Stream / Throttle │
│ Judge / Snapshot / Lesson   │  │ Usage                     │
└──────────────┬──────────────┘  └────────────┬──────────────┘
               │                              │
               │ Tool Calls                   │ LLM API
┌──────────────▼───────────────────────────────▼──────────────┐
│ internal/tool                                               │
│ ToolCatalog → Executor → domain handlers                    │
└──────────────┬──────────────────────────────────────────────┘
               │
┌──────────────▼──────────────────────────────────────────────┐
│ 文件 / Shell / Task / Memory / Team / Web / MCP / Skill     │
└─────────────────────────────────────────────────────────────┘

横切层：
security · hitlaudit · event · history · session · store
```

### 生命周期边界

项目把对象分成三种主要生命周期。

#### 进程级

由 `application.Application` 持有：

- 不可变配置快照；
- Provider Registry；
- Model Gateway；
- Session Repository；
- 当前活动 `SessionRuntime` 引用。

`Application` 不保存每次 LLM 调用的可变状态。

#### 会话级

每次 `Application.Build` 都重新创建：

- 独立 `ToolCatalog`；
- `SessionRuntime` 上下文和取消函数；
- HITL Manager 和审批姿态；
- 权限实例和 diff preview；
- History Store、Usage Tracker、Decision Log；
- Task Service 和 Todo Manager；
- MCP Manager；
- Background Supervisor；
- Message Bus、Protocol Store 和 Teammate Manager；
- Worktree Service；
- Web Service；
- Runner、Subagent Runner 和 Judge。

会话切换时，MCP 工具和其他动态注册不会泄漏到新 session。

#### 调用级

每次用户请求或工具调用创建：

- trace ID；
- Runner 轮次状态；
- tool timeout context；
- 带 `Context` 的 `ToolScope` 副本；
- 结构化 `Result` 和事件。

这使 Ctrl-C、工具超时和 Session 关闭可以沿 `context.Context` 向下传播。

### Application 组合根

`internal/application/application.go` 是最重要的装配入口：

1. `Application.New` 加载配置、注册 Provider、创建 Gateway 和 Session Repository；
2. `Application.Build` 选择或创建 session；
3. 为 session 创建专属 Tool Catalog；
4. 创建任务、记忆、权限、MCP、Web、团队和后台服务；
5. `SessionRuntime.BuildRunner` 注册 39 个内置工具；
6. MCP 工具在内置工具之后增量注册；
7. 组合系统提示词；
8. 创建 Lead Runner、Explore Subagent 和 Teammate Manager；
9. 注册 history、worktree、background、MCP、team 和日志关闭钩子。

`SessionRuntime.Close` 先取消上下文，再按逆序执行关闭钩子。

### Runner

`internal/agent/runner.go` 是统一 Agent Loop。Lead、Explore 和 Teammate 使用相同的基础执行模型，但通过 `Profile` 和 `ToolScope` 获得不同能力。

Runner 每个用户回合会：

1. 重置轮次、失败、重复调用和 token 统计；
2. 根据用户请求召回相关长期记忆；
3. 评估规划门控；
4. 根据上下文预算执行 micro-compaction 或 LLM summary；
5. 通过 Gateway 流式调用模型；
6. 顺序执行模型返回的 tool calls；
7. 记录结构化工具结果、事件和 Usage；
8. 在失败、重复或卡住时注入反思；
9. 必要时触发 lesson 或 Judge；
10. 返回 `TurnOutcome` 给 REPL。

### Model Gateway

`internal/model.Gateway` 为所有角色提供统一模型入口：

- `Call`：非流式调用；
- `Stream`：流式调用；
- 按 Lead、Subagent、Teammate、Judge 选择 Provider；
- 统一重试、超时、限流和 Usage 回调；
- Provider SDK 类型转换成 `internal/llm` 中立类型。

当前应用默认使用一个主 Provider；Judge 可以选择已注册的另一个 Provider。角色级并发由 `RoleThrottle` 控制。

### Tool Catalog

`ToolCatalog` 使用不可变 snapshot 加读写锁：

- `RegisterAll` 原子替换整个目录；
- `Register` 增量加入 MCP 工具；
- `Subset` 为 Explore 构造只读工具子集；
- `Order` 保存稳定注册顺序。

工具 schema 位于每次 LLM 请求的前部。稳定顺序可以避免 Go map 随机迭代破坏 OpenAI/Anthropic 的 prompt-prefix cache。`builtin_order_test.go` 用 golden test 锁定内置工具顺序。

### 事件与可观测性

Runner、Subagent 和 Teammate 共享 `event.MultiSink`：

- `ConsoleSink`：终端事件；
- `AuditSink`：审批和工具审计；
- `UsageSink`：token 使用信息；
- `SessionLogSink`：写入 session 级 JSONL 日志。

工具执行结果还会写入 `decisions.jsonl`，模型 Usage 写入 `usage.jsonl`。

## 工具系统

### ToolDefinition

每个工具由完整的 `ToolDefinition` 描述：

- `Name`、`Description`；
- JSON Schema；
- `RiskLevel`；
- `Effects`；
- Handler；
- 可选 mutation preview；
- 可选专属 timeout；
- snapshot policy 元数据。

工具返回结构化 `Result`，状态包括：

```text
succeeded
failed
denied
rejected
modified
timeout
cancelled
invalid_arguments
unavailable
```

### 39 个内置工具

#### 文件与 Shell

| 工具 | 作用 |
|---|---|
| `bash` | 在工作目录运行前台命令，固定 120 秒超时 |
| `read_file` | 按偏移和行数读取文件 |
| `write_file` | 创建或覆盖文件 |
| `edit_file` | 精确字符串替换 |
| `delete_file` | 删除文件 |
| `insert_file` | 在指定位置插入内容 |
| `list_dir` | 列出目录 |
| `search_file` | 按名称模式搜索文件 |
| `search_content` | 按文本或正则搜索内容 |
| `background_run` | 启动后台 Shell；未指定正数超时时使用 120 秒 |
| `check_background` | 查询后台任务 |

`write_file`、`edit_file`、`insert_file` 和 `delete_file` 提供变更预览。`bash` 与 `background_run` 都经过相同的 `BashPolicy`。

#### Todo 与任务 DAG

```text
TodoWrite
task_create
task_list
task_update
task_get
task_add_dep
task_remove_dep
task_ready
task_dag
claim_task
```

#### 长期记忆

```text
memory_write
memory_search
memory_delete
memory_stats
session_save_memory
```

#### 团队与协议

```text
spawn_teammate
list_teammates
send_message
read_inbox
broadcast
shutdown_request
plan_approval
submit_plan
```

#### 上下文、技能和网络

```text
compress
load_skill
web_fetch
web_search
explore
```

MCP 工具不是固定内置工具。发现后使用以下名称动态追加：

```text
mcp__<server-name>__<tool-name>
```

### Executor 执行管线

所有 Lead 工具调用进入同一个 `tool.Executor`：

1. 检查参数是否为完整 JSON；
2. 从当前 Tool Catalog snapshot 解析定义和 Handler；
3. 根据 `Effects` 检查 `ToolScope` 能力；
4. 对显式 `AllowedRoots` 执行路径范围检查；
5. 在 mutation 前生成 diff preview；
6. 执行 HITL 审批；
7. 执行 scope 级 approval/network policy；
8. 设置工具专属或默认超时；
9. 用带取消上下文的 `ToolScope` 调用 Handler；
10. 捕获 panic 并转换成结构化失败；
11. 对成功输出进行 secret sanitization；
12. 记录 duration 和 decision。

文件 Handler 还会调用 `SecurePath`；Shell Handler 还会调用 `BashPolicy`；Web Client 在真正 dial 前再次检查目标 IP。

## 安全与审批

### 能力模型

`ToolScope` 为不同 Agent 声明：

```text
CanRead
CanWrite
CanExecute
CanNetwork
CanTeam
CanMemory
```

工具通过 `Effects` 声明需要的能力。Explore 没有写、团队和记忆能力；Teammate 在计划获批前没有写能力。

### HITL 模式

| 模式 | 当前行为 |
|---|---|
| `safe-only` | 自动通过低风险 review，对高风险 review 交互确认 |
| `interactive` | 对被 `NeedsReview` 判定需要审查的操作交互确认 |
| `auto-approve` | 自动通过所有 HITL review |
| `auto-reject` | 自动拒绝所有 HITL review |
| `notify-only` | 显示 review 信息后继续 |

HITL 当前通过工具名、Shell 命令内容和关键路径判断是否需要 review。`interactive` 不代表每一个只读工具都会弹窗。

文件 mutation 在 HITL 开启、非 `auto-approve` 且 diff preview 开启时，会进入逐块预览确认。

非 TTY 环境默认拒绝交互审批。确有需要时可以：

```bash
export HITL_NON_TTY_FALLBACK=approve
```

这会扩大自动执行范围，CI 或无人值守环境应谨慎使用。

### `/approve` 与 `/hitl`

`/approve` 是常用预设，同时同步 HITL 模式与 diff preview 姿态：

| 预设 | HITL 模式 | Diff preview |
|---|---|---|
| `off` | `interactive` | 开启 |
| `safe` | `safe-only` | 开启 |
| `danger` | `auto-approve` | 跳过 |

`/hitl off` 只关闭 HITL 提示，不会关闭 Bash hard deny、路径沙箱或 Web SSRF 防护。

### 权限文件

权限文件位于项目状态根目录：

```text
<project-state>/permissions.json
```

推荐格式：

```json
{
  "rules": [
    {"tool": "bash", "pattern": "git status*", "level": "allow"},
    {"tool": "bash", "pattern": "git push*", "level": "confirm"},
    {"tool": "bash", "pattern": "rm *", "level": "block"}
  ]
}
```

也兼容裸数组：

```json
[
  {"tool": "bash", "pattern": "rm *", "level": "block"}
]
```

兼容字段：

- `level`: `allow`、`confirm`、`block`；
- `action`: `allow`、`ask`、`deny`，加载时归一化；
- `tool` 和 `pattern` 支持 `*`、`?`；
- 规则按顺序匹配，第一条命中规则生效。

**当前权限执行范围需要特别注意：** `Permissions` 实例目前注入 `bash` 和 `background_run` 的 BashPolicy。其他工具规则可以被加载和显示，但通用 Executor 尚未根据 `permissions.json` 对所有工具做 allow/confirm/block。不要把 `mcp__*` 或文件工具规则误认为已经形成强制边界。

在当前 Shell Handler 中，`block` 会直接拒绝命中的普通命令；`allow` 不会绕过 hard deny；`confirm` 返回的提示标记不会直接传入 HITL，HITL 会用自己的 Shell 分类再次判断。因此，自定义 `confirm` 目前不能单独保证弹出审批框。

Bash hard deny 和内置 confirm pattern 在自定义权限之前执行，用户规则不能覆盖 hard deny。

### 路径安全

文件工具使用 `security.SecurePath`：

- 路径相对 `ToolScope.Workdir` 解析；
- 阻止 `..` 目录穿越；
- 对已存在路径解析 symlink 后再次检查；
- 读操作要求目标存在；
- 写操作允许新文件，但仍限制在工作目录内。

Explore 的工作目录是调用者工作区；Teammate 的工作目录是独立 worktree。

### Shell 安全

Shell 工具最终使用：

```text
sh -c <command>
```

执行前经过：

- 基础命令 allowlist；
- 永久拒绝的危险正则；
- 需要确认的命令模式；
- session 级 Bash 权限规则；
- HITL review；
- 120 秒默认超时；
- 输出脱敏。

这些机制不是操作系统级沙箱。处理不可信仓库或模型时，应在容器、虚拟机或专用低权限用户中运行。

### Web 与 SSRF

安全 HTTP Client：

- 只允许 HTTP/HTTPS；
- 最多 5 次 redirect；
- 限制响应大小；
- DNS 解析后、真正连接前校验 IP；
- 默认拒绝 loopback、RFC1918、IPv6 ULA 和额外内部网段；
- link-local、云 metadata 网段和 `0.0.0.0/8` 始终拒绝。

`WEB_ALLOW_PRIVATE_IPS=1` 只放开可配置的私有网段，不放开 link-local/metadata。

### Snapshot

设置：

```bash
export SNAPSHOT_ENABLED=1
```

Runner 会对文件写入、删除、Shell 和后台 Shell 等工具尝试 `git stash create --include-untracked`。工具失败时使用 `git read-tree -u --reset` 恢复 snapshot。

Snapshot 只在 Git 仓库中生效，并且本身会操作工作树。重要修改仍应先提交或备份。

### 安全边界

请把以下内容视为不可信输入：

- 工作区代码和文档；
- Web 搜索结果；
- MCP server 返回值；
- Tool 输出；
- Teammate 消息；
- 工作区技能文件。

不要把真实凭据写入仓库、`.mcp.json`、`permissions.json`、Prompt、Skill 或日志。

## 多 Agent 设计

### Lead Agent

Lead 拥有完整工具目录，并由主 REPL 驱动。它负责理解用户目标、规划、执行工具、汇总结果和请求人工批准。

### Explore Subagent

`explore` 工具创建隔离上下文的只读 Agent。它只看到以下工具：

```text
bash
read_file
list_dir
search_file
search_content
```

`web_fetch` 类型的 Subagent 会额外获得 `web_fetch` 和网络能力。

Explore 的特点：

- `CanWrite=false`；
- `CanTeam=false`；
- `CanMemory=false`；
- 有独立轮次和压缩预算；
- 工具输出会额外截断；
- Shell 仍受 BashPolicy 和 HITL 约束。

### Teammate

Teammate 是长期运行的协作 Agent：

- 有独立名称、角色和消息上下文；
- 可以领取任务并使用任务 DAG；
- 可以收发消息；
- 在 WORK/IDLE 阶段间切换；
- 写操作前需要通过 `submit_plan` / `plan_approval`；
- 必须成功获取独立 git worktree 才能启动。

创建：

```text
/team spawn reviewer review 检查认证模块并汇报风险
```

Worktree 从当前仓库 `HEAD` 创建，因此主工作区未提交的修改不会自动出现在 teammate worktree 中。需要共享的基础修改应先提交。

Worktree 获取失败时系统 fail closed：不会降级到与 Lead 共享主目录。

## MCP 集成

### 工作方式

MCP Manager 通过 stdio 启动外部子进程：

1. 启动 server；
2. 发送 `initialize`；
3. 调用 `tools/list`；
4. 把工具转换为 `ToolDefinition`；
5. 增量注册到当前 session 的 Tool Catalog；
6. 用 `mcp__server__tool` 形式执行 `tools/call`。

MCP stdio JSON-RPC 请求串行执行。动态工具默认超时 30 秒，并使用当前工具调用上下文。调用取消或超时时，Client 会关闭管道并停止对应 MCP 子进程，避免遗留阻塞读取。

### `.mcp.json`

在工作目录创建：

```json
{
  "servers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
    }
  }
}
```

也支持数组格式：

```json
[
  {
    "name": "filesystem",
    "command": "npx",
    "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
  }
]
```

工作区配置不会自动启动，而是进入 pending：

```text
/mcp pending
/mcp approve filesystem
```

### `MCP_SERVERS`

环境变量中的配置在启动时自动连接：

```bash
export MCP_SERVERS='[
  {
    "name": "filesystem",
    "command": "npx",
    "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
  }
]'
```

每个 MCP 子进程继承父进程环境。配置内的 `env` 值按字面量传递，不执行 `${VAR}` 展开。敏感 Token 更适合预先导出到父进程环境，并让 server 自行读取。

### MCP 安全提示

- `.mcp.json` 可以启动任意本地命令，应先审查；
- MCP 工具定义会标记为 `RiskDanger` 和 `EffectNetworkAccess`；
- 副作用还会根据工具名和描述启发式推断；
- 当前 HITL 的 `NeedsReview` 主要按内置工具名判断，不能只依赖 MCP 的 `RiskDanger` 元数据形成审批边界；
- 当前通用 `permissions.json` 也尚未强制覆盖 MCP 工具；
- 对不可信 MCP server 应使用容器、最小权限环境或直接不连接。

## 会话、历史与持久化

### 状态根目录

```text
$XDG_CONFIG_HOME/go-code-agent/<project-basename>/
```

或：

```text
$HOME/.config/go-code-agent/<project-basename>/
```

项目标识当前只使用工作目录 basename。两个不同路径但同名的项目会共享默认状态目录；这种情况应通过 `--data-dir` 隔离。

### 典型数据结构

```text
<project-state>/
├── sessions.json
├── permissions.json
├── MEMORY.md
├── daily/
│   └── YYYY-MM-DD.jsonl
└── sessions/
    └── <session-id>/
        ├── meta.json
        ├── history/
        │   └── history.jsonl
        ├── transcripts/
        │   └── transcript_<unix>.jsonl
        ├── tasks/
        │   ├── task_<id>.json
        │   └── dag_edges.json
        ├── team/
        │   ├── config.json
        │   └── inbox/
        │       └── <agent-id>.jsonl
        ├── worktrees/
        │   └── <teammate-name>/
        ├── session.log
        ├── usage.jsonl
        └── decisions.jsonl
```

部分文件和目录只会在对应能力首次使用后创建。

### 历史

`history/history.jsonl` 是 append-only 消息日志，保存：

- user message；
- assistant text 和 tool calls；
- tool result；
- compression checkpoint。

恢复时会重新构造系统消息，并清理：

- 没有对应 call 的孤立 tool result；
- 没有完整 tool result 的 assistant tool-call 块；
- 末尾没有 assistant 响应的 user message。

这可以降低中断后重复执行未完成请求的风险。

### 上下文压缩

压缩分两层：

- **MicroCompact**：把较早的大型 Tool Result 替换为占位符；
- **AutoCompact**：让 LLM 总结旧消息前缀，保留最近消息原文。

执行 AutoCompact 前会把完整消息保存到 session 的 `transcripts/`，并向历史写入 checkpoint。

### 长期记忆

记忆是项目级、跨 session 的：

- `MEMORY.md`：evergreen 记忆；
- `daily/YYYY-MM-DD.jsonl`：结构化每日记忆。

每日记忆默认保留 90 天，按分类和 token 集合 Jaccard 相似度去重。Runner 在每个用户回合开始时搜索相关记忆并注入上下文。

### Task 与 Todo

- `TodoManager`：当前 session runtime 内的短期 checklist，不写磁盘；
- `task.Service`：持久化任务、状态和 DAG 依赖；
- Teammate 可以通过 `claim_task` 领取 ready task。

### Usage 与审计

- `usage.jsonl`：Provider、角色、模型、trace 和 token 使用；
- `decisions.jsonl`：工具名、结果状态和原因；
- `session.log`：结构化 Agent 事件，长输出会截断。

这些文件可能包含源代码、命令输出和外部响应，应按敏感数据保护。

## 目录设计

```text
go-code-agent/
├── cmd/
│   ├── agent/
│   │   ├── main.go                  # CLI 入口和 Application 生命周期
│   │   └── repl.go                  # 交互循环和斜杠命令
│   └── eval/
│       └── main.go                  # 离线/在线评估入口
├── internal/
│   ├── agent/                       # Runner 和高级 Agent 行为
│   │   ├── runner.go                # 统一多轮 Agent Loop
│   │   ├── subagent.go              # Explore 隔离执行
│   │   ├── teammate.go              # Teammate WORK/IDLE 生命周期
│   │   ├── compression.go           # 上下文压缩与 checkpoint
│   │   ├── reflection.go            # 失败和卡住反思
│   │   ├── judge.go                 # LLM-as-Judge
│   │   ├── plan.go                  # 规划门控
│   │   ├── snapshot.go              # git snapshot/rollback
│   │   ├── lesson.go                # 自动经验写入
│   │   ├── system_prompt.go         # 系统提示词组装
│   │   ├── decisions.go             # 工具决策日志
│   │   └── usage.go                 # Usage 持久化
│   ├── application/
│   │   └── application.go           # 进程和 session 组合根
│   ├── background/
│   │   └── supervisor.go            # 后台命令管理
│   ├── config/
│   │   └── config.go                # 环境配置和运行常量
│   ├── eval/                        # 回归任务、Harness 和 mock executor
│   ├── event/                       # 事件模型与 Console/Audit/Usage/Log Sink
│   ├── history/                     # JSONL 对话历史和恢复清理
│   ├── hitlaudit/                   # HITL 模式、审批 UI 和 Executor Adapter
│   ├── llm/                         # Provider 无关消息、ToolCall、Usage 类型
│   ├── logging/                     # 进程日志辅助
│   ├── mcp/
│   │   ├── client.go                # stdio JSON-RPC Client
│   │   └── manager.go               # server 生命周期和动态工具注册
│   ├── memory/                      # Evergreen/Daily 记忆、搜索和 backfill
│   ├── model/
│   │   ├── gateway.go               # 统一模型调用入口
│   │   ├── client.go                # 重试和超时
│   │   ├── sink.go                  # 流式输出
│   │   └── provider/                # OpenAI/Anthropic/Gemini adapters
│   ├── prompt/
│   │   ├── loader.go                # go:embed 模板注册
│   │   └── templates/               # 系统、Judge、规划等模板文件
│   ├── security/
│   │   ├── security.go              # SecurePath、Approval、BashPolicy
│   │   ├── permissions.go           # permissions.json
│   │   ├── diff_preview.go          # 变更预览
│   │   ├── ssrf.go                  # 出站 IP 策略
│   │   └── audit.go                 # 输出脱敏
│   ├── session/                     # Session 元数据和索引
│   ├── skill/                       # SKILL.md 递归加载
│   ├── store/                       # 原子文件写入
│   ├── task/                        # Todo、任务 CRUD 和 DAG
│   ├── team/                        # Message Bus 和协作协议
│   ├── tool/
│   │   ├── definition.go            # Result、Definition、Scope、Catalog
│   │   ├── executor.go              # 统一执行管线
│   │   ├── builtin.go               # 稳定顺序的工具组装
│   │   ├── builtin_deps.go          # Handler session 依赖
│   │   ├── handler_fs_read.go       # 只读文件工具
│   │   ├── handler_fs_write.go      # 文件 mutation 工具
│   │   ├── handler_shell.go         # Bash 和后台命令
│   │   ├── handler_task.go          # Todo 和任务
│   │   ├── handler_memory.go        # 长期记忆
│   │   ├── handler_team.go          # Teammate 消息
│   │   ├── handler_protocol.go      # 团队协议
│   │   ├── handler_web.go           # Web 和 Explore
│   │   └── handler_meta.go          # 压缩和 Skill
│   ├── utils/                       # 终端、截断和进程平台差异
│   ├── web/                         # Fetch、Search、HTML 和安全 Client
│   └── worktree/                    # git worktree 创建和清理
├── prompts/
│   ├── human_modify.md              # 人工修改反馈模板文件
│   └── human_reject.md              # 人工拒绝反馈模板文件
├── skills/
│   ├── agent-builder/SKILL.md
│   ├── code-review/SKILL.md
│   ├── judge/SKILL.md
│   ├── pdf/SKILL.md
│   └── skill_format.md
├── go.mod
└── go.sum
```

### 目录职责原则

- `cmd/` 只处理进程入口和用户交互，不承载工具业务逻辑；
- `internal/application` 只负责装配和生命周期；
- `internal/agent` 负责策略，不直接实现文件或网络细节；
- `internal/tool` 负责能力描述和统一执行；
- 具体业务服务放在独立 package，通过接口注入 Handler；
- `internal/security` 与 `internal/hitlaudit` 是横切层；
- session 数据不放进进程全局单例；
- 新的动态工具必须注册到当前 session catalog，而不是共享 catalog。

## 开发与扩展

### 添加内置工具

1. 选择合适的 `internal/tool/handler_*.go`；
2. 定义完整 JSON Schema；
3. 明确 `RiskLevel` 和 `Effects`；
4. 为长操作设置 `Timeout`；
5. 文件 mutation 提供 `Preview`；
6. Handler 从 `ToolScope` 和 `builtinDeps` 获取上下文；
7. 使用 `Succeeded`、`Failed`、`Denied` 等结构化结果；
8. 把工具加入 `BuiltinTools`，不要随意改变已有顺序；
9. 更新 `builtin_order_test.go`；
10. 增加参数、能力、安全、取消和错误测试。

Handler 内的外部调用应使用 `scope.Context`，以便工具 timeout 和 Ctrl-C 真正停止底层操作。

### 添加 Provider

1. 实现 `model.Provider`；
2. 将 SDK 消息映射到 `internal/llm`；
3. 支持普通调用和流式调用；
4. 正确合并流式 tool-call delta；
5. 映射 Usage 和 Provider 错误；
6. 在 `application.New` 注册 Provider；
7. 如有需要，更新 `provider.inferName`；
8. 为文本、工具调用、流式、Usage、限流和错误响应增加测试。

仅增加 `internal/model/provider/*.go` 文件并不会自动启用 Provider，组合根必须显式注册。

### 添加 session 服务

1. 在 `Application.Build` 创建服务；
2. 通过 `RunnerParams` 注入；
3. 避免保存 `Application` 的循环引用；
4. 如果有 goroutine、文件或子进程，在 `SessionRuntime` 注册关闭钩子；
5. 让长操作继承 `rt.Ctx`；
6. 为 session 切换和重复关闭增加测试。

### 添加 REPL 命令

命令位于 `cmd/agent/repl.go` 的 `handleCommand`。新增命令时：

- 更新 `/help`；
- 明确命令是否修改 session；
- 长操作使用传入的 `ctx`，不要无条件使用 `context.Background()`；
- session 切换通过设置下一次 `BuildOptions` 完成；
- 同步更新 README。

### Skill

工作区技能目录：

```text
<workdir>/skills/**/SKILL.md
```

技能名取 `SKILL.md` 的父目录名。Session 构建时会把所有技能拼入系统提示词，也可以由 `load_skill` 按名称读取。

技能属于工作区输入，加载第三方项目时应审查其内容。

### Prompt

Prompt 文件位于 `internal/prompt/templates/`，使用 `go:embed` 编译进二进制。修改后需要重新编译。

注意：模板文件存在不代表已经注册到 `prompt.Loader`。新增模板时必须同时：

1. 添加 `//go:embed` 变量；
2. 在 `NewLoader` 的 map 中注册名称；
3. 为调用方增加非空模板测试。

## 测试与评估

### 常用开发命令

```bash
# 格式化
go fmt ./...

# 静态检查
go vet ./...

# 编译
go build ./...

# 单元测试
go test ./...

# 竞态检测
go test -race ./...
```

### 重点测试

- `internal/tool/*_test.go`：Catalog 顺序、Executor、安全管线、上下文取消；
- `internal/security/*_test.go`：路径、Bash、Permissions、SSRF；
- `internal/agent/*_test.go`：压缩、反思、Runner 控制、Subagent/Teammate 沙箱；
- `internal/mcp/mcp_test.go`：动态注册、超时、取消和 RPC framing；
- `internal/history/history_test.go`：中断历史恢复；
- `internal/task/*_test.go`：Todo、任务和 DAG；
- `internal/web/*_test.go`：搜索配置和 Fetch；
- `internal/model/provider/*_test.go`：DSML、Usage 和 Provider 映射。

### 离线评估

默认使用 mock，不需要真实 API Key：

```bash
go run ./cmd/eval
```

参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-live` | `false` | 使用真实 LLM |
| `-model` | 空 | live 模式模型 |
| `-v`、`-verbose` | `false` | 详细输出 |
| `-timeout` | `5m` | 单任务超时 |
| `-category` | 空 | 过滤任务类别 |
| `-task` | 空 | 过滤任务名 |
| `-output` | 空 | 写 JSON 结果 |
| `-baseline-out` | 空 | 写 baseline JSON |

示例：

```bash
go run ./cmd/eval -category tools -v
go run ./cmd/eval -task file_edit -output /tmp/eval.json
go run ./cmd/eval -live -model gpt-4o -timeout 10m
```

## 故障排查

### 启动时报 `no LLM provider available`

检查：

```bash
env | grep -E 'MODEL_ID|LLM_PROVIDER|OPENAI|ANTHROPIC'
```

确认：

- 至少配置一个 API Key；
- `LLM_PROVIDER` 是 `openai` 或 `anthropic`；
- 模型 ID 与 Provider 匹配；
- OpenAI-compatible 服务的 Base URL 正确。

### 输入后长时间没有文本

可能正在：

- 等待 LLM 首个 stream event；
- 生成 tool-call 参数；
- 等待 HITL；
- 执行 Shell、Web 或 MCP；
- 压缩上下文；
- 执行 Judge。

查看终端事件和：

```text
<session-dir>/session.log
```

需要终止时按一次 `Ctrl-C` 进行关闭。

### 文件操作被拒绝

检查：

- 路径是否在 `--workdir` 内；
- 是否通过 symlink 指向目录外；
- Agent profile 是否有 `CanWrite`；
- Teammate 是否已经提交并通过计划；
- diff preview 是否被拒绝；
- 当前 HITL 模式和 Bash/权限规则。

### `/approve danger` 后仍被阻止

`danger` 只自动通过 HITL review 和跳过 diff preview。它不会绕过：

- `SecurePath`；
- capability gate；
- Bash hard deny；
- SSRF；
- 工具参数校验；
- Handler 自身错误。

### MCP server 无法启动

检查：

- `command` 是否在 PATH；
- `.mcp.json` 是否是合法 JSON；
- server 是否实现 stdio MCP；
- `/mcp pending` 是否仍在等待批准；
- 环境变量是否真的由子进程读取；
- 最近一次调用是否超时并停止了 Client。

超时后可以先：

```text
/mcp disconnect <name>
/mcp connect <name> <command> [args...]
```

### Teammate 无法创建

Teammate 必须创建 git worktree。检查：

```bash
git rev-parse --is-inside-work-tree
git worktree list
git status
```

确认目标名称没有残留 worktree/branch 冲突，并且主仓库 `HEAD` 包含 teammate 需要的基础修改。

### Web 请求被阻止

目标可能解析到 loopback、私有地址或 link-local。只有在受控环境中才设置：

```bash
export WEB_ALLOW_PRIVATE_IPS=1
```

云 metadata/link-local 仍然不会放开。

### 两个项目看到了相同 session

默认项目状态键只使用目录 basename。为同名项目提供不同 `--data-dir`：

```bash
./go-code-agent --workdir /repo/a/app --data-dir ~/.config/go-code-agent-a
./go-code-agent --workdir /repo/b/app --data-dir ~/.config/go-code-agent-b
```

## 当前限制

1. **不是 OS 沙箱**：Shell、MCP 和第三方工具仍可能产生真实副作用。
2. **Gemini 未接入组合根**：存在 Provider 源码，但启动流程不注册。
3. **Permissions 尚未统一进入 Executor**：当前强制执行主要覆盖 Bash/Background Bash。
4. **MCP 风险元数据与 HITL review 尚未完全统一**：第三方 MCP 应视为高信任边界。
5. **部分 Prompt 文件尚未在 Loader 注册**：新增或使用模板前需核对 `prompt.NewLoader`。
6. **QPS/Burst 配置尚未接入**：当前实际限流主要是 `LLM_MAX_CONCURRENCY`。
7. **项目状态键只用 basename**：同名目录需要手动隔离 `--data-dir`。
8. **Todo 不持久化**：持久工作应使用 `task_*` 和 DAG。
9. **Teammate 只看到 worktree 的 HEAD 基线**：主目录未提交修改不会自动同步。
10. **公共 SearXNG 不适合生产**：稳定性、隐私和可用性不可控。
11. **状态文件包含敏感内容**：历史、日志、Tool 输出和记忆都可能包含源代码或外部数据。
12. **当前仓库没有 LICENSE 文件**：公开分发或复用前应补充许可证。

## 许可证

当前仓库尚未提供 LICENSE。若计划公开发布、二次分发或作为依赖使用，请先选择许可证并核对第三方依赖许可。
