# go-code-agent

一个用 Go 构建的自主编程 Agent，具备多轮规划、自我反思、多 Agent 协作、持久化记忆以及 MCP（Model Context Protocol）集成能力。

## 目录

- [快速开始](#快速开始)
- [架构](#架构)
- [执行流程](#执行流程)
- [配置项](#配置项)
- [文件布局](#文件布局)
- [REPL 命令](#repl-命令)
- [内置工具](#内置工具)
- [LLM 提供商](#llm-提供商)
- [安全模型](#安全模型)
- [Agentic 特性](#agentic-特性)
- [记忆系统](#记忆系统)
- [会话管理](#会话管理)
- [多 Agent 协作](#多-agent-协作)
- [MCP 集成](#mcp-集成)
- [Token 管理](#token-管理)
- [用量追踪](#用量追踪)
- [依赖项](#依赖项)

---

## 快速开始

### 前置条件

- 已安装 Go 1.25+
- 已配置至少一个 LLM API Key

### 安装

```bash
git clone <repo-url>
cd go-code-agent
go build -o agent ./cmd/agent/
```

### 运行

```bash
# 默认：Anthropic (claude-*)
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

# 忽略 MODEL_ID 前缀，强制指定提供商
export LLM_PROVIDER="anthropic"  # openai | anthropic | gemini
./agent

# 启用 LLM-as-Judge（默认配置）
export JUDGE_ENABLED=1
./agent

# 启用 Judge，自定义模型和阈值
export JUDGE_ENABLED=1
export JUDGE_MODEL=claude-haiku-4.5
export JUDGE_MIN_SCORE=8
./agent

# 使用独立的 LLM 服务进行评判（例如更便宜/更快的模型）
export JUDGE_ENABLED=1
export JUDGE_PROVIDER=openai
export JUDGE_API_KEY="<judge-api-key>"
export JUDGE_BASE_URL="https://api.deepseek.com"
export JUDGE_MODEL="deepseek-chat"
./agent
```

### 会话管理

```bash
./agent                          # 恢复最近一次会话
./agent --new-session            # 开启全新会话
./agent --session <id>           # 恢复指定会话
```

### 可选功能

```bash
# LLM-as-Judge：完成后的验证
#（完整配置示例请参考上方的"运行"章节）
JUDGE_ENABLED=1 ./agent

# 高风险操作启用人工确认（Human-in-the-loop）
./agent --human
./agent --human --human-mode auto-approve    # CI 友好模式

# 写操作启用基于 git-stash 的快照/回滚
SNAPSHOT_ENABLED=1 ./agent
```

**💡 Judge 配置说明**：如需详细配置（自定义模型、独立端点等），请参考[运行](#运行)章节中的完整 `export` 命令示例。

所有持久化状态存储在 `{workdir}/.go-code-agent/` 目录下。会话在重启和崩溃后均可恢复。

---

## 架构

```
┌──────────────────────────────────────────────────────────────────┐
│                        USER INPUT (REPL)                         │
│   slash commands → repl_commands.go (short-circuit)              │
│   user message  → agentLoop (memory via memory_search tool)     │
└──────────────────────────────┬───────────────────────────────────┘
                               │
                               ▼
┌──────────────────────────────────────────────────────────────────┐
│                ORCHESTRATION (internal/agent, loop.go)           │
│                                                                  │
│  Pre-round:   microCompact → tokenCheck → drain bg/inbox        │
│  Gates:       think-gate → planning-gate → write-gate           │
│  Reflection:  mini-reflect → strategy-change → stuck            │
│  Termination: maxRounds → auto-lesson → judge verify            │
└──────────────┬───────────────────────────────┬───────────────────┘
               │                               │
               ▼                               ▼
┌──────────────────────────┐   ┌───────────────────────────────────┐
│  LLM PROVIDERS           │   │  TOOL DISPATCH                    │
│  (provider_*.go)         │   │  (tool_registry + MCP)            │
│                          │   │                                   │
│  openai / anthropic /    │   │  32 built-in + mcp__* tools        │
│  gemini (stub)           │   │  security gate → HITL gate →      │
│  + retry (exp backoff)   │   │  timeout → snapshot/rollback      │
└──────────────────────────┘   └────────────────┬──────────────────┘
                                                │
                     ┌──────────────────────────┼──────────────────┐
                     ▼                          ▼                  ▼
┌───────────────────────┐  ┌─────────────────────┐  ┌─────────────────┐
│  Planning             │  │  Execution          │  │  Multi-agent    │
│  DAGScheduler +       │  │  bash / files / bg  │  │  TeamMgr + Bus  │
│  TodoList + think     │  │  subagent           │  │  Protocols      │
└───────────────────────┘  └─────────────────────┘  └─────────────────┘
                                                │
                                                ▼
┌──────────────────────────────────────────────────────────────────┐
│                       STORAGE / STATE                            │
│  MemoryStore (evergreen + daily, BM25+vector hybrid search)     │
│  TaskStore (file-persisted JSON) + DAG edges                    │
│  MessageBus (JSONL inbox) + ProtocolStore (TTL)                 │
│  HistoryStore (append-only JSONL + checkpoint compaction)       │
│  Skills / Prompts (workspace dir) + MCP servers                 │
└──────────────────────────────────────────────────────────────────┘
```

### 组件所有权

`AppContext`（`internal/agent/app.go`）是持有全部进程级/会话级状态的唯一
根对象；`main.go` 只构造一次（`agent.NewApp(model, workdir, bashValidate)`），
其余全部通过它访问——除工具注册表外，没有任何子系统以裸包变量形式存在。

- **进程级配置**（`AppContext` 字段，由 `NewApp` 一次性构造）：`Model`、`Workdir`、`System`（每会话重建）
- **工作目录全局子系统**（`AppContext` 字段，进程生命周期内持久）：`Skills`、`MemStore`、`MCPMgr`、`PromptLoader`、`SessionManager`、`Snapshot`、`Judge`
- **每会话**（会话切换时重新绑定，经 `SessionManager.Active()` 访问）：`TaskManager`、`DAGScheduler`、`MessageBus`、`ProtocolStore`、`HistoryStore`、`TeammateManager`
- **编译期能力表**（包级、非实例级）：`ToolDefs` / `ToolHandlers` / `ToolSecurityMap`，由 `InitTools()` 一次性填充

生命周期由 `AppContext` 的三个方法驱动，每个都收敛了此前在 `main.go` 与
`repl_commands.go` 之间重复的一段流程：
- `NewApp(model, workdir, bashValidate)` —— 一次调用构造全部工作目录全局子系统
- `ActivateSession(sess)` —— 把 `sess` 绑定进 `SessionManager`，重建 `TeamMgr`，重新生成 `System`
- `DeactivateActiveSession()` —— 关闭全部 teammate，把当前活跃会话落盘到记忆

---

## 执行流程

### 单条用户消息的生命周期

```
user msg
  │
  ├─ buildSystemPrompt()         ← system.md 模板 + 技能 + 常驻记忆 + 任务
  │      （常驻 MEMORY.md 在会话启动时加载一次；日常记忆
  │       由模型按需通过 memory_search 工具召回）
  │
  ▼
agentLoop(ctx, &conv)
  │
  ├─ [preRound]
  │    ├─ microCompact: 将旧的工具结果折叠为摘要
  │    ├─ tokenCheck: 超过 300K tokens → autoCompact（LLM 摘要）
  │    ├─ drain background results → 注入为工具消息
  │    └─ drain team inbox → 注入为用户消息
  │
  ├─ [gates]
  │    ├─ think-gate (round 0): 规划前强制使用 `think` 工具
  │    ├─ planning-gate (round 1): 执行前强制创建任务
  │    └─ write-gate: 文件写入前要求计划审批（teammate）
  │
  ├─ LLM stream call → 解析响应
  │    ├─ 仅文本 → 检查 auto-lesson → 完成
  │    └─ tool_calls[] → 逐一派发：
  │         ├─ 安全检查（bash 策略、路径沙箱）
  │         ├─ HITL 审批门控（如已启用）
  │         ├─ 快照预状态（如已启用）
  │         ├─ 超时包装器（5 分钟上限）
  │         ├─ 执行处理器 → 收集结果
  │         ├─ 输出中的密钥脱敏
  │         └─ 失败时快照回滚（如已启用）
  │
  ├─ [post-dispatch]
  │    ├─ planningGate: 首轮工具调用后检查任务是否存在
  │    ├─ judge: 所有任务完成 + JUDGE_ENABLED → LLM 验证
  │    └─ 反思触发器：
  │         ├─ mini-reflect: 工具调用失败时
  │         ├─ strategy-change: 连续失败 3 次后
  │         ├─ stuck detection: 10 轮无进展
  │         └─ 周期性完整反思：每 5 轮一次
  │
  └─ 循环直至：仅文本响应 | 达到 maxRounds(100) | 判定为 stuck
```

### 多轮对话

```
会话启动：
  HistoryStore.LoadRuntime() → 从 JSONL 恢复消息
  ↓
每轮：
  user input → agentLoop (N 轮) → 持久化新消息
  ↓
会话结束：
  Deactivate → session_to_memory prompt → 将经验保存到 MemoryStore
```

---

## 配置项

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `MODEL_ID` | `claude-opus-4.7` | 模型标识符（也可通过前缀推断提供商） |
| `LLM_PROVIDER` | 自动推断 | 强制指定提供商：`openai` \| `anthropic` \| `gemini` |
| `ANTHROPIC_API_KEY` | — | claude-* 系列模型必需 |
| `ANTHROPIC_BASE_URL` | SDK 默认值 | Anthropic 网关/代理覆盖 |
| `OPENAI_API_KEY` | — | gpt-*/o*/兼容接口必需 |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | 代理/本地模型端点 |
| `SNAPSHOT_ENABLED` | `0` | 设为 `1` 启用基于 git-stash 的回滚 |
| `JUDGE_ENABLED` | 未设置 | 启用 Judge（`1` / `true` / `yes` / `on`） |
| `JUDGE_MODEL` | 主模型 | Judge 调用的模型 ID（空 = 复用主模型） |
| `JUDGE_MIN_SCORE` | 7 | 低于此分数触发重试（1-10 分制） |
| `JUDGE_PROVIDER` | 自动推断 | 显式指定后端 SDK（`openai` / `anthropic` / `gemini`） |
| `JUDGE_API_KEY` | 主密钥 | 仅 Judge 使用的密钥（否则使用对应后端的常规密钥） |
| `JUDGE_BASE_URL` | 主 URL | 仅 Judge 使用的端点（否则使用对应后端的常规 URL） |

### CLI 标志

| 标志 | 默认值 | 说明 |
|------|--------|------|
| `--session <id>` | — | 按 ID 激活指定会话 |
| `--new-session` | false | 强制创建新会话 |
| `--human` | false | 启用人工确认（Human-in-the-loop） |
| `--human-mode` | interactive | `interactive` / `auto-approve` / `auto-reject` / `notify-only` |

### Judge 提供商解析逻辑

Judge 使用 `llm.JudgeProvider()` 在运行时动态选择 LLM 后端：

1. **未设置任何 JUDGE_* 变量** → 使用与主 Agent 相同的提供商（保持向后兼容）
2. **设置了 JUDGE_API_KEY 或 JUDGE_BASE_URL** → 使用独立凭证创建隔离的提供商实例
3. **仅设置了 JUDGE_PROVIDER** → 复用该后端类型的共享提供商实例

这使得 Judge 可以在不同的（通常更便宜的）模型上运行，甚至使用完全独立的 LLM 服务，而不影响主 Agent 的配置。

### 可调常量（infra/consts.go）

所有阈值集中在一个文件中，便于统一调整：

| 分类 | 常量 | 值 | 说明 |
|------|------|-----|------|
| **循环** | MaxRounds | 100 | Agent 循环硬性安全上限 |
|  | StuckThreshold | 20 | 多少轮无进展判定为 stuck |
|  | ReflectInterval | 20 | 每 N 轮周期性反思 |
|  | MaxConsecutiveFailures | 3 | 同一工具连续失败 → 策略变更 |
|  | LessonThreshold | 3 | 自动总结经验最少轮数 |
|  | SubagentMaxRounds | 30 | 子 Agent 内循环上限 |
|  | TeammateWorkMaxRounds | 50 | Teammate 工作阶段上限 |
|  | DefaultMaxOutputTokens | 16,384 | 每次 LLM 调用默认最大输出 token 数 |
| **Token** | TokenThreshold | 300,000 | autoCompact 触发阈值 |
|  | KeepRecent | 15 | microCompact 保留最近 N 条工具消息 |
|  | MaxOutputLen | 500,000 | 每条工具输出最大字节数（500KB） |
|  | TokenCheckInterval | 3 | 每 N 轮重新检查 token 数 |
| **超时** | PerToolTimeout | 5 min | 每个工具处理器硬性上限 |
|  | BashTimeout | 120s | bash / background_run 超时 |
|  | LlmMaxRetries | 5 | LLM 调用重试次数 |
|  | LlmBaseDelay | 1s | 指数退避基数 |
|  | LlmRateLimitDelay | 10s | 429 专用基础退避时间 |
|  | LlmMaxDelay | 60s | 指数退避上限 |
|  | LlmCallTimeout | 5 min | 单次 provider Call/Stream 调用的墙钟上限 |
|  | LlmHTTPTimeout | 6 min | 底层 HTTP 传输超时（兜底） |
|  | LlmDefaultMaxQPS | 2.0 | 进程级 LLM 限流（每秒请求数） |
|  | LlmDefaultMaxBurst | 4 | 令牌桶突发容量 |
|  | LlmDefaultMaxConcurrency | 2 | 进程级最大并发 LLM 调用数 |
|  | SpawnMinInterval | 750ms | teammate/subagent 生成之间的错峰间隔 |
| **记忆** | MemoryTTLDays | 90 | 日常文件超过此天数自动删除 |
|  | MaxEvergreenChars | 8,000 | MEMORY.md 注入截断长度 |
|  | DeduplicateThreshold | 0.7 | 去重 Jaccard 相似度阈值 |
|  | MaxMemoryContentLen | 2,000 | 每条记忆条目最大字符数 |
| **搜索** | Bm25K1 | 1.5 | TF 饱和度参数 |
|  | Bm25B | 0.75 | 长度归一化参数 |
|  | HybridKeywordWeight | 0.65 | 混合合并中 BM25 权重 |
|  | HybridVectorWeight | 0.35 | 混合合并中向量权重 |
| **团队** | PollInterval | 5s | 空闲 teammate 收件箱轮询间隔 |
|  | IdleTimeout | 60s | 空闲 teammate 自动关闭 |
|  | MaxTeamMessageSize | 64 KB | 防止收件箱洪水 |
|  | PlanRequestTTL | 30 min | 待审计划请求过期时间 |
|  | ApprovedPlanTTL | 24h | 已批准计划过期时间 |
| **Judge** | JudgeMinScore | 7 | 重试阈值（1-10） |
|  | JudgeMaxRetryInjects | 2 | 验证失败最多重试次数 |
| **规划** | PlanningGateMinTaskChars | 80 | 跳过门控的最小查询字符数 |
|  | LessonRoundsLimit | 3 | 总结经验后额外最多轮数 |

---

## 文件布局

```
go-code-agent/
├── cmd/agent/                    # 应用层（package main）—— 只做组装，不含业务逻辑
│   ├── main.go                   # 入口：标志解析、依赖组装、REPL 循环、SIGINT 清理
│   └── repl_commands.go          # 斜杠命令派发器（/session、/tasks 等）
│
├── internal/                     # 可复用基础设施 + Agent 引擎
│   ├── agent/                    # Agent 引擎（package agent）
│   │   ├── app.go                #   AppContext：根对象 + NewApp/ActivateSession/DeactivateActiveSession
│   │   ├── loop.go               #   核心多轮执行循环（Run）
│   │   ├── plan.go               #   Think-gate + planning-gate 逻辑
│   │   ├── reflection.go         #   Mini-reflect、strategy-change、stuck 检测
│   │   ├── judge.go              #   LLM-as-Judge 完成后验证器
│   │   ├── decisions.go          #   自主决策审计轨迹（decisions.jsonl，/decisions）
│   │   ├── compression.go        #   microCompact + autoCompact（token 管理）
│   │   ├── subagent.go           #   只读子 Agent 生成器（task 工具）
│   │   ├── team.go               #   TeammateManager：WORK/IDLE 自治循环
│   │   ├── tool_registry.go      #   工具定义（30+ 工具在此注册）
│   │   ├── tool_base.go          #   基础工具处理器（bash、文件、think 等）
│   │   ├── system_prompt.go      #   系统提示词组装
│   │   ├── security.go           #   工具安全注册表、checkToolApproval、HITL 门控 glue
│   │   ├── snapshot.go           #   基于 git-stash 的 Saga 模式快照/回滚
│   │   └── log_file.go           #   每会话文件日志（session.log）
│   ├── llm/                      # LLM 抽象层
│   │   ├── llm_types.go          #   中立类型：Message、ToolCall、ToolDef、Role
│   │   ├── llm_client.go         #   重试包装器、trace ID、流式接口
│   │   ├── provider.go           #   提供商注册表 + PickProvider 逻辑
│   │   ├── provider_openai.go    #   OpenAI/兼容后端（流式 + 工具）
│   │   ├── provider_anthropic.go #   Anthropic 后端（流式 + 工具）
│   │   ├── provider_gemini.go    #   Gemini 占位（预留）
│   │   └── tool_helpers.go       #   工具参数提取辅助函数（MkOk/MkErr/ParseArgs）
│   ├── session/                  # 会话生命周期
│   │   ├── session.go            #   每会话聚合根（拥有 task/team/history）
│   │   └── session_manager.go    #   CRUD、BootstrapOrCreate、activate/deactivate、索引
│   ├── history/                  # 对话持久化
│   │   └── history.go            #   Append-only JSONL + checkpoint 压缩
│   ├── hitlaudit/                # 人工确认 + 审计
│   │   ├── hitlaudit.go          #   审计日志（JSONL）+ 审批门控
│   │   └── human_approval.go     #   4 种模式审批逻辑 + 风险分类
│   ├── task/                     # 任务管理
│   │   ├── task.go               #   TaskManager：CRUD、文件持久化
│   │   ├── task_scheduler.go     #   DAGScheduler：拓扑排序、阶段执行
│   │   └── todo.go               #   简易 TodoList（遗留，DAG 前）
│   ├── team/                     # 多 Agent 基础设施
│   │   ├── team_bus.go           #   MessageBus：每个 Agent 的 JSONL 收件箱、死信
│   │   └── team_protocols.go     #   ProtocolStore：带 TTL 的计划审批
│   ├── memory/                   # 持久化记忆
│   │   └── memory.go             #   双层存储 + BM25 + 哈希向量搜索
│   ├── mcp/                      # Model Context Protocol
│   │   ├── mcp.go                #   MCPManager：服务器生命周期、断路器
│   │   └── mcp_jsonrpc.go        #   JSON-RPC 2.0 over stdio 传输
│   ├── prompt/                   # 模板引擎
│   │   └── prompt.go             #   Loader + 渲染器（{{.Var}} 替换）
│   ├── skill/                    # 技能系统
│   │   └── skills.go             #   SkillLoader：读取 SKILL.md 文件
│   ├── background/               # 后台任务运行器
│   │   └── background.go         #   Goroutine 池 + 结果收集
│   ├── usage/                    # Token 用量遥测
│   │   └── usage.go              #   UsageRecorder：每次调用的 JSONL 日志
│   └── logging/                  # 终端输出
│       └── log.go                #   彩色输出辅助函数（system/error/tool/agent）
│
├── infra/                        # 横切常量
│   └── consts.go                 # 所有可调阈值集中在此
│
├── utils/                        # 共享工具函数
│   └── utils.go                  # 路径辅助函数（JoinWorkdir、Truncate 等）
│
├── prompts/                      # 提示词模板（*.md）
│   ├── system.md                 # 主系统提示词模板
│   ├── think_required.md         # Think-gate 注入内容
│   ├── planning_required.md      # Planning-gate 注入内容
│   ├── strategy_change.md        # Strategy-change 反思内容
│   ├── auto_lesson.md            # 自动总结经验提示词
│   ├── session_to_memory.md      # 会话停用 → 记忆提取
│   ├── judge_system.md           # Judge 系统提示词
│   ├── judge_critical.md         # Judge 关键反馈注入
│   ├── teammate.md               # Teammate 系统提示词模板
│   ├── todo_nag.md               # Todo 提醒注入
│   ├── human_modify.md           # HITL 修改反馈
│   └── human_reject.md           # HITL 拒绝反馈
│
├── skills/                       # 技能定义（启动时加载）
│   ├── agent-builder/SKILL.md    # 元技能：构建 Agent
│   ├── code-review/SKILL.md      # 代码审查方法论
│   ├── judge/SKILL.md            # 评判/评估技能
│   └── pdf/SKILL.md              # PDF 处理技能
│
├── go.mod / go.sum               # Go 模块定义
└── .go-code-agent/               # 运行时状态目录（被 .gitignore 忽略）
    └── sessions/                 # 每会话数据（tasks、team、history 等）
```

---

## REPL 命令

| 命令 | 说明 |
|------|------|
| `/session` | 列出所有会话及其状态和最后活跃时间 |
| `/session new [title]` | 创建并切换到新会话 |
| `/session switch <id>` | 切换到已有会话（停用当前会话） |
| `/session rename [id] <title>` | 重命名会话 |
| `/session archive [id]` | 归档会话（从活跃列表中移除） |
| `/compact` | 手动触发对话压缩（LLM 摘要） |
| `/tasks` | 列出当前会话中所有任务及其状态 |
| `/dag` | 展示 DAG 执行计划（拓扑阶段） |
| `/decisions` | 显示当前会话的自主决策审计轨迹（decisions.jsonl） |
| `/team` | 列出活跃 teammates 及其状态（WORK/IDLE） |
| `/inbox` | 读取主导 Agent 的收件箱消息 |
| `/memory` | 显示记忆统计（常驻字符数、日常文件数、条目数） |
| `/search <query>` | 用混合 BM25+向量搜索记忆 |
| `/mcp` | 列出已连接的 MCP 服务器及其工具 |
| `/mcp connect <name> <cmd>` | 运行时连接新的 MCP 服务器 |
| `/mcp disconnect <name>` | 运行时断开某个 MCP 服务器 |
| `/usage` | Token 用量汇总（按来源、模型、会话） |
| `/approve [safe\|danger\|off]` | 切换工具调用的自动审批级别 |
| `/security` | 显示当前安全配置状态 |
| `/security test-bash <cmd>` | 对某条命令试跑 bash 白名单/危险模式检测 |

---

## 内置工具

### 核心（文件与 Shell）

| 工具 | 说明 |
|------|------|
| `bash` | 执行 Shell 命令（白名单门控，120s 超时） |
| `read_file` | 读取文件内容，可指定行数限制（优化：缓冲读取，内存高效） |
| `write_file` | 创建/覆盖文件（显示 diff 预览） |
| `edit_file` | 搜索/替换式精确编辑（优化：逐行处理，< 500KB 文件显示 diff） |
| `delete_file` | 删除文件（启用 HITL 时需要确认） |

**性能优化**：
- `read_file`：使用 `bufio.Scanner` 逐行读取，内存高效；到达行数限制时提前停止。
- `edit_file`：逐行处理文件，而非一次性加载整个文件到内存。仅对 < 500KB 的文件生成 diff 预览，避免大文件性能下降。
- 两个工具均使用动态缓冲区分配和提前退出策略以获得最佳性能。

### 推理

| 工具 | 说明 |
|------|------|
| `think` | 推理暂存区（不发送给用户，持久化在上下文中） |
| `compress` | 手动触发上下文压缩 |

### 规划与任务

| 工具 | 说明 |
|------|------|
| `TodoWrite` | 快速待办清单（简单扁平列表） |
| `task_create` | 创建带元数据的结构化任务 |
| `task_get` | 按 ID 获取任务详情 |
| `task_update` | 更新任务状态/描述 |
| `task_list` | 列出所有任务，可按状态过滤 |
| `task_add_dep` | 添加依赖边（A 必须在 B 之前完成） |
| `task_remove_dep` | 移除依赖边 |
| `task_ready` | 列出所有依赖已满足的任务 |
| `task_dag` | 以拓扑阶段形式渲染完整 DAG |

### 子 Agent 与技能

| 工具 | 说明 |
|------|------|
| `task` | 生成只读子 Agent 用于研究/分析（30 轮上限） |
| `load_skill` | 将技能定义加载到上下文中 |

### 后台执行

| 工具 | 说明 |
|------|------|
| `background_run` | 在后台运行长时间命令（非阻塞） |
| `check_background` | 检查后台任务的状态/输出 |

### 多 Agent 协作

| 工具 | 说明 |
|------|------|
| `spawn_teammate` | 创建具有特定角色的新 teammate Agent |
| `list_teammates` | 列出所有 teammates 及其状态 |
| `send_message` | 向指定 teammate 发送消息 |
| `read_inbox` | 读取主导 Agent 收件箱中的消息 |
| `broadcast` | 向所有活跃 teammates 发送消息 |
| `shutdown_request` | 请求 teammate 优雅关闭 |
| `plan_approval` | 批准/拒绝 teammate 的执行计划 |
| `claim_task` | Teammate 从 DAG 中认领一个就绪任务 |

### 记忆

| 工具 | 说明 |
|------|------|
| `memory_write` | 写入一条记忆条目（常驻或日常） |
| `memory_search` | 按查询搜索记忆 |
| `memory_delete` | 按 ID 删除记忆条目 |
| `session_save_memory` | 提取会话经验并保存到记忆 |

### MCP（动态）

MCP 工具在服务器连接时动态注册为 `mcp__<server>__<tool>`，遵循与内置工具相同的安全/HITL/超时流程。

---

## LLM 提供商

| 提供商 | 状态 | 流式 | 工具调用 | 模型 |
|--------|------|------|----------|------|
| Anthropic | 完整 | ✅ SSE | ✅ 原生 | claude-*（opus、sonnet、haiku） |
| OpenAI | 完整 | ✅ SSE | ✅ 原生 | gpt-*、o*、任何兼容端点 |
| Gemini | 占位 | ❌ | ❌ | gemini-*（预留） |

### 提供商选择逻辑

```
LLM_PROVIDER 环境变量已设置？
  → 是：直接使用该提供商
  → 否：从 MODEL_ID 前缀推断：
           "claude-*"  → anthropic
           "gemini-*"  → gemini
           其他         → openai（覆盖 gpt-*、o*、兼容接口）
```

### 重试策略

所有提供商共享相同的重试包装器：
- **最大重试次数**：5
- **退避策略**：指数退避（1s → 2s → 4s → 8s → 16s 上限 60s）
- **可重试**：429（速率限制）、500/502/503（服务器错误）、网络超时
- **不可重试**：400（错误请求）、401（认证失败）、404

---

## 安全模型

四层独立防护，默认全部启用：

### 1. Bash 策略（命令白名单）

```
用户查询 → Agent 调用 bash("rm -rf /") →
  BashPolicy.Validate(cmd):
    ├─ 白名单检查（ls、cat、grep、go、git 等）
    ├─ 危险模式检测（rm -rf、sudo 等）
    ├─ 如果危险 + HITL 已启用 → 提示审批
    └─ 如果被阻止 → 拒绝并附上解释
```

### 2. 路径沙箱

- 阻止访问敏感路径：`.ssh/`、`.aws/`、`.gnupg/` 等
- 防止符号链接逃逸（检查前解析真实路径）
- 所有文件工具（read/write/edit/delete）均经过此门控

### 3. 工具审批级别

| 级别 | 行为 |
|------|------|
| `auto` | 所有工具无需确认直接执行 |
| `safe` | 仅"安全"工具自动执行；其他工具提示确认 |
| `danger` | 所有工具均需确认 |
| `off` | 审批系统禁用 |

运行时使用 `/approve [level]` 切换。

### 4. 密钥脱敏器

扫描所有工具输出，匹配以下模式：
- API 密钥（`sk-*`、`key-*`、Bearer 令牌）
- AWS 凭证、私钥
- 含密码的数据库连接字符串
- 通用 `password=`、`secret=` 模式

检测到的密钥在添加到对话上下文之前被替换为 `[REDACTED]`。

### 额外安全特性

- **Diff 预览**：任何文件修改前在终端显示统一 diff
- **HITL 审批**：4 种模式（interactive/auto-approve/auto-reject/notify-only）
- **快照/回滚**：基于 git-stash 的 Saga 模式，用于写工具（通过 `SNAPSHOT_ENABLED=1` 选择启用）
- **每工具超时**：5 分钟硬性上限，防止挂起的处理器冻结 REPL

---

## Agentic 特性

### Think → Plan → Act → Reflect 循环

```
Round 0:  [think-gate] → 强制 Agent 先使用 `think` 工具
Round 1:  [planning-gate] → 强制创建任务（如果查询 > 80 字符）
Round 2+: [execution] → Agent 执行任务
每 5 轮:  [periodic reflection] → Agent 回顾进展
失败时:   [mini-reflect] → 对工具失败立即反思
3 次后:   [strategy-change] → 强制策略转向
10 轮后:  [stuck detection] → 通知 Agent 已卡住
100 轮:   [maxRounds] → 硬性停止
```

### DAG 调度器

任务可以有依赖边，形成有向无环图（DAG）：

```
task_create("设计 API")     → T1
task_create("实现 API")     → T2
task_create("编写测试")     → T3
task_add_dep(T2, T1)          → T2 依赖于 T1
task_add_dep(T3, T2)          → T3 依赖于 T2

task_dag() 输出：
  Stage 0: [T1: 设计 API]
  Stage 1: [T2: 实现 API]
  Stage 2: [T3: 编写测试]
```

调度器提供 `task_ready` 来列出所有依赖已完成的任务，使得 teammates 可以并行执行。

### 自动总结经验

完成工作后（≥3 轮工具调用），Agent 会被提示将经验教训写入记忆。这些经验通过混合搜索在 future 会话中被召回，实现跨会话学习。

### LLM-as-Judge

当设置了 `JUDGE_ENABLED` 时：
1. Agent 完成所有任务
2. 第二次 LLM 调用评估："Agent 是否真正达成了用户的目标？"
3. 评分 1-10；低于阈值（默认 7）→ 注入关键反馈 → Agent 重试
4. 每次 agentLoop 运行最多 2 次重试注入，防止无限循环

---

## 记忆系统

### 双层架构

```
┌─────────────────────────────────────────┐
│  常驻层（MEMORY.md）                     │
│  - 长期事实、偏好                        │
│  - 注入到每条系统提示词                   │
│  - 最多 8000 字符（超出则截断）          │
│  - 通过 memory_write 手动写入           │
└─────────────────────────────────────────┘

┌─────────────────────────────────────────┐
│  日常层（memory/daily/*.jsonl）          │
│  - 带时间戳的条目                       │
│  - 90 天 TTL（自动删除）                │
│  - 可通过混合搜索检索                   │
│  - 由会话经验自动填充                   │
└─────────────────────────────────────────┘
```

### 混合搜索

记忆召回使用双信号合并：

1. **BM25 关键词搜索**（权重 0.65）：基于 TF-IDF 的评分，可配置 k1/b 参数
2. **哈希向量相似度**（权重 0.35）：基于随机投影的词袋模型，用于语义近似

两个评分在加权合并前均归一化到 [0,1]。结果会去重（Jaccard > 0.7），Top-3 在每次 LLM 调用前注入系统提示词。

### 自动召回流程

```
用户消息 → 提取关键词 → 混合搜索 → Top-3 结果
  → 作为"相关记忆："段落注入系统提示词
  → LLM 看到来自过往会话的上下文
```

---

## 会话管理

### 生命周期

```
SessionManager.BootstrapOrCreate(forceNew, explicitID)
  → forceNew（且无显式 id）：NewSession("New session")
  → 否则：BootstrapSession 解析 显式 id > 最近活跃 > 全新会话
  → NewSession 创建目录：.go-code-agent/sessions/<uuid>/
    子目录：tasks/、team/、history/、transcripts/
  │
AppContext.ActivateSession(session)
  → SessionManager.Activate：绑定 session，落盘为索引中的活跃会话
  → 用该会话的 Bus/TaskMgr/DagSched/Protocols 重建 TeammateManager
  → 为当前活跃会话重新生成 System（系统提示词）
  │
Work（多轮）
  → 历史以 JSONL 追加（每次写入 fsync）
  → 任务以 JSON 文件持久化
  │
AppContext.DeactivateActiveSession()
  → ShutdownTeammates：关闭所有运行中的 teammate
  → SessionManager.Deactivate：session_to_memory 提示词提取经验，
    保存到 MemoryStore，更新会话索引（最后活跃时间戳）
  │
Archive(session)
  → 在索引中标记为已归档
  → 数据保留但从 /session 列表中隐藏
```

### 历史持久化

- **格式**：Append-only JSONL（每行一个 JSON 对象）
- **崩溃安全**：每次写入后 fsync；恢复时修剪尾部悬挂的 tool_calls
- **压缩**：基于 Checkpoint —— 当 autoCompact 运行时，它写入一个包含 LLM 摘要的 checkpoint 条目，随后的 LoadRuntime 从最新 checkpoint 开始
- **类型**：system、user、assistant、tool、checkpoint

---

## 多 Agent 协作

### TeammateManager

每个 teammate 是一个自治 Agent，拥有自己的：
- 对话上下文（与主导 Agent 分离）
- WORK/IDLE 状态机
- 收件箱（通过 MessageBus）
- 对共享 TaskManager 和 DAGScheduler 的访问权

### 状态机

```
IDLE ──(收到消息)──→ WORK
  │                            │
  │ (IdleTimeout=60s)          │ (任务完成 / 无更多工作)
  ▼                            ▼
SHUTDOWN                     IDLE
```

### 通信协议

```
主导 Agent ──send_message──→ Teammate 收件箱（JSONL）
主导 Agent ──broadcast────→ 所有 teammate 收件箱
Teammate ──send_message──→ 主导 Agent 收件箱
Teammate ──plan_approval──→ ProtocolStore（TTL=30min）
主导 Agent ──plan_approval──→ 批准/拒绝（TTL=24h）
```

### 计划审批协议

Teammates 在执行写操作前必须提交执行计划：
1. Teammate 创建计划 → 存储在 ProtocolStore 中，TTL 30 分钟
2. 主导 Agent 在收件箱中收到通知
3. 主导 Agent 通过 `plan_approval` 工具审查并批准/拒绝
4. Teammate 仅在批准后继续

### DAG 感知自动认领

空闲的 teammates 自动从共享 DAG 面板中认领就绪任务（依赖已满足），无需显式的主导 Agent 协调即可实现并行执行。

---

## MCP 集成

### 配置

在工作区根目录创建 `.mcp.json`：

```json
{
  "servers": {
    "my-server": {
      "command": "node",
      "args": ["path/to/server.js"],
      "env": {"API_KEY": "value"}
    },
    "python-tools": {
      "command": "python",
      "args": ["-m", "my_mcp_server"],
      "env": {}
    }
  }
}
```

### 工作原理

1. 启动时，`MCPManager` 读取 `.mcp.json` 并作为子进程启动每个服务器
2. 通过 stdio（stdin/stdout）上的 JSON-RPC 2.0 进行通信
3. 通过 `initialize` + `tools/list` 调用发现服务器能力
4. 工具在工具注册表中注册为 `mcp__<server>__<tool>`
5. 它们经过与内置工具相同的安全/HITL/超时流程

### 断路器

每个 MCP 服务器都有一个断路器：
- **CLOSED**（正常）：调用正常通过
- **OPEN**（连续失败 3 次后）：调用拒绝，进入 30 秒冷却
- **HALF-OPEN**（冷却后）：下一次调用为探测；成功 → CLOSED，失败 → OPEN

### 运行时管理

```
/mcp                        # 列出所有服务器 + 状态 + 工具数量
/mcp connect <name> <cmd>   # 运行时连接新服务器
```

---

## Token 管理

### microCompact（每轮）

将旧的工具结果消息折叠为简短摘要，仅保留最近 N 条（默认 15 条）完整内容。这防止了长时间使用工具期间上下文无限增长。

### autoCompact（阈值触发）

当估计的总 token 数超过 300K 时：
1. 将完整对话发送给 LLM，附带"总结"指令
2. 用一条 checkpoint 消息替换所有消息（system + 最近除外）
3. Checkpoint 持久化到历史 JSONL 以供崩溃恢复
4. Token 数量急剧下降；Agent 在完整上下文感知下继续

### Token 估算

使用简单启发式：`len(content) / 4`（近似 GPT tokenization）。每 3 轮重新检查一次，避免每次迭代都产生 O(N) 开销。

---

## 用量追踪

每次 LLM 调用都记录到 `memory/usage.jsonl`，包含：

| 字段 | 说明 |
|------|------|
| `trace_id` | 将一个用户请求的所有调用关联起来 |
| `session_id` | 产生此调用的会话 |
| `source` | `agent` / `judge` / `subagent` / `team` / `compress` / `memory-save` |
| `provider` | `openai` / `anthropic` |
| `model` | 实际使用的模型 ID |
| `input_tokens` | 消耗的提示词 token 数 |
| `output_tokens` | 生成的完成 token 数 |
| `duration_ms` | 调用的墙上时钟时间 |
| `timestamp` | ISO 8601 时间戳 |

使用 `/usage` REPL 命令查看聚合统计。

---

## 依赖项

| 包 | 用途 |
|----|------|
| `github.com/anthropics/anthropic-sdk-go` | Anthropic API 客户端 |
| `github.com/openai/openai-go` | OpenAI API 客户端 |
| `github.com/chzyer/readline` | 带历史的Interactive REPL |
| `github.com/tidwall/gjson` | 快速 JSON 路径查询 |
| `github.com/tidwall/sjson` | JSON 变更 |
| `github.com/invopop/jsonschema` | 工具定义的 JSON Schema 生成 |
| `golang.org/x/sync` | 并发原语（errgroup） |

---

## 示例工作流

```
$ JUDGE_ENABLED=1 ./agent --human

> 实现一个用户管理的 REST API，包含 CRUD 端点

[think] 分析需求：REST API、用户 CRUD、需要规划...
[task_create] "设计 API Schema" → T1
[task_create] "实现 Handlers" → T2（依赖于 T1）
[task_create] "编写测试" → T3（依赖于 T2）
[task_create] "添加文档" → T4（依赖于 T2）

[正在处理 T1：设计 API Schema]
  bash: go mod init user-api
  write_file: internal/model/user.go
  write_file: internal/handler/user.go（骨架）

[T1 完成 → T2 就绪]
[正在处理 T2：实现 Handlers]
  edit_file: internal/handler/user.go（完整实现）
  bash: go build ./...  ✓

[T2 完成 → T3、T4 就绪]
[正在处理 T3：编写测试]
  write_file: internal/handler/user_test.go
  bash: go test ./... -v  ✓

[所有任务完成]
[judge] 评估完成度... 评分：9/10 ✓
[auto-lesson] 将经验写入记忆供未来参考
[memory_write] "REST API 模式：model → handler → router → test"

完成。
```
