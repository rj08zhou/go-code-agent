# go-code-agent-refactor

一个使用 Go 编写的终端交互式自主编程 Agent。它通过 LLM 多轮决策和工具调用，帮助用户阅读代码、分析项目、修改文件、执行测试、管理任务，并支持子 Agent、团队协作、MCP、长期记忆、会话恢复和人工审批。

> 当前项目是一个 CLI/REPL 应用，不是 HTTP 服务，也不是 IDE 插件。

## 目录

- [项目定位](#项目定位)
- [核心能力](#核心能力)
- [架构概览](#架构概览)
- [运行流程](#运行流程)
- [快速开始](#快速开始)
- [命令行参数](#命令行参数)
- [环境变量](#环境变量)
- [交互命令](#交互命令)
- [工具清单](#工具清单)
- [审批与安全](#审批与安全)
- [会话、历史和持久化数据](#会话历史和持久化数据)
- [MCP 集成](#mcp-集成)
- [多 Agent 与团队协作](#多-agent-与团队协作)
- [技能和提示词](#技能和提示词)
- [长期记忆](#长期记忆)
- [网络访问](#网络访问)
- [Judge 质量验证](#judge-质量验证)
- [离线评估](#离线评估)
- [项目结构](#项目结构)
- [开发指南](#开发指南)
- [故障排查](#故障排查)
- [已知限制](#已知限制)
- [安全建议](#安全建议)

## 项目定位

项目的核心思想是将 Agent 拆分为三个部分：

1. **模型**：负责理解用户意图、规划下一步、决定是否调用工具。
2. **能力**：通过统一的 Tool Catalog 暴露文件、Shell、任务、记忆、网络和协作能力。
3. **运行时约束**：通过上下文、权限、HITL 审批、超时、快照、限流和历史机制控制执行边界。

一次用户请求通常会经历以下循环：

```text
用户输入
   ↓
追加到会话历史
   ↓
LLM 推理
   ├─ 直接返回文本 → 输出给用户
   └─ 返回工具调用 → 权限/审批检查 → 执行工具
                              ↓
                         工具结果写回上下文
                              ↓
                         继续 LLM 推理
```

项目中的 Agent Loop 尽量保持通用，复杂能力通过模块化服务和工具注入，而不是在 REPL 中写死具体工作流。

## 核心能力

### 代码操作

- 读取文件和目录；
- 按文件名或内容搜索；
- 创建、编辑、插入和删除文件；
- 执行 Shell 命令；
- 生成和查看文件变更预览；
- 可选的 git snapshot 和失败回滚。

### Agent 编排

- Lead Agent：处理用户主请求；
- Explore Subagent：隔离上下文执行只读探索；
- Teammate：拥有独立角色和工作目录的长期协作 Agent；
- 后台任务：异步执行耗时命令；
- 任务 CRUD 和 DAG 依赖调度；
- Todo 状态跟踪；
- 连续失败、重复工具调用和卡住检测；
- 上下文压缩和会话 checkpoint。

### 模型和服务

- OpenAI API 及 OpenAI-compatible API；
- Anthropic API；
- Provider Registry 和统一 Gateway；
- 流式输出；
- 角色级并发限流；
- LLM 调用重试、超时和 Usage 记录；
- 可选 Judge 模型进行二次质量检查。

### 扩展能力

- MCP stdio 子进程集成；
- 项目级 `skills/` 技能加载；
- 长期记忆和每日 JSONL 记忆；
- `web_fetch` 和 `web_search`；
- 外部网络 SSRF 防护；
- 权限规则和工具决策审计。

## 架构概览

### 总体架构图

```text
┌──────────────────────────────────────────────────────────────────────────────┐
│                              用户 / 终端 REPL                                 │
│                         cmd/agent/repl.go                                     │
└──────────────────────────────────┬───────────────────────────────────────────┘
                                   │ 用户输入 / 斜杠命令 / Ctrl-C
                                   ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                         Application 组合根                                    │
│                    internal/application/application.go                      │
│                                                                              │
│  配置 · Session Repository · Provider Registry · Tool Catalog · 生命周期管理   │
└───────────────────────────┬──────────────────────────────┬───────────────────┘
                            │                              │
                            ▼                              ▼
┌──────────────────────────────────────┐  ┌───────────────────────────────────┐
│          SessionRuntime               │  │          Agent Runner             │
│  历史 / 任务 / 记忆 / MCP / 团队       │  │   Lead · Explore · Teammate       │
│  HITL / worktree / 后台任务           │  │   Loop · Compression · Judge      │
└──────────────────┬───────────────────┘  └──────────────────┬────────────────┘
                   │                                         │
                   │ 上下文、状态和资源                       │ LLM 请求 / 工具调用决策
                   │                                         ▼
                   │                          ┌────────────────────────────────┐
                   │                          │       Model Gateway            │
                   │                          │ 重试 · 超时 · 限流 · Usage       │
                   │                          └───────────────┬────────────────┘
                   │                                          │
                   │                                          ▼
                   │                          ┌────────────────────────────────┐
                   │                          │       Provider Registry         │
                   │                          │ OpenAI · Anthropic · Compatible │
                   │                          └───────────────┬────────────────┘
                   │                                          │ API / Streaming
                   │                                          ▼
                   │                          ┌────────────────────────────────┐
                   │                          │           LLM 服务              │
                   │                          └────────────────────────────────┘
                   │
                   ▼
┌──────────────────────────────────────────────────────────────────────────────┐
│                            Tool Executor                                       │
│ 参数解析 · 权限检查 · HITL 审批 · 超时 · Snapshot · 审计 · 输出脱敏            │
└──────────────┬─────────────────────┬─────────────────────┬───────────────────┘
               │                     │                     │
               ▼                     ▼                     ▼
      ┌────────────────┐    ┌────────────────┐    ┌─────────────────────────┐
      │ 文件 / Shell   │    │ 任务 / 记忆     │    │ Web / MCP / Team         │
      │ read/edit/bash │    │ Todo / DAG     │    │ fetch/search/协作         │
      └────────────────┘    └────────────────┘    └─────────────────────────┘
               │                     │                     │
               └─────────────────────┴─────────────────────┘
                                     │ 工具结果写回上下文
                                     └──────────────────────► Agent Runner

┌──────────────────────────────────────────────────────────────────────────────┐
│ 横切能力：SecurePath · BashPolicy · Permissions · SSRF · SecretsSanitizer     │
│ 持久化：History · Session · Memory · Task · Usage · Decision · Event Logs      │
└──────────────────────────────────────────────────────────────────────────────┘
```

架构图中的核心闭环是：`REPL → Runner → Gateway/Provider → LLM → Tool Executor → 工具结果 → Runner`。其中 `SessionRuntime` 提供会话级上下文和资源，安全模块横向约束工具执行，持久化模块负责跨请求和跨进程保存状态。

### 分层结构

```text
cmd/agent
  ├─ main.go              CLI 参数、应用生命周期、REPL 创建
  └─ repl.go              输入循环、斜杠命令、历史持久化

internal/application
  ├─ Application           进程级组合根
  ├─ SessionRuntime        单会话运行时和资源生命周期
  └─ BuildRunner           组装所有 session-scoped 服务

internal/agent
  ├─ Runner                Lead/Explore/Teammate Agent Loop
  ├─ SubagentRunner        隔离上下文的子 Agent
  ├─ TeammateManager       团队 Agent 生命周期
  ├─ Compression           上下文压缩
  ├─ Reflection            卡住/失败反思
  ├─ Judge                 LLM-as-Judge
  └─ Snapshot/Usage/Lesson 辅助模块

internal/model
  ├─ Gateway               Provider 统一入口、重试、限流、统计
  ├─ Provider              LLM 后端适配器
  └─ StreamSink            流式输出接口

internal/tool
  ├─ ToolDefinition        工具定义、Schema、风险和副作用
  ├─ ToolCatalog            原子化工具注册表
  ├─ ToolExecutor           统一执行管线
  └─ handlers.go            内置工具实现

internal/security
  ├─ SecurePath            工作目录和路径约束
  ├─ BashPolicy             Shell 命令策略
  ├─ Permissions            permissions.json 规则
  ├─ DiffPreview            文件变更预览
  ├─ SSRF 防护              出站网络限制
  └─ SecretsSanitizer       输出中的敏感信息清理
```

### 组合根

`internal/application/application.go` 是应用的组合根，负责把进程级和会话级对象连接起来：

- 加载 `internal/config` 配置；
- 创建 Provider Registry 和 `model.Gateway`；
- 创建全局 Tool Catalog；
- 创建 Session Repository；
- 为每个会话创建任务、记忆、MCP、团队、后台任务、worktree、HITL 和历史服务；
- 注册内置工具和动态 MCP 工具；
- 构造系统提示词；
- 创建 `agent.Runner` 和事件 Sink；
- 在关闭时逆序释放资源。

`Application` 保存项目级服务，`SessionRuntime` 保存单个会话的上下文、取消函数和关闭钩子，避免把每次调用的状态放到全局变量中。

### Provider 和模型选择

Provider 的选择顺序如下：

1. 如果设置了 `LLM_PROVIDER`，优先使用指定 Provider；
2. 否则根据模型名前缀推断：
   - `claude-*` 或 `claude.*` → `anthropic`；
   - `gpt-*`、`o1*`、`o3*` → `openai`；
   - `gemini-*` 或 `gemini.*` → `gemini`；
3. 如果仍无法推断，则回退到已注册的 `openai` Provider。

当前 `Application.New` 默认注册 OpenAI 和 Anthropic Provider。项目中虽然保留了 `gemini.go`，但当前应用组装流程没有注册 Gemini Provider，因此 Gemini 目前不应视为开箱即用的后端。

## 运行流程

### 启动阶段

`cmd/agent/main.go` 执行以下操作：

1. 解析命令行参数；
2. 确定工作目录和配置根目录；
3. 创建 `application.Application`；
4. 创建 readline 输入器；
5. 根据活动会话、指定会话或 `--new-session` 构造 `BuiltRunner`；
6. 打印模型、工作区、会话和 HITL 状态；
7. 进入 REPL。

### 一次普通请求

`cmd/agent/repl.go` 中的一次普通消息处理流程：

1. 从 `history.Store` 加载当前会话消息；
2. 读取用户输入；
3. 将用户消息追加到内存历史和磁盘历史；
4. 调用 `agent.Runner.Run`；
5. Runner 调用 Gateway 发起 LLM 请求；
6. 如果模型返回工具调用，交给 `tool.Executor`；
7. Executor 执行权限检查、审批、参数解析、超时、脱敏和审计；
8. 把 Assistant 消息和 Tool 消息追加到历史；
9. 必要时写入 checkpoint；
10. 输出错误并同步历史。

### Ctrl-C 行为

第一次 `Ctrl-C` 会取消当前运行上下文，给正在进行的 LLM 调用、工具调用和子 Agent 发送取消信号；第二次 `Ctrl-C` 强制退出。

用户消息是在请求开始前写入历史的。启动时加载历史会清理尾部没有对应 Agent 响应的孤儿 user 消息，避免上一次被中断的 prompt 在下次启动时自动重放。

## 快速开始

### 环境要求

- Go `1.25.3` 或兼容的较新版本；
- 一个可用的 LLM API Key；
- macOS、Linux 或其他支持 Go 和 Shell 的环境；
- 如果使用 MCP，需要额外安装对应的 MCP server 命令。

### 编译

当前仓库没有根目录 `Makefile`，使用 Go 命令直接编译：

```bash
git clone <repository-url>
cd go-code-agent-refactor

go mod download
go build -o bin/go-code-agent ./cmd/agent
go build -o bin/go-code-agent-eval ./cmd/eval
```

也可以直接运行：

```bash
go run ./cmd/agent
```

### OpenAI 或 OpenAI-compatible API

```bash
export OPENAI_API_KEY="your-api-key"
export MODEL_ID="gpt-4o"

go run ./cmd/agent
```

DeepSeek 等 OpenAI-compatible 服务可以显式指定 Provider 和 Base URL：

```bash
export LLM_PROVIDER="openai"
export OPENAI_API_KEY="your-api-key"
export OPENAI_BASE_URL="https://api.deepseek.com"
export MODEL_ID="deepseek-v4-flash"

go run ./cmd/agent
```

Base URL 应按照对应服务商的 OpenAI-compatible API 要求填写。不要把 API Key 写进代码、提交到 Git 或放入 `.mcp.json` 示例文件中。

### Anthropic API

```bash
export ANTHROPIC_API_KEY="your-api-key"
export MODEL_ID="claude-opus-4.7"

go run ./cmd/agent
```

### 指定工作区和状态目录

```bash
go run ./cmd/agent \
  --workdir /path/to/project \
  --data-dir /path/to/config
```

`--data-dir` 是配置根目录，项目状态实际保存到：

```text
<data-dir>/go-code-agent/<工作目录basename>/
```

默认配置根目录为 `$XDG_CONFIG_HOME`；如果未设置，则使用 `$HOME/.config`。

## 命令行参数

`cmd/agent` 支持以下参数：

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `--workdir` | 当前目录 | Agent 工作目录，也是文件工具的主要允许根目录 |
| `--data-dir` | `$XDG_CONFIG_HOME` 或 `$HOME/.config` | 持久化状态的配置根目录 |
| `--session` | 空 | 启动时恢复指定会话 ID |
| `--new-session` | `false` | 忽略当前活动会话，创建新会话 |
| `--human` | `false` | 启用人工审批，并以 interactive 模式启动 |
| `--human-mode` | `interactive` | 启动时设置 HITL 模式 |

示例：

```bash
# 使用指定会话
go run ./cmd/agent --session 20260721T102833-1230001

# 创建新会话并在安全模式启动
go run ./cmd/agent --new-session --human-mode safe-only

# 所有工具都要求人工确认
go run ./cmd/agent --human --human-mode interactive
```

注意：启动参数中的 `--human-mode` 使用 `interactive`、`auto-approve`、`auto-reject`、`notify-only` 或 `safe-only` 等模式名称；交互过程中也可以使用 `/approve` 命令切换常用的 `off`、`safe`、`danger` 模式。

## 环境变量

### 主 LLM

| 变量 | 默认值 | 说明 |
|---|---|---|
| `MODEL_ID` | `claude-opus-4.7` | 主模型 ID |
| `LLM_PROVIDER` | 自动推断 | 强制 Provider，例如 `openai`、`anthropic` |
| `OPENAI_API_KEY` | 空 | OpenAI 或 OpenAI-compatible API Key |
| `OPENAI_BASE_URL` | SDK 默认值 | OpenAI API Base URL |
| `ANTHROPIC_API_KEY` | 空 | Anthropic API Key |
| `ANTHROPIC_BASE_URL` | SDK 默认值 | Anthropic API Base URL |
| `CONTEXT_WINDOW_TOKENS` | 自动推断 | 覆盖模型上下文窗口大小 |
| `SNAPSHOT_ENABLED` | `false` | 设置为 `1` 时启用工具执行前的 git snapshot 策略 |

如果没有检测到 `OPENAI_API_KEY` 或 `ANTHROPIC_API_KEY`，启动时会打印警告；后续调用仍可能因没有可用 Provider 而失败。

### LLM 限流

| 变量 | 默认值 | 说明 |
|---|---:|---|
| `LLM_MAX_QPS` | `4.0` | LLM 请求速率限制 |
| `LLM_MAX_BURST` | `8` | 突发容量配置 |
| `LLM_MAX_CONCURRENCY` | `4` | 角色限流器的并发容量 |

### Judge

| 变量 | 默认值 | 说明 |
|---|---:|---|
| `JUDGE_ENABLED` | `false` | 是否启用 LLM 二次评估 |
| `JUDGE_MODEL` | 空 | Judge 模型；为空时使用主模型回退逻辑 |
| `JUDGE_MIN_SCORE` | `7` | 低于该分数时要求 Agent 重试 |
| `JUDGE_PROVIDER` | 自动推断 | Judge 使用的 Provider |
| `JUDGE_API_KEY` | 空 | Judge 专用 API Key |
| `JUDGE_BASE_URL` | 空 | Judge 专用 Base URL |

示例：

```bash
export JUDGE_ENABLED=1
export JUDGE_MODEL="claude-haiku-4.5"
export JUDGE_MIN_SCORE=7
```

Judge 的内部错误采用宽松回退策略，通常不会因为 Judge 本身异常而阻塞主 Agent。

### Web 搜索

| 变量 | 默认值 | 说明 |
|---|---|---|
| `WEB_SEARCH_PROVIDER` | fallback chain | `tavily` 或 `brave`；未配置时使用 SearXNG → DuckDuckGo |
| `WEB_SEARCH_API_KEY` | 空 | Tavily 或 Brave API Key |
| `SEARXNG_URL` | 空 | 指定一个 SearXNG 实例 |
| `SEARXNG_INSTANCES` | 内置公共实例 | 逗号分隔的 SearXNG 实例列表 |
| `WEB_ALLOW_PRIVATE_IPS` | `false` | 是否允许访问解析到私有/内部 IP 的主机 |

推荐生产环境显式配置可信的 SearXNG 实例，不要依赖公共实例。

### MCP

| 变量 | 说明 |
|---|---|
| `MCP_SERVERS` | JSON 数组，启动时自动启动其中的 MCP server |

示例：

```bash
export MCP_SERVERS='[
  {"name":"filesystem","command":"npx","args":["-y","@anthropic-ai/mcp-server-filesystem","."]}
]'
```

### 配置文件

以下文件位于项目状态目录或工作区目录：

- `<data-dir>/go-code-agent/<project>/permissions.json`：工具权限规则；
- `<workdir>/.mcp.json`：工作区 MCP server 配置，默认进入待审批列表；
- `<workdir>/skills/**/SKILL.md`：项目技能文件；
- `<workdir>`：Agent 的主要代码工作区。

## 交互命令

在 REPL 中输入 `/help` 可以查看基础命令。完整命令如下：

### 帮助和退出

```text
/help
/exit
/quit
```

### 任务和 Todo

```text
/tasks
/dag
/task clear
/task reset
```

- `/tasks`：显示 Todo 列表；
- `/dag`：显示持久化任务 DAG 和进度；
- `/task clear`：隐藏已完成任务；
- `/task reset`：删除当前任务数据并重新开始。

### 会话

```text
/session
/session list
/session new
/session switch <session-id>
/session rename <title>
/session archive
```

切换或创建会话会退出当前 REPL，并由顶层程序重新构造对应的 SessionRuntime。

### 审批和安全

```text
/approve
/approve off
/approve safe
/approve danger
/hitl
/hitl interactive
/hitl auto-approve
/hitl auto-reject
/hitl notify-only
/hitl safe-only
/permissions
/permissions reload
/security
/security test-bash <command>
/decisions
```

`/approve danger` 会自动批准包括危险工具在内的所有工具，并跳过 diff preview，只建议在完全信任工作区和模型行为时使用。

### MCP

```text
/mcp
/mcp pending
/mcp approve <name>
/mcp connect <name> <command> [args...]
/mcp disconnect <name>
```

### 团队和收件箱

```text
/team
/team spawn <name> <role> <prompt>
/team shutdown <name>
/team message <name> <content>
/team inbox
/inbox
```

### 网络、Judge 和运行状态

```text
/search <query>
/judge
/usage
/compact
/memory
```

- `/search`：直接调用 Web Search 服务；
- `/judge`：切换当前 Judge 开关；
- `/usage`：查看 LLM Usage；
- `/compact`：手动压缩当前对话；
- `/memory`：查看长期记忆统计。

## 工具清单

内置工具在 `internal/tool/handlers.go` 和 `internal/tool/handlers_helpers.go` 中注册。工具定义包含名称、描述、JSON Schema、风险等级、影响类型、超时、快照策略和 Handler。

### 文件和 Shell

| 工具 | 作用 | 默认风险 |
|---|---|---|
| `read_file` | 读取文件内容，可指定行偏移和行数 | Auto |
| `list_dir` | 列出目录 | Auto |
| `search_file` | 按文件名模式搜索 | Auto |
| `search_content` | 按内容或正则搜索 | Auto |
| `write_file` | 创建或覆盖文件 | Safe/Danger 取决于策略 |
| `edit_file` | 精确字符串替换编辑文件 | Safe/Danger 取决于策略 |
| `insert_file` | 在文件中插入内容 | Safe/Danger 取决于策略 |
| `delete_file` | 删除文件 | Danger |
| `bash` | 在工作目录执行 Shell 命令，硬超时 120 秒 | Danger |

`bash` 使用 `sh -c` 执行命令，因此必须把它视为高风险能力。命令会先经过 Bash Policy 检查，并受 HITL、权限和工具超时约束。

### 任务、记忆和技能

| 工具 | 作用 |
|---|---|
| `TodoWrite` | 更新短期 Todo 状态 |
| `task_create` | 创建持久化任务 |
| `task_get` | 获取任务详情 |
| `task_update` | 更新任务状态或内容 |
| `task_list` | 列出任务 |
| `task_add_dep` | 添加 DAG 依赖 |
| `task_remove_dep` | 移除 DAG 依赖 |
| `task_ready` | 查询当前可执行任务 |
| `task_dag` | 查看 DAG 拓扑 |
| `claim_task` | Teammate 领取任务 |
| `memory_write` | 写入长期记忆 |
| `memory_search` | 搜索长期记忆 |
| `memory_delete` | 删除匹配的记忆 |
| `memory_stats` | 查看记忆统计 |
| `session_save_memory` | 将当前会话摘要保存到长期记忆 |
| `load_skill` | 按名称加载一个技能文件 |

### 后台任务和协作

| 工具 | 作用 |
|---|---|
| `background_run` | 启动后台命令 |
| `check_background` | 查询后台任务 |
| `spawn_teammate` | 创建持久化 teammate |
| `list_teammates` | 列出 teammate |
| `send_message` | 向 teammate 发送消息 |
| `read_inbox` | 读取 Agent 收件箱 |
| `broadcast` | 广播消息 |
| `shutdown_request` | 请求关闭 teammate |
| `plan_approval` | 审批 teammate 计划 |
| `submit_plan` | 提交计划 |

### Web、MCP 和上下文

| 工具 | 作用 |
|---|---|
| `web_fetch` | 获取并提取一个网页内容 |
| `web_search` | 搜索网页 |
| `explore` | 启动隔离上下文的只读探索子 Agent |
| MCP 工具 | 以 `mcp__<server>__<tool>` 命名并动态注册 |
| `compress` | 请求压缩当前上下文 |

MCP 工具默认被标为高风险，并自动带有网络访问影响；实际工具效果会根据工具名和描述进行启发式推断。

## 审批与安全

### 风险等级

工具定义中包含以下风险等级：

- `RiskAuto`：只读或低风险操作；
- `RiskSafe`：通常可自动执行但会产生用户可见变更；
- `RiskInteractive`：需要用户确认；
- `RiskDanger`：可能破坏文件、执行命令、访问网络或修改外部状态。

工具还会声明副作用类型：

- 读文件；
- 写文件；
- 删除文件；
- 执行进程；
- 网络访问；
- 会话、记忆和团队状态修改。

### 默认模式

应用默认启用 HITL，并使用 `safe-only` 策略：

- 读取、搜索、任务等安全工具自动批准；
- 写入、编辑、删除、Shell 等危险操作需要确认；
- 文件写入可以先生成 diff preview；
- 工具决策会写入 session decision log。

### 三种常用模式

| 命令 | 行为 |
|---|---|
| `/approve off` | 每个工具都需要手动确认 |
| `/approve safe` | 安全工具自动批准，危险工具确认 |
| `/approve danger` | 所有工具自动批准，跳过 diff preview |

危险模式不是安全沙箱。启用前请确认当前工作区、API Key、Shell 环境和 MCP server 都是可信的。

### 路径安全

文件工具通过 `security.SecurePath` 解析路径，默认限制在工作目录及允许根目录内，并拒绝目录穿越和不符合权限的路径。

### Shell 安全

`bash` 工具使用以下机制：

- 固定的 120 秒命令超时；
- Bash Policy 白名单、拒绝规则和确认规则；
- 工具风险等级和 HITL 审批；
- 工具权限规则；
- 输出截断和敏感信息清理。

这不是完备的操作系统级沙箱。对于不可信模型或不可信项目，建议在容器、临时虚拟机或专用用户中运行。

### 权限文件

权限文件位置：

```text
<data-dir>/go-code-agent/<project>/permissions.json
```

格式：

```json
[
  {"tool": "read_file", "level": "allow"},
  {"tool": "bash", "level": "confirm"},
  {"tool": "delete_file", "level": "block"},
  {"tool": "mcp__*", "level": "block"}
]
```

其中 `level` 支持 `allow`、`confirm` 和 `block`。`pattern` 可用于匹配 MCP 工具或带参数的特定操作。修改后可使用 `/permissions reload` 重新加载。

## 会话、历史和持久化数据

默认状态目录如下：

```text
$XDG_CONFIG_HOME/go-code-agent/<project>/
# 或
$HOME/.config/go-code-agent/<project>/
```

典型结构：

```text
<project-state>/
├── sessions.json                 # 会话索引和 active_id
├── permissions.json              # 权限规则
├── MEMORY.md                     # 长期 evergreen 记忆
├── daily/                        # 每日 JSONL 记忆
│   └── 2026-01-01.jsonl
└── sessions/
    └── <session-id>/
        ├── meta.json             # 会话元数据
        ├── session.log           # 事件日志
        ├── usage.jsonl            # LLM 用量记录
        ├── decisions.jsonl        # 工具决策审计
        ├── history/
        │   └── history.jsonl     # 会话消息历史
        ├── tasks/
        │   ├── task_<id>.json
        │   └── dag_edges.json
        ├── team/
        │   └── inbox/
        └── worktrees/
```

部分目录只在对应能力被使用后创建。

### 历史恢复规则

历史存储的是中立的 LLM 消息结构，而不是终端显示文本。启动时会：

- 恢复系统消息和已完成的用户/助手/工具消息；
- 清理孤立 Tool 消息；
- 清理未完成的 Assistant Tool Call 块；
- 删除尾部没有 Assistant 响应的 user 消息；
- 继续使用清理后的历史向模型发起请求。

这可以避免网络中断或 `Ctrl-C` 后重复执行上一次未完成的 prompt。

### 数据备份

状态目录包含会话内容、工具输出、代码片段和可能的敏感信息。建议：

- 将状态目录加入个人备份，但不要无意中提交到公共仓库；
- 对包含敏感代码的项目设置合适的文件权限；
- API Key 只通过环境变量注入；
- 在共享机器上使用独立的 `--data-dir`。

## MCP 集成

MCP Manager 通过 stdio 启动外部子进程，发现其工具后将工具注册到统一 Tool Catalog 中。

### 工作区配置

在工作目录放置 `.mcp.json`：

```json
[
  {
    "name": "filesystem",
    "command": "npx",
    "args": ["-y", "@anthropic-ai/mcp-server-filesystem", "."],
    "env": {}
  }
]
```

工作区配置中的 server 默认进入 pending 列表，不会直接启动。启动 Agent 后查看并批准：

```text
/mcp pending
/mcp approve filesystem
```

### 环境变量配置

`MCP_SERVERS` 中的 server 会在启动时自动启动：

```json
[
  {
    "name": "github",
    "command": "npx",
    "args": ["-y", "@anthropic-ai/mcp-server-github"],
    "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"}
  }
]
```

当前实现会把环境配置传给子进程。敏感值应通过外部环境或安全的进程注入机制提供，不要把真实 Token 写入仓库文件。

### 工具命名

MCP 工具使用以下全限定名：

```text
mcp__<server-name>__<tool-name>
```

MCP 工具默认按照危险工具处理，并经过统一的 Tool Executor、权限、审批和审计管线。

## 多 Agent 与团队协作

### Explore Subagent

`explore` 用于只读、隔离上下文的代码探索。它适合：

- 跨多个文件查找实现；
- 分析大型项目结构；
- 查找某个功能的调用链；
- 避免把大量探索细节污染 Lead Agent 的主上下文。

Explore 子 Agent 有自己的角色、上下文和轮次限制，但共享受控的模型 Gateway 和工具目录。

### Teammate

Teammate 是持久化的协作 Agent，可以：

- 领取任务；
- 在独立角色和上下文中工作；
- 发送和接收消息；
- 使用任务 DAG；
- 可选地绑定独立 git worktree；
- 在完成或关闭时等待资源清理。

常用命令：

```text
/team spawn explorer explore 分析认证模块
/team message explorer 请汇报当前进度
/team inbox
/team shutdown explorer
```

Agent 也可以通过 `spawn_teammate`、`send_message`、`plan_approval` 等工具使用协作协议。

### Worktree

`internal/worktree` 为 teammate 提供工作树服务。启用时，每个 Agent 可以在独立 worktree 中修改代码，减少并发编辑冲突。会话关闭时由 SessionRuntime 统一清理。

## 技能和提示词

### 项目技能

`internal/skill/loader.go` 会递归扫描：

```text
<workdir>/skills/**/SKILL.md
```

当前仓库内包含以下技能目录：

- `skills/agent-builder/`：Agent 设计与构建指导；
- `skills/code-review/`：安全、正确性、性能和可维护性审查；
- `skills/judge/`：Judge 方法和配置说明；
- `skills/pdf/`：PDF 处理工作流；
- `skills/skill_format.md`：技能文件格式说明。

技能内容会被拼接到系统提示词中，也可以通过 `load_skill` 按名称加载。

### 内置提示词

`internal/prompt/loader.go` 使用 Go `embed` 嵌入以下模板：

- `system.md`：主系统提示词；
- `auto_lesson.md`：自动经验总结；
- `session_to_memory.md`：会话转长期记忆；
- `judge_system.md`：Judge 主提示词；
- `judge_critical.md`：低分时的关键反馈；
- `planning_required.md`：规划门控；
- `strategy_change.md`：策略切换；
- `teammate.md`：teammate 角色提示词；
- `think_required.md`：思考要求；
- `todo_nag.md`：Todo 提醒。

工作区根目录的 `prompts/` 目前保存 HITL 反馈模板：

- `prompts/human_modify.md`：用户要求修改工具方案时的反馈；
- `prompts/human_reject.md`：用户拒绝工具调用时的反馈。

如果需要调整 Agent 行为，应优先确认模板是通过 `go:embed` 加载，还是由项目目录动态读取，避免修改了不会生效的文件。

## 长期记忆

`internal/memory.Store` 提供两种存储：

### Evergreen 记忆

```text
MEMORY.md
```

内容按段落解析，可使用以下分类前缀：

```text
[preference] 用户偏好简洁输出
[lesson] 该项目的测试命令是 go test ./...
[fact] 项目使用 OpenAI-compatible API
[context] 当前正在重构工具执行器
```

### 每日记忆

```text
daily/YYYY-MM-DD.jsonl
```

每条记录包含时间、分类和内容。系统会：

- 默认保留最近 90 天；
- 使用基于 token 集合的 Jaccard 相似度做去重/替换；
- 限制单条记忆长度；
- 将搜索结果按相关性返回。

长期记忆会参与系统提示词构造，并可在 Agent 运行时由 `memory_search` 主动召回。

## 网络访问

### Web Fetch

`web_fetch` 通过安全 HTTP Client 请求网页，并具备：

- 请求超时；
- 响应大小限制；
- HTML 文本提取；
- 私有 IP 和本地地址检查；
- URL 和 Host 校验。

### Web Search

搜索 Provider 选择：

1. 如果 `WEB_SEARCH_PROVIDER=tavily` 且存在 API Key，使用 Tavily；
2. 如果 `WEB_SEARCH_PROVIDER=brave` 且存在 API Key，使用 Brave；
3. 否则使用配置的 SearXNG；
4. 如果没有指定 SearXNG，则尝试内置公共 SearXNG 实例；
5. 最后回退 DuckDuckGo Lite。

搜索结果只是外部不可信输入，不应被当作系统指令或权限来源。

### SSRF 防护

默认拒绝解析到以下地址的目标：

- Loopback；
- Link-local；
- `10.0.0.0/8`；
- `172.16.0.0/12`；
- `192.168.0.0/16`；
- `169.254.0.0/16`；
- `0.0.0.0/8`。

只有在明确需要访问内部资源时，才设置：

```bash
export WEB_ALLOW_PRIVATE_IPS=1
```

该设置会放宽出站网络限制，应仅在受控环境中使用。

## Judge 质量验证

Judge 是可选的 LLM-as-Judge 层。启用后，Agent 完成一个使用过工具的任务时，会把最近的对话尾部和工具结果交给独立 Judge 模型，要求其输出：

- 是否通过；
- 1–10 分；
- 具体问题；
- 改进建议；
- 是否应该重试。

如果分数低于 `JUDGE_MIN_SCORE`，Runner 会把验证失败反馈重新注入上下文，并要求 Agent 自我修正。Judge 内部错误会采用宽松回退，不应让主流程永久阻塞。

Judge 主要代码：

```text
internal/agent/judge.go
internal/prompt/templates/judge_system.md
internal/prompt/templates/judge_critical.md
```

## 离线评估

`cmd/eval` 提供回归评估入口，默认使用 Mock 模式，不需要真实 API Key：

```bash
go run ./cmd/eval
```

主要参数：

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `-live` | `false` | 使用真实 LLM，而不是 Mock |
| `-model` | 空 | live 模式下的模型 ID |
| `-v` / `-verbose` | `false` | 输出详细过程 |
| `-timeout` | `5m` | 单任务超时 |
| `-category` | 空 | 只运行指定类别 |
| `-task` | 空 | 只运行指定名称的任务 |
| `-output` | 空 | 输出 JSON 结果文件 |
| `-baseline-out` | 空 | 输出 baseline JSON 文件 |

示例：

```bash
# 只运行某一类任务
go run ./cmd/eval -category tools -v

# 只运行一个任务并保存 JSON
go run ./cmd/eval -task file_edit -output /tmp/eval.json

# 使用真实模型运行
go run ./cmd/eval -live -model gpt-4o -timeout 10m
```

评估任务和 Harness 位于：

```text
internal/eval/tasks.go
internal/eval/eval.go
internal/eval/executor.go
```

## 项目结构

```text
go-code-agent-refactor/
├── cmd/
│   ├── agent/
│   │   ├── main.go              # CLI 入口
│   │   └── repl.go              # 终端 REPL 和斜杠命令
│   └── eval/
│       └── main.go              # 离线评估入口
├── internal/
│   ├── agent/                   # Agent Loop、子 Agent、团队、Judge、压缩
│   ├── application/             # 应用组合根和会话运行时
│   ├── background/              # 后台任务监督器
│   ├── config/                  # 环境配置和运行常量
│   ├── eval/                    # 评估任务和 Harness
│   ├── event/                   # 事件模型和 Console/Audit/Usage Sink
│   ├── history/                 # JSONL 会话历史和恢复清理
│   ├── hitlaudit/               # HITL 审批适配器和模式
│   ├── llm/                     # Provider 无关的消息和调用类型
│   ├── mcp/                     # MCP client、manager 和工具注册
│   ├── memory/                  # 长期记忆和相似度搜索
│   ├── model/                   # Gateway、Throttle、Stream Sink
│   ├── prompt/                  # go:embed 提示词模板加载器
│   ├── security/                # 路径、Shell、SSRF、权限、脱敏
│   ├── session/                 # 会话元数据和索引
│   ├── skill/                   # SKILL.md 递归加载器
│   ├── store/                   # 原子文件写入
│   ├── task/                    # 任务 CRUD、Todo 和 DAG
│   ├── team/                    # 消息总线和协作协议
│   ├── tool/                    # 工具定义、Catalog、Executor、Handlers
│   ├── utils/                   # 终端和进程辅助函数
│   ├── web/                     # Web Fetch、Search 和安全 HTTP Client
│   └── worktree/                 # git worktree 生命周期
├── prompts/
│   ├── human_modify.md          # HITL 修改反馈模板
│   └── human_reject.md          # HITL 拒绝反馈模板
├── skills/
│   ├── agent-builder/SKILL.md
│   ├── code-review/SKILL.md
│   ├── judge/SKILL.md
│   ├── pdf/SKILL.md
│   └── skill_format.md
├── examples/
│   └── mcp/                     # MCP 配置示例
├── go.mod
└── go.sum
```

`tmp/`、`workspace/` 和用户本地生成的状态目录可能出现在开发环境中，但它们不是核心源码，不应作为项目功能依赖。运行 Agent 时，工作目录由 `--workdir` 指定。

## 开发指南

### 常用命令

```bash
# 下载依赖
go mod download

# 格式化
gofmt -w ./cmd ./internal

# 编译
go build ./...

# 单元测试
go test ./...

# 竞态检测
go test -race ./...

# 查看包列表
go list ./...
```

### 添加内置工具

1. 在 `internal/tool/handlers.go` 或 `handlers_helpers.go` 中构造 `ToolDefinition`；
2. 提供完整 JSON Schema；
3. 明确 `RiskLevel`、`Effects`、`Timeout` 和 `SnapshotPolicy`；
4. Handler 只通过 `ToolScope` 获取工作目录、权限和上下文；
5. 使用 `Succeeded`、`Failed`、`Denied` 等结构化结果；
6. 避免 Handler 直接访问全局 Application；
7. 为路径、参数、网络和错误边界补充测试。

### 添加 Provider

1. 实现 `model.Provider` 接口；
2. 将 SDK 类型转换为 `internal/llm` 中立类型；
3. 正确处理文本 delta、工具调用 delta、Usage、finish reason 和 stream error；
4. 在 Provider Registry 注册实例或 builder；
5. 更新模型前缀推断逻辑（如需要）；
6. 为普通请求、流式请求、工具调用和错误响应增加测试。

### 修改 Agent 行为

优先考虑以下扩展点：

- 增加工具，而不是在 Runner 中硬编码业务流程；
- 增加 Prompt Template，而不是复制 Agent Loop；
- 使用 Subagent 隔离探索上下文；
- 使用 Task/DAG 表达多步骤工作；
- 使用 Event Sink 增加日志或 UI 输出；
- 使用 `ToolScope` 注入能力和权限；
- 保持 SessionRuntime 的关闭顺序和资源边界。

### 测试重点

核心测试文件分布在各包的 `*_test.go` 中，重点覆盖：

- Agent compression、reflection、lesson 和 runner controls；
- history 恢复和 dangling message 清理；
- tool executor、JSON 参数解析和安全管线；
- permissions、SSRF、security path；
- MCP 协议；
- task/Todo/DAG；
- memory 和 web service；
- provider DSML 和 Usage 映射；
- background supervisor 和 team protocol。

## 故障排查

### 启动时报没有可用 Provider

检查：

```bash
env | grep -E 'MODEL_ID|LLM_PROVIDER|OPENAI|ANTHROPIC'
```

确认：

- 至少设置了一个受支持的 API Key；
- `LLM_PROVIDER` 拼写正确；
- 模型 ID 与 Provider 匹配；
- OpenAI-compatible 服务设置了正确的 `OPENAI_BASE_URL`；
- Base URL 没有被错误地重复拼接路径。

### 输入后长时间没有文本

Agent 可能正在：

- 等待 LLM 首个响应；
- 生成大量工具调用参数；
- 访问网络；
- 执行 Shell 或 MCP 工具；
- 等待安全审批；
- 进行上下文压缩或 Judge 评估。

当前 Provider 会尽量转发流式文本 delta。工具调用本身可能只有结构化参数，没有可显示的自然语言文本，因此终端可能在工具开始前看起来没有输出。可以观察事件日志和 `session.log`，也可以按 `Ctrl-C` 取消当前请求。

### Ctrl-C 后旧 prompt 再次执行

确认使用的是最新构建，并检查当前会话的：

```text
<state>/sessions/<session-id>/history/history.jsonl
```

启动恢复时会清理尾部孤儿 user 消息。如果历史文件在外部被修改，或旧版本写入了不完整的消息块，建议先备份该 session 目录，再使用 `/session new` 创建新会话。

### `/approve danger` 后文件操作异常

`danger` 会跳过人工确认和 diff preview。它不会修复模型产生的错误参数、路径错误或工具自身的失败。建议恢复：

```text
/approve safe
```

并使用：

```text
/decisions
/security
```

检查工具决策和安全状态。

### MCP server 启动失败

检查：

- 命令是否存在，例如 `npx`、Python 或自定义 server；
- `args` 是否正确；
- 环境变量是否传入；
- 使用 `/mcp pending` 和 `/mcp` 查看状态；
- 查看标准错误和 `session.log`；
- 工作区 `.mcp.json` 是否是有效 JSON。

### Web 请求被拒绝

这是默认安全策略的预期行为，常见原因是目标解析到本地或私有 IP。只有在明确需要访问内部服务并且环境受控时，才设置：

```bash
WEB_ALLOW_PRIVATE_IPS=1
```

## 已知限制

1. **Gemini 尚未在当前应用组装流程中注册**：虽然仓库中存在 Gemini Provider 实现，但 `Application.New` 当前只注册 OpenAI 和 Anthropic。
2. **MCP 是外部进程集成**：MCP server 的可靠性、依赖安装和权限由外部命令决定；项目本身不能保证第三方 server 的安全性。
3. **`bash` 不是完整沙箱**：它最终通过 `sh -c` 执行，安全策略只能降低风险，不能替代容器或操作系统隔离。
4. **公共搜索实例不适合生产环境**：未显式配置时会尝试公共 SearXNG 实例，稳定性和隐私不可控。
5. **上下文和工具输出仍可能很大**：虽然存在截断、压缩和输出限制，但复杂任务仍可能消耗大量 Token 和时间。
6. **流式工具调用依赖 Provider 协议**：不同 OpenAI-compatible 服务对原生 tool call、DSML 和 stream event 的实现可能不同；不兼容的服务可能出现工具参数解析失败或显示协议标记。
7. **自动批准会显著扩大风险面**：`/approve danger` 会跳过关键的人工确认，不建议在包含敏感数据、生产代码或未审查 MCP 的环境中使用。
8. **状态文件包含会话内容**：历史、日志、Usage、决策和记忆可能包含源代码、命令输出或外部数据，需要按敏感数据处理。
9. **README 和技能内容会影响模型行为**：工作区 `skills/` 与提示词会进入模型上下文，应审查来源，避免把不可信文件当成系统级指令。

## 安全建议

- API Key 仅使用环境变量或外部 Secret Manager 注入；
- 不要把真实凭据写入 `examples/`、`.mcp.json`、`permissions.json` 或日志；
- 默认使用 `/approve safe`，仅在受控环境临时使用 `/approve danger`；
- 对外部 MCP server 逐个审批，不要盲目执行未知命令；
- 对不可信代码库使用容器、虚拟机或专用用户；
- 不要在生产机器上直接启用宽泛 Shell 权限；
- 将状态目录和 `session.log` 当作可能包含敏感代码的文件保护；
- 对 `WEB_ALLOW_PRIVATE_IPS=1`、自定义 `SEARXNG_URL` 和自定义 MCP endpoint 做变更审计；
- 修改工具时同时检查路径穿越、命令注入、SSRF、敏感信息泄露和权限绕过；
- 提交代码前运行：

```bash
gofmt -w ./cmd ./internal
go test ./...
go test -race ./...
go build ./...
```

## 相关文件索引

| 主题 | 主要文件 |
|---|---|
| CLI | `cmd/agent/main.go`、`cmd/agent/repl.go` |
| 应用组装 | `internal/application/application.go` |
| Agent Loop | `internal/agent/runner.go` |
| LLM 抽象 | `internal/llm/types.go`、`internal/model/gateway.go` |
| Provider | `internal/model/provider/*.go` |
| 工具定义和执行 | `internal/tool/definition.go`、`internal/tool/executor.go`、`internal/tool/handlers.go` |
| 安全 | `internal/security/*.go`、`internal/hitlaudit/*.go` |
| 会话和历史 | `internal/session/repository.go`、`internal/history/history.go` |
| 记忆 | `internal/memory/*.go` |
| 任务 | `internal/task/*.go` |
| 团队 | `internal/team/*.go`、`internal/agent/teammate.go` |
| MCP | `internal/mcp/*.go` |
| 网络 | `internal/web/*.go` |
| Prompt | `internal/prompt/loader.go`、`internal/prompt/templates/` |
| Skill | `internal/skill/loader.go`、`skills/` |
| 评估 | `cmd/eval/main.go`、`internal/eval/` |

## 许可证

当前仓库未发现根目录许可证文件。若要公开发布或作为依赖使用，请先补充明确的许可证和第三方依赖声明。
