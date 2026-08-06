# go-code-agent

使用 Go 编写的终端自主编程 Agent：多轮规划、自我反思、多 Agent 协作、持久化记忆，以及 MCP（Model Context Protocol）集成。

本项目是 CLI/REPL 应用，不是 HTTP 服务，也不是 IDE 插件。

英文版见 [README.md](README.md)。

## 目录

- [快速开始](#快速开始)
- [架构设计](#架构设计)
- [执行流程](#执行流程)
- [配置参考](#配置参考)
- [目录结构](#目录结构)
- [REPL 命令](#repl-命令)
- [内置工具](#内置工具)
- [LLM 提供商](#llm-提供商)
- [安全模型](#安全模型)
- [Web 访问与 SSRF 防护](#web-访问与-ssrf-防护)
- [Agent 能力](#agent-能力)
- [记忆系统](#记忆系统)
- [会话管理](#会话管理)
- [多 Agent 协作](#多-agent-协作)
- [MCP 集成](#mcp-集成)
- [Token 管理](#token-管理)
- [Usage 统计](#usage-统计)
- [依赖](#依赖)
- [示例工作流](#示例工作流)

---

## 快速开始

### 环境要求

- Go `1.25.3`（以 `go.mod` 为准）
- 至少一个 LLM API Key（`OPENAI_API_KEY` 或 `ANTHROPIC_API_KEY`）
- 建议安装 Git（Teammate worktree 与可选 snapshot 依赖 Git）

### 安装

```bash
git clone https://github.com/rj08zhou/go-code-agent.git
cd go-code-agent
go mod download
go build -o agent ./cmd/agent
```

### 运行

```bash
# 默认：Anthropic（claude-*）
export ANTHROPIC_API_KEY="sk-ant-..."
./agent

# OpenAI
export OPENAI_API_KEY="sk-..."
export MODEL_ID="gpt-4o"
./agent

# OpenAI 兼容接口（如智谱 GLM、DeepSeek、本地 Ollama）
export OPENAI_API_KEY="<key>"
export OPENAI_BASE_URL="https://open.bigmodel.cn/api/paas/v4"
export MODEL_ID="glm-4.7-flash"
./agent

# 强制指定提供商（忽略 MODEL_ID 前缀推断）
export LLM_PROVIDER="anthropic"  # openai | anthropic
./agent

# 启用 LLM-as-Judge（默认配置）
export JUDGE_ENABLED=1
./agent

# 自定义 Judge 模型与分数阈值
export JUDGE_ENABLED=1
export JUDGE_MODEL=claude-haiku-4.5
export JUDGE_MIN_SCORE=8
./agent
```

### 会话管理

```bash
./agent --session <id>     # 恢复指定会话
./agent --new-session      # 强制新建会话
./agent --workdir /path/to/project
```

默认持久化状态根目录：

```text
~/.config/go-code-agent/<basename(workdir)>/
```

可用 `--data-dir` 覆盖配置根目录（最终解析为 `{data-dir}/go-code-agent/<basename(workdir)>`）。

### 可选功能

```bash
# LLM-as-Judge：完成后校验
export JUDGE_ENABLED=1

# 将 HITL 提升为 interactive（所有工具都需确认）
./agent --human

# 显式指定 HITL 模式
./agent --human-mode=auto-approve

# 危险写操作前启用 git-stash snapshot / 失败回滚
export SNAPSHOT_ENABLED=1
./agent
```

默认审批模式为 **`safe-auto`**（较低风险审查自动继续；高风险操作与写文件 diff 会进入审查），可通过 `/approval` 查看或切换。

---

## 架构设计

```
┌──────────────────────────────────────────────────────────────────┐
│                        USER INPUT (REPL)                         │
│   slash commands → cmd/agent/repl.go                             │
│   user message  → Runner.Run (memory recall + tool loop)         │
└──────────────────────────────┬───────────────────────────────────┘
                               │
                               ▼
┌──────────────────────────────────────────────────────────────────┐
│              COMPOSITION ROOT (internal/application)             │
│  Application (process) → SessionRuntime → Build / BuildRunner    │
└──────────────┬───────────────────────────────┬───────────────────┘
               │                               │
               ▼                               ▼
┌──────────────────────────┐   ┌───────────────────────────────────┐
│  MODEL GATEWAY           │   │  TOOL PIPELINE                    │
│  (internal/model)        │   │  (internal/tool)                  │
│                          │   │                                   │
│  openai / anthropic      │   │  39 built-in + mcp__* tools       │
│  role throttle + retry   │   │  validate → capability → preview  │
│                          │   │  → HITL → timeout → sanitize      │
└──────────────────────────┘   └────────────────┬──────────────────┘
                                                │
                     ┌──────────────────────────┼──────────────────┐
                     ▼                          ▼                  ▼
┌───────────────────────┐  ┌─────────────────────┐  ┌─────────────────┐
│  Planning             │  │  Execution          │  │  Multi-agent    │
│  PlanGate + Todo +    │  │  bash / files / bg  │  │  TeammateMgr    │
│  task DAG             │  │  explore / web_*    │  │  Bus + Protocols│
└───────────────────────┘  └─────────────────────┘  └─────────────────┘
                                                │
                                                ▼
┌──────────────────────────────────────────────────────────────────┐
│                       STORAGE / STATE                            │
│  MemoryStore (MEMORY.md + daily JSONL)                           │
│  TaskService (file-persisted JSON) + DAG edges                   │
│  MessageBus (JSONL inbox) + ProtocolStore                        │
│  HistoryStore (JSONL + compression checkpoints)                  │
│  SessionRepository (sessions.json + per-session dirs)            │
│  Skills / Prompts + MCP servers                                  │
└──────────────────────────────────────────────────────────────────┘
```

### 组件归属

`application.Application` 是组合根。`cmd/agent` 只负责解析参数、调用 `application.New` / `Build`，并运行 REPL。

| 生命周期 | 归属 | 示例 |
|----------|------|------|
| 进程级 | `Application` | Gateway、provider 注册表、session repository、dataDir |
| 会话级 | `SessionRuntime` + `BuiltRunner` | Runner、catalog、HITL、ApprovalState、MCP、team、history、tasks |
| 单次 `Run()` | `Runner.turn`（`turnState`） | rounds、failures、readCounts、lesson/judge 计数 |

`ApprovalState` 是**会话级**对象（在 `Build` 中创建，挂在 `BuiltRunner` 上，并注入 `HITLApprovalAdapter`），不是进程全局单例。

---

## 执行流程

### 单条用户消息生命周期

```
user msg
  │
  ├─ 注入 memory recall（UserMessage，利于 prompt cache）
  ├─ AgentStarted 事件
  ▼
Runner.Run(ctx, messages, traceID)
  │
  for each round:
  │  ├─ prepareRound
  │  │    丢弃已消费 nudge → max-rounds / soft-deadline 收束
  │  │    autoCompact / microCompact → plan gate → task note → reflection
  │  ├─ callModel
  │  │    修复 tool_call 配对 → explore prompt budget → Stream
  │  ├─ truncation? 合成 tool error 后 continue
  │  ├─ 无工具? auto-lesson → judge → TurnComplete / return
  │  └─ 有工具? executeToolBatch → afterTools（手动 compress、lesson budget）
  ▼
REPL 将新消息追加到 HistoryStore
```

### 多轮对话

```
会话启动（Build）
  → 确保 session 目录，接线 sinks（session.log、usage.jsonl）
  → 如需恢复则加载 history
  → 打印 REPL banner

每个用户回合
  → 排空后台通知与 inbox
  → Runner.Run
  → 持久化 assistant/tool 消息与可选 checkpoint

会话切换 / 新建 / 归档
  → 关闭 SessionRuntime hooks
  → 用新选项再次 Build
```

---

## 配置参考

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `MODEL_ID` | `claude-opus-4.7` | 模型 ID（也用于推断提供商） |
| `LLM_PROVIDER` | 自动推断 | 强制提供商：`openai` \| `anthropic` |
| `ANTHROPIC_API_KEY` | — | Anthropic 模型必需 |
| `ANTHROPIC_BASE_URL` | SDK 默认 | Anthropic 网关/代理 |
| `OPENAI_API_KEY` | — | OpenAI / 兼容接口必需 |
| `OPENAI_BASE_URL` | SDK 默认 | 代理或本地模型端点 |
| `REASONING_ENABLED` | 关闭 | 为 Agent 调用启用 Provider 原生推理；思考过程以暗色品红 `[thinking]` 流式展示，不透明状态不会写入会话历史 |
| `REASONING_EFFORT` | `medium` | OpenAI 兼容推理强度：`minimal` \| `low` \| `medium` \| `high` |
| `LLM_MAX_QPS` | `4.0` | 进程级 LLM 请求/秒 |
| `LLM_MAX_BURST` | `8` | 令牌桶突发容量 |
| `LLM_MAX_CONCURRENCY` | `4` | 最大并发 LLM 调用 |
| `SNAPSHOT_ENABLED` | 关闭 | 设为 `1` 启用 git-stash 回滚 |
| `CONTEXT_WINDOW_TOKENS` | 模型默认 | 覆盖上下文窗口大小 |
| `WEB_ALLOW_PRIVATE_IPS` | 关闭 | 允许 `web_*` 访问私网 IP |
| `WEB_SEARCH_PROVIDER` | auto | `tavily` \| `brave`（需 API Key）或自动降级链 |
| `WEB_SEARCH_API_KEY` | — | 强制搜索后端的 API Key |
| `SEARXNG_URL` | — | 可信 SearXNG 实例 |
| `SEARXNG_INSTANCES` | 内置列表 | 逗号分隔的 SearXNG 回退列表 |

### 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--workdir <path>` | 当前目录 | Agent 编辑与执行命令的项目目录 |
| `--data-dir <path>` | `~/.config`（或 `$XDG_CONFIG_HOME`） | 配置根；状态位于 `{data-dir}/go-code-agent/<project>-<路径哈希>` |
| `--session <id>` | — | 恢复指定会话 |
| `--new-session` | false | 强制新建会话 |
| `--human` | false | 将审批模式提升为 `manual` |
| `--human-mode` | （保持默认） | `interactive` \| `safe-auto` \| `auto-approve` \| `auto-reject` \| `notify-only`（别名：`safe-only`） |

### LLM-as-Judge 环境变量

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `JUDGE_ENABLED` | 未设置 | 启用 Judge（`1` / `true` / `yes` / `on`） |
| `JUDGE_MODEL` | 主模型 | Judge 使用的模型 ID |
| `JUDGE_MIN_SCORE` | 7 | 低于该分数触发重试（1–10） |
| `JUDGE_PROVIDER` | 推断 | `openai` / `anthropic` |
| `JUDGE_API_KEY` | 主 Key | Judge 专用 Key |
| `JUDGE_BASE_URL` | 主 URL | Judge 专用端点 |

### 可调常量（`internal/config/config.go`）

编译期常量（不可通过环境变量配置）。部分取值：

| 类别 | 常量 | 值 | 说明 |
|------|------|----|------|
| **Loop** | `MaxRounds` | 100 | Agent 循环硬上限 |
| | `StuckThreshold` | 12 | 无进展轮数 → stuck |
| | `ReflectInterval` | 12 | 每 N 轮周期性反思 |
| | `MaxConsecutiveFailures` | 3 | 同工具连续失败 → 策略变更 |
| | `LessonThreshold` | 3 | 触发 auto-lesson 的最少轮数 |
| | `SubagentMaxRounds` | 16 | Explore 软轮数上限 |
| | `TeammateWorkMaxRounds` | 50 | Teammate 工作阶段上限 |
| | `DefaultMaxOutputTokens` | 16384 | 默认最大输出 token |
| | `SubagentPromptTokenBudget` | 80000 | Explore 累计 prompt 预算 |
| | `ExploreBudgetWarnFrac` | 0.70 | 「开始收束总结」软提示阈值 |
| **Tokens** | `TokenThreshold` | 300000 | autoCompact 辅助上界 |
| | `CompactionThresholdFrac` | 0.75 | 触发 compact 的上下文占比 |
| | `MicroCompactThresholdFrac` | 0.5 | microCompact 压力门槛 |
| | `MicroCompactMinClearBytes` | 4096 | 回收过少则跳过 microCompact |
| | `KeepRecent` | 15 | compact 后保留的完整近期 tool 消息数 |
| | `TokenCheckInterval` | 3 | 每 N 轮重新估算 token |
| **Timing** | `PerToolTimeout` | 5 min | 单工具硬超时 |
| | `BashTimeout` | 120s | bash / background_run 超时 |
| | `LlmMaxRetries` | 5 | LLM 重试次数 |
| | `LlmCallTimeout` | 5 min | 单次 Call/Stream 超时 |
| | `WebFetchSubagentBudget` | 60s | Lead `web_fetch` 子代理总时限 |
| | `WebSearchTimeout` | 25s | 搜索总预算 |
| | `WebSearchPerBackendTimeout` | 7s | 单后端搜索上限 |
| **Memory** | `MemoryTTLDays` | 90 | daily 文件过期清理天数 |
| | `MaxEvergreenChars` | 8000 | MEMORY.md 注入截断 |
| | `MaxMemoryContentLen` | 2000 | 单条记忆最大字符数 |
| **Judge** | `JudgeMaxRetryInjects` | 2 | 校验失败最大重试注入次数 |
| **Team** | `PollInterval` | 5s | 空闲 teammate 轮询 inbox |
| | `IdleTimeout` | 60s | 空闲 teammate 自动关闭 |

---

## 目录结构

```
go-code-agent/
├── cmd/
│   ├── agent/
│   │   ├── main.go              # 参数解析、Application.New、Build 循环
│   │   └── repl.go              # Slash 命令与对话循环
│   └── eval/
│       └── main.go              # Eval 入口
├── internal/
│   ├── application/             # 组合根（Application、SessionRuntime）
│   ├── agent/                   # Runner 循环、explore、teammate、judge、压缩
│   ├── tool/                    # Catalog、executor、内置 handlers
│   ├── model/                   # Gateway + provider 实现
│   ├── hitlaudit/               # HITL manager + ApprovalAdapter
│   ├── security/                # 路径沙箱、bash 策略、ApprovalState、SSRF、diff
│   ├── session/                 # 会话索引与 meta.json
│   ├── history/                 # 对话 JSONL + checkpoint
│   ├── memory/                  # Evergreen + daily 记忆
│   ├── task/                    # 持久化任务 DAG + TodoManager
│   ├── team/                    # 消息总线与协议
│   ├── mcp/                     # MCP stdio 客户端/管理器
│   ├── web/                     # web_fetch / web_search
│   ├── event/                   # 事件 sinks（console、audit、usage、session.log）
│   ├── config/                  # 环境配置 + 编译期常量
│   ├── prompt/                  # Prompt 模板
│   ├── skill/                   # SKILL.md 加载器
│   ├── worktree/                # Teammate git worktree
│   ├── background/              # 后台任务监管
│   ├── store/                   # 原子写文件
│   ├── llm/                     # 共享 LLM 消息类型
│   ├── logging/                 # 结构化日志
│   ├── utils/                   # 终端与辅助函数
│   └── eval/                    # 回归评估辅助
├── skills/                      # 示例工作区 skills
├── go.mod
├── README.md
└── README_zh.md
```

---

## REPL 命令

| 命令 | 说明 |
|------|------|
| `/help` | 显示命令帮助 |
| `/session` | 当前会话信息 |
| `/session list` | 列出会话 |
| `/session switch <id>` | 切换会话（重建 runtime） |
| `/session new` | 新建会话 |
| `/session rename <title>` | 重命名当前会话 |
| `/session archive` | 归档当前会话 |
| `/tasks` | 渲染短期 Todo |
| `/dag` | 显示持久化任务 DAG |
| `/task clear\|reset` | 清理已完成 / 清空全部任务 |
| `/memory` | 记忆统计 |
| `/mcp` | 列出 MCP 服务器 |
| `/mcp pending` / `approve <name>` | 待批准 MCP |
| `/mcp connect` / `disconnect` | 运行时连接/断开 MCP |
| `/team` | 列出 / 拉起 / 关闭 / 发消息给 teammate |
| `/inbox` | 读取 lead inbox |
| `/judge` | 开关 LLM-as-Judge |
| `/approval [manual\|safe-auto\|all-auto\|reject\|notify-only]` | 查看或设置最终审批模式（`all-auto` 需附加 `confirm`） |
| `/permissions [reload]` | 查看 / 重载 permissions.json |
| `/security` | 安全状态 |
| `/security test-bash <cmd>` | 干跑 bash 策略 |
| `/decisions` | 最近决策日志 |
| `/usage` | Token 用量汇总 |
| `/compact` | 手动压缩上下文 |
| `/search <query>` | 一次性 web 搜索 |
| `/exit` `/quit` | 退出 |

---

## 内置工具

39 个内置工具（目录顺序稳定，利于 prompt cache），外加动态 `mcp__*` 工具。

### 核心（文件与 Shell）

| 工具 | 说明 |
|------|------|
| `read_file` / `list_dir` | 读文件 / 列目录 |
| `search_file` / `search_content` | 按文件名 / 内容搜索 |
| `write_file` / `edit_file` / `insert_file` / `delete_file` | 变更（启用时带 diff preview） |
| `bash` | 前台 Shell（超时 + 策略） |
| `background_run` / `check_background` | 后台任务 |

### 推理 / Meta

| 工具 | 说明 |
|------|------|
| `explore` | 隔离只读探索子代理 |
| `compress` | 请求对话压缩 |
| `load_skill` | 加载工作区 skill |

### 规划与任务

| 工具 | 说明 |
|------|------|
| `TodoWrite` | 短期清单 |
| `task_create` / `task_list` / `task_update` / `task_get` | 持久化任务 |
| `task_add_dep` / `task_remove_dep` / `task_ready` / `task_dag` / `claim_task` | DAG 调度 |

### 多 Agent 协作

| 工具 | 说明 |
|------|------|
| `spawn_teammate` / `list_teammates` | 生命周期 |
| `send_message` / `read_inbox` / `broadcast` | 消息 |
| `shutdown_request` / `plan_approval` / `submit_plan` | 协议 |

### 记忆

| 工具 | 说明 |
|------|------|
| `memory_write` / `memory_search` / `memory_delete` / `memory_stats` | 长期记忆 |
| `session_save_memory` | 归档会话收获 |

### Web 访问

| 工具 | 说明 |
|------|------|
| `web_fetch` | 通过隔离子代理抓取 URL（仅 `web_fetch` + `web_search`） |
| `web_search` | 多后端搜索与回退 |

### MCP（动态）

MCP 服务器启动/批准后，以 `mcp__<server>__<tool>` 形式注册到会话工具目录。

---

## LLM 提供商

支持的后端：

- Anthropic API（`anthropic-sdk-go`）
- OpenAI API 及 OpenAI 兼容端点（`openai-go`）

### 提供商选择逻辑

1. 若设置了 `LLM_PROVIDER` 则使用它
2. 否则根据 `MODEL_ID` 前缀与可用 Key 推断
3. Gateway 上按角色限流（`lead`、`explore`、`teammate`、`judge`）

### 重试策略

指数退避，并对限流单独处理（`LlmMaxRetries`、`LlmBaseDelay`、`LlmRateLimitDelay`、`LlmMaxDelay`），另有单次调用与 HTTP 超时。

---

## 安全模型

围绕工具执行的多层防护。

### 1. Bash 策略（命令白名单）

常见安全命令白名单，外加危险模式的 deny/confirm 正则（如 `rm -rf /`、提权、危险 git 等）。

### 2. 路径沙箱

`security.SecurePath` 将文件操作限制在 workdir / 允许根内。工作区内的绝对路径会被正确保留（已处理 Go 1.25 `filepath.Join` 对绝对段的行为）。

### 3. 审批级别与 HITL

两套协作组件，由 `ApplyMode` 一并切换，避免姿态漂移：

| 组件 | 作用 |
|------|------|
| `security.ApprovalState` | 会话 `/approval` 姿态：`manual` / `safe-auto` / `all-auto`；控制自动放行与是否展示 diff preview |
| `hitlaudit.HITLManager` | 交互模式：`interactive`、`safe-auto`、`auto-approve`、`auto-reject`、`notify-only` |

`HITLApprovalAdapter` 将二者适配为 executor 使用的 `tool.ApprovalChecker`。启动默认模式为 **`safe-auto`**（内部 `HITLModeSafeAuto`）；`--human` 提升为 `manual`，也可用 `--human-mode` / REPL `/approval` 切换。

#### 变更计划、Diff 渲染与 HITL（三层）

写文件工具在审批前会经过两步**无交互**准备，再进入 HITL：

| 层 | 符号 | 职责 |
|----|------|------|
| 1. Mutation plan | `def.PlanMutation` → `MutationPlan` | **算出**拟写入内容（path / 原文 / 新文），不生成 diff |
| 2. Diff render | `DiffPreview.PreviewChange` | 把计划渲染成 unified diff **文本** |
| 3. HITL | `DecideTool` → `PreviewAndConfirm` 等 | **决定**是否放行；有计划时用第 2 层文本开 diff / hunk UI |

打包给审批的 DTO 是 `MutationApprovalInput{DiffText, Plan}`。没有计划时（如 `bash`）HITL 走通用 y/n/m；有计划且 `ShouldShowDiffUI()` 为真时才开交互 diff UI——那是 HITL 的展示，不是又一次 plan。

#### 执行时数据流

工具真正执行前，`Executor` 走 `prepare → authorize → invoke`：

```text
ToolCall
  → prepare（解析定义 / 参数）
  → authorize
       → [Plan]   若有 PlanMutation：算出 MutationPlan（无交互）
       → [Diff]   DiffPreview.PreviewChange → DiffText（无交互）
       → [HITL]   DecideTool(tool, args, MutationApprovalInput)
            1. permissions.json（block / confirm / allow）
            2. classifyReview（shell → ClassifyCommand；敏感路径等）
            3. resolveReviewRequirement（无 per-call 分类时，才用工具静态 RiskLevel → ReviewSeverity 兜底）
            4. decideApprovalDecision
                 · 有 Plan + manual/safe-auto + showDiffUI → decisionPromptMutation（diff UI）
                 · 否则 needsReview → decisionPromptGeneral
                 · 否则 allow
            5. ApprovalReviewer.Apply（PreviewAndConfirm / y/n/m / 或自动决策）
  → invoke handler
       → bash 另有 BashPolicy 硬拒绝（VerdictDeny 即使 HITL 批准也不执行）
```

关键包：`internal/tool/`（PlanMutation + Executor）、`internal/security/diff_preview.go`（渲染）、`internal/security/diff_review.go`（交互 UI）、`internal/hitlaudit/`（HITL）、`internal/security/classify.go`（命令风险）。

#### Shell 命令如何判定

`bash` / `execute_command` / `background_run` 的工具级 `RiskLevel` 是**最坏情况**（`RiskDanger`）。真正是否审查看**这一次命令**：

| `ClassifyCommand` | HITL | 默认 safe-auto |
|-------------------|------|----------------|
| Safe（`ls` / `grep` / `git status`） | 不审，`commandClassified` | 直接执行 |
| Caution（`mkdir` / `git commit`） | 审，`ReviewSeverity=high`，保留命令级 reason | 弹窗确认 |
| Danger（`rm …`） | 审，`ReviewSeverity=high`，保留命令级 reason | 弹窗确认 |
| Deny（`sudo` / `\| sh`） | 可能仍弹窗 | handler 内硬拒绝 |

命令一旦分类完成，**不再**被工具静态 `RiskDanger` 覆盖（避免先 high 再 danger、reason 被冲成 “tool is classified as dangerous”）。用户 `permissions.json` 的 `confirm` / `block` 规则仍优先于命令分类。

#### 模式行为（摘要）

| REPL `/approval` | 内部模式 | 只读 bash | Caution/Danger bash、危险 MCP | 文件变更（有 PlanMutation） |
|------------------|----------|----------|-------------------------------|------------------------------|
| `manual` | interactive | 放行 | y/n/m 面板 | HITL 用 DiffText 开 diff UI |
| `safe-auto`（默认） | safe-auto | 放行 | severity 为 high/danger 时弹窗 | HITL 用 DiffText 开 diff UI |
| `all-auto`（需 `confirm`） | auto-approve | 放行 | 打印后自动批 | 仍算 Plan+Diff，但跳过 diff UI |
| `reject` | auto-reject | 放行 | 打印后拒 | DiffText 可附在通用审里 → 拒 |
| `notify-only` | notify-only | 放行 | 打印后放行 | DiffText 可附在通用审里 → 放行 |

非 TTY 下 interactive 默认拒绝；可用环境变量 `HITL_NON_TTY_FALLBACK=approve` 改为放行。

#### 与 PlanGate 的关系

PlanGate（规划闸）是另一层：无既定 plan 时拦写文件 / 非只读 shell / 委派。过了 PlanGate 仍可能进入 HITL。两者都消费 `ClassifyCommand`，但互不调用。

### 4. 密钥脱敏

工具输出在返回模型 / 写入日志前会做脱敏。

### 其他安全能力

- 可选 git snapshot + 失败回滚（`SNAPSHOT_ENABLED=1`）
- 决策审计日志（`decisions.jsonl`）
- 会话事件日志（`session.log`）
- 权限规则文件（dataDir 下的 `permissions.json`）

---

## Web 访问与 SSRF 防护

### 默认拒绝私网

除非启用 `WEB_ALLOW_PRIVATE_IPS`，否则 `web_fetch` / `web_search` 会拦截私网 / 链路本地 / 元数据地址。

### web_search 降级链

在配置的 API 提供商与 SearXNG 实例间自动降级；有总预算与单后端超时，避免一个挂起的后端拖死整条链路。

### 不可信内容处理

抓取页面会标记为不可信内容、限制体积，并在注入模型前清洗明显密钥。

---

## Agent 能力

### Think → Plan → Act → Reflect

- 复杂任务时，plan gate 会在早期轮次推动使用规划类工具
- Reflection 注入 stuck / 失败 / todo-nag 提示
- 重复相同 tool call 与 explore 重读会受到限制

### DAG 调度

带依赖边的持久化任务（`task_*` 工具）。就绪任务可被认领；`/dag` 展示拓扑与进度。

### Auto-Lesson

轮数足够后，lead（具备 memory 能力）可能被提示通过 `memory_write` 记录经验。

### LLM-as-Judge

可选的完成后校验。低分会注入重试反馈，最多 `JudgeMaxRetryInjects` 次。

---

## 记忆系统

### 两层架构

| 层级 | 存储 | 用途 |
|------|------|------|
| Evergreen | `MEMORY.md` | 稳定偏好 / 事实 |
| Daily | `daily/YYYY-MM-DD.jsonl` | 带 TTL 清理的短期笔记 |

### 搜索与去重

写入时用 Jaccard 相似度去重；超过 `MemoryTTLDays` 的 daily 文件会在 Store 初始化时清理。

### 自动召回

每个用户回合会把相关记忆以一条 `UserMessage`（“Relevant memory: …”）注入一次，从而保持 system 块稳定、利于 prompt cache。

---

## 会话管理

### 生命周期

1. `Application.New` 创建进程级服务与 dataDir
2. `Build` 解析/创建会话，确保会话目录，装配 `BuiltRunner`
3. REPL 运行直到退出或切换会话
4. 切换 / 新建 / 归档会关闭 hooks 并重新 Build

### 历史持久化

- 追加写入 `history/history.jsonl`
- 压缩时把完整 transcript 存到 `transcripts/`，并写入可恢复的 checkpoint
- `session.log`（JSONL 事件）、`usage.jsonl`、`decisions.jsonl` 位于会话目录
- 索引：`{dataDir}/sessions.json`；元数据：`{dataDir}/sessions/<id>/meta.json`

---

## 多 Agent 协作

### TeammateManager

拉起 teammate，可选独立 git worktree，共享消息总线，并复用与 lead 相同的 HITL/approval adapter。

### 通信

通过 `team.MessageBus` 的 JSONL inbox。协议辅助覆盖 shutdown 请求与 plan approval。

### Explore 子代理

只读向隔离，带 prompt/token 预算与 soft-deadline 收束；工具集受限（不能任意写文件/写记忆）。

---

## MCP 集成

### 配置

工作区 `.mcp.json` 和/或 `MCP_SERVERS` 风格配置（以 manager 当前 schema 为准）。不受信服务器可能需要 `/mcp approve`。

### 工作方式

1. Manager 启动 stdio MCP 服务器
2. 工具以**追加**方式合并进会话 `ToolCatalog`（排在 builtins 之后，顺序保持稳定）
3. 可通过 REPL 运行时 connect/disconnect

### 熔断 / 安全提示

失败服务器会在 `/mcp` 中展示；工具名带 `mcp__` 前缀便于归因。第三方 MCP 工具应视为扩大后的能力面，仍受 HITL / 路径 / 网络等闸门约束（在适用处）。

---

## Token 管理

### microCompact（压缩门控）

仅当估算用量超过上下文窗口的 `MicroCompactThresholdFrac`，且至少能回收 `MicroCompactMinClearBytes` 时，才清理旧 tool 结果——避免短会话无谓打爆 prompt cache。

### autoCompact（阈值触发）

需要压缩时（窗口占比 / explore 阈值）：

1. 将完整 transcript 保存到 `transcripts/`
2. 用 LLM 总结较早轮次
3. 保留近期轮次，并使缓存的 token 估算失效
4. 写入 history checkpoint 以便恢复

### Explore 预算

Explore 子代理跟踪累计 prompt token，接近 70% 时注入 budget-warn，硬预算或 soft deadline 到达时以无工具总结收束。

### Token 估算

用 `EstimateRequestTokens` 做启发式估算，按间隔刷新而非每轮都算。

---

## Usage 统计

LLM 调用会追加到 `{sessionDir}/usage.jsonl`，并同时向 event sinks 发送 usage 事件。

常见字段包括 role/source、model、prompt/completion/cache tokens、耗时与 trace id。

用 `/usage` 查看汇总。

---

## 依赖

| 包 | 用途 |
|----|------|
| `github.com/anthropics/anthropic-sdk-go` | Anthropic API 客户端 |
| `github.com/openai/openai-go` | OpenAI / 兼容 API 客户端 |
| `github.com/chzyer/readline` | 带历史的交互 REPL |
| `golang.org/x/net` | web fetch/search 的 HTML 解析 |

---

## 示例工作流

```
$ JUDGE_ENABLED=1 ./agent

> 先分析这个仓库的工具管线，再提一个聚焦的重构建议。先不要改代码。

[explore] 只读调查…
[memory_search] 与 executor 管线相关的旧笔记
[TodoWrite] 整理发现

> 好，把 HITLApprovalAdapter 拆到单独文件。

[hitl] reviewing write_file [safe]
[write_file] internal/hitlaudit/approval_adapter.go
[edit_file] internal/hitlaudit/human_approval.go
[bash] go test ./internal/hitlaudit/

[judge] score=9 approved
Done.
```
