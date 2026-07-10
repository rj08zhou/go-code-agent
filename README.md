# go-code-agent

An autonomous coding agent built in Go featuring multi-round planning, self-reflection, multi-agent collaboration, persistent memory, and MCP (Model Context Protocol) integration.

## Table of Contents

- [Quick Start](#quick-start)
- [Architecture](#architecture)
- [Execution Flow](#execution-flow)
- [Configuration](#configuration)
- [File Layout](#file-layout)
- [REPL Commands](#repl-commands)
- [Built-in Tools](#built-in-tools)
- [LLM Providers](#llm-providers)
- [Security Model](#security-model)
- [Web Access & SSRF Protection](#web-access--ssrf-protection)
- [Agentic Features](#agentic-features)
- [Memory System](#memory-system)
- [Session Management](#session-management)
- [Multi-Agent Collaboration](#multi-agent-collaboration)
- [MCP Integration](#mcp-integration)
- [Token Management](#token-management)
- [Usage Tracking](#usage-tracking)
- [Dependencies](#dependencies)

---

## Quick Start

### Prerequisites

- Go 1.25+ installed
- At least one LLM API key configured

### Installation

```bash
git clone <repo-url>
cd go-code-agent
go build -o agent ./cmd/agent/
```

### Running

```bash
# Default: Anthropic (claude-*)
export ANTHROPIC_API_KEY="sk-ant-..."
./agent

# OpenAI
export OPENAI_API_KEY="sk-..."
export MODEL_ID="gpt-4o"
./agent

# OpenAI-compatible (e.g. Zhipu GLM, DeepSeek, local Ollama)
export OPENAI_API_KEY="<key>"
export OPENAI_BASE_URL="https://open.bigmodel.cn/api/paas/v4"
export MODEL_ID="glm-4.7-flash"
./agent

# Force provider regardless of MODEL_ID prefix
export LLM_PROVIDER="anthropic"  # openai | anthropic | gemini
./agent

# Enable LLM-as-Judge with default settings
export JUDGE_ENABLED=1
./agent

# Enable Judge with custom model and threshold
export JUDGE_ENABLED=1
export JUDGE_MODEL=claude-haiku-4.5
export JUDGE_MIN_SCORE=8
./agent

# Use a separate LLM service for judging (e.g., cheaper/faster model)
export JUDGE_ENABLED=1
export JUDGE_PROVIDER=openai
export JUDGE_API_KEY="<judge-api-key>"
export JUDGE_BASE_URL="https://api.deepseek.com"
export JUDGE_MODEL="deepseek-chat"
./agent
```

### Session Management

```bash
./agent                          # Resume most recent session
./agent --new-session            # Start a fresh session
./agent --session <id>           # Resume a specific session
```

### Optional Features

```bash
# LLM-as-Judge: post-completion verification
# (See "Running" section above for full configuration examples)
JUDGE_ENABLED=1 ./agent

# Human-in-the-loop approval for high-risk operations
./agent --human
./agent --human --human-mode auto-approve    # CI-friendly

# Git-stash-based snapshot/rollback for write tools
SNAPSHOT_ENABLED=1 ./agent
```

**💡 Judge Configuration**: For detailed judge setup (custom model, separate endpoint, etc.), refer to the [Running](#running) section which shows full `export` command examples.

All persistent state is stored under `{workdir}/.go-code-agent/`. Sessions survive across restarts and crashes.

---

## Architecture

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
│  openai / anthropic /    │   │  34 built-in + mcp__* tools        │
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

### Component Ownership

`AppContext` (`internal/agent/app.go`) is the single root object holding
every piece of process-/session-scoped state; `main.go` only ever
constructs one (`agent.NewApp(model, workdir, bashValidate)`) and reaches
everything else through it — no subsystem lives as a bare package
variable outside the tool registry.

- **Process-wide config** (fields on `AppContext`, built once by `NewApp`): `Model`, `Workdir`, `System` (rebuilt per-session)
- **Workdir-global subsystems** (fields on `AppContext`, persist for process lifetime): `Skills`, `MemStore`, `MCPMgr`, `PromptLoader`, `SessionManager`, `Snapshot`, `Judge`
- **Per-session** (rebound on session switch, reached via `SessionManager.Active()`): `TaskManager`, `DAGScheduler`, `MessageBus`, `ProtocolStore`, `HistoryStore`, `TeammateManager`
- **Compile-time capability table** (package-level, not per-instance): `ToolDefs` / `ToolHandlers` / `ToolSecurityMap`, populated once by `InitTools()`

Lifecycle is driven by three `AppContext` methods, each collapsing a
sequence that used to be duplicated across `main.go` and
`repl_commands.go`:
- `NewApp(model, workdir, bashValidate)` — constructs every workdir-global subsystem in one call
- `ActivateSession(sess)` — binds `sess` into `SessionManager`, rebuilds `TeamMgr`, regenerates `System`
- `DeactivateActiveSession()` — shuts down teammates, flushes the active session to memory

---

## Execution Flow

### Single User Message Lifecycle

```
user msg
  │
  ├─ buildSystemPrompt()         ← system.md template + skills + evergreen memory + tasks
  │      (evergreen MEMORY.md loaded once at session start; daily memories
  │       recalled on-demand by the model via the memory_search tool)
  │
  ▼
agentLoop(ctx, &conv)
  │
  ├─ [preRound]
  │    ├─ microCompact: collapse old tool results to summaries
  │    ├─ tokenCheck: if > 300K tokens → autoCompact (LLM summarization)
  │    ├─ drain background results → inject as tool messages
  │    └─ drain team inbox → inject as user messages
  │
  ├─ [gates]
  │    ├─ think-gate (round 0): force `think` tool before planning
  │    ├─ planning-gate (round 1): force task creation before execution
  │    └─ write-gate: require plan approval before file writes (teammates)
  │
  ├─ LLM stream call → parse response
  │    ├─ text only → check auto-lesson → done
  │    └─ tool_calls[] → dispatch each:
  │         ├─ security check (bash policy, path sandbox)
  │         ├─ HITL approval gate (if enabled)
  │         ├─ snapshot pre-state (if enabled)
  │         ├─ timeout wrapper (5 min ceiling)
  │         ├─ execute handler → collect result
  │         ├─ secrets sanitizer on output
  │         └─ snapshot rollback on failure (if enabled)
  │
  ├─ [post-dispatch]
  │    ├─ planningGate: check if tasks exist after first tool round
  │    ├─ judge: if all tasks done + JUDGE_ENABLED → LLM verification
  │    └─ reflection triggers:
  │         ├─ mini-reflect: on tool failure
  │         ├─ strategy-change: after 3 consecutive failures
  │         ├─ stuck detection: 10 rounds without progress
  │         └─ periodic full reflection: every 5 rounds
  │
  └─ loop until: text-only response | maxRounds(100) | stuck
```

### Multi-Round Conversation

```
Session Boot:
  HistoryStore.LoadRuntime() → restore messages from JSONL
  ↓
Each Turn:
  user input → agentLoop (N rounds) → persist new messages
  ↓
Session End:
  Deactivate → session_to_memory prompt → save lessons to MemoryStore
```

---

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MODEL_ID` | `claude-opus-4.7` | Model identifier (also infers provider from prefix) |
| `LLM_PROVIDER` | auto-infer | Force provider: `openai` \| `anthropic` \| `gemini` |
| `ANTHROPIC_API_KEY` | — | Required for claude-* models |
| `ANTHROPIC_BASE_URL` | SDK default | Gateway/proxy override for Anthropic |
| `OPENAI_API_KEY` | — | Required for gpt-*/o*/compatible models |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Proxy/local model endpoint |
| `SNAPSHOT_ENABLED` | `0` | Set `1` to enable git-stash-based rollback |
| `WEB_ALLOW_PRIVATE_IPS` | `0` | Set `1` to let `web_fetch`/`web_search` reach private/internal network addresses (default: blocked) |
| `WEB_SEARCH_PROVIDER` | auto | Force a search backend: `tavily` \| `brave` (requires `WEB_SEARCH_API_KEY`); unset = auto downgrade chain |
| `WEB_SEARCH_API_KEY` | — | API key for the forced `WEB_SEARCH_PROVIDER` |
| `SEARXNG_URL` | — | A specific/trusted SearXNG instance, tried alone instead of the public instance list |
| `SEARXNG_INSTANCES` | built-in list | Comma-separated override of the public SearXNG instances tried in the downgrade chain |

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--session <id>` | — | Activate a specific session by ID |
| `--new-session` | false | Force creation of a new session |
| `--human` | false | Enable Human-in-the-loop approval |
| `--human-mode` | interactive | `interactive` / `auto-approve` / `auto-reject` / `notify-only` |

### LLM-as-Judge Env Vars

The judge is configured entirely through environment variables (no CLI flags). The judge can use a separate LLM backend with its own model, API key, and endpoint:

| Env Var | Default | Description |
|---------|---------|-------------|
| `JUDGE_ENABLED` | unset | Enable the judge (`1` / `true` / `yes` / `on`) |
| `JUDGE_MODEL` | main model | Model id for judge calls (empty = reuse main model) |
| `JUDGE_MIN_SCORE` | 7 | Score below this triggers a retry (scale 1-10) |
| `JUDGE_PROVIDER` | inferred | Explicit backend SDK (`openai` / `anthropic` / `gemini`) |
| `JUDGE_API_KEY` | main key | Judge-only key (else the backend's standard key) |
| `JUDGE_BASE_URL` | main url | Judge-only endpoint (else the backend's standard url) |

**Judge Provider Resolution:**

The judge uses `llm.JudgeProvider()` to dynamically select the LLM backend at runtime:

1. **No JUDGE_* vars set** → Uses the same provider as the main agent (preserves backward compatibility)
2. **JUDGE_API_KEY or JUDGE_BASE_URL set** → Creates an isolated provider instance with dedicated credentials
3. **Only JUDGE_PROVIDER set** → Reuses the shared provider instance of that backend type

This allows the judge to run on a different (often cheaper) model or even a completely separate LLM service without affecting the main agent's configuration.

### Tunable Constants (infra/consts.go)

All thresholds are centralized in one file for easy tuning:

| Category | Constant | Value | Description |
|----------|----------|-------|-------------|
| **Loop** | MaxRounds | 100 | Hard safety cap for agent loop |
| | StuckThreshold | 20 | Rounds without progress = stuck |
| | ReflectInterval | 20 | Periodic reflection every N rounds |
| | MaxConsecutiveFailures | 3 | Same tool failing → strategy change |
| | LessonThreshold | 3 | Min rounds before auto-lesson |
| | SubagentMaxRounds | 30 | Subagent inner loop cap |
| | TeammateWorkMaxRounds | 50 | Teammate work phase cap |
| | DefaultMaxOutputTokens | 16,384 | Default max output tokens per LLM call |
| **Tokens** | TokenThreshold | 300,000 | autoCompact trigger |
| | KeepRecent | 15 | microCompact keeps N recent tool msgs |
| | MaxOutputLen | 500,000 | Max bytes per tool output (500KB) |
| | TokenCheckInterval | 3 | Re-check tokens every N rounds |
| **Timing** | PerToolTimeout | 5 min | Hard ceiling per tool handler |
| | BashTimeout | 120s | bash / background_run timeout |
| | LlmMaxRetries | 5 | LLM call retry attempts |
| | LlmBaseDelay | 1s | Exponential backoff base |
| | LlmRateLimitDelay | 10s | 429-specific base backoff |
| | LlmMaxDelay | 60s | Exponential backoff cap |
| | LlmCallTimeout | 5 min | Wall-clock cap per provider Call/Stream attempt |
| | LlmHTTPTimeout | 6 min | Underlying HTTP transport timeout (backstop) |
| | LlmDefaultMaxQPS | 2.0 | Process-wide LLM throttle (requests/sec) |
| | LlmDefaultMaxBurst | 4 | Token-bucket burst capacity |
| | LlmDefaultMaxConcurrency | 2 | Max in-flight LLM calls process-wide |
| | SpawnMinInterval | 750ms | Stagger between teammate/subagent spawns |
| **Memory** | MemoryTTLDays | 90 | Daily files auto-deleted after this |
| | MaxEvergreenChars | 8,000 | MEMORY.md injection truncation |
| | DeduplicateThreshold | 0.7 | Jaccard similarity for dedup |
| | MaxMemoryContentLen | 2,000 | Max chars per memory entry |
| **Search** | Bm25K1 | 1.5 | TF saturation parameter |
| | Bm25B | 0.75 | Length normalization parameter |
| | HybridKeywordWeight | 0.65 | BM25 weight in hybrid merge |
| | HybridVectorWeight | 0.35 | Vector weight in hybrid merge |
| **Team** | PollInterval | 5s | Idle teammate inbox poll interval |
| | IdleTimeout | 60s | Idle teammate auto-shutdown |
| | MaxTeamMessageSize | 64 KB | Prevents inbox flooding |
| | PlanRequestTTL | 30 min | Pending plan requests expire |
| | ApprovedPlanTTL | 24h | Approved plans expire |
| **Judge** | JudgeMinScore | 7 | Retry threshold (1-10) |
| | JudgeMaxRetryInjects | 2 | Max verification-failed retries |
| **Planning** | PlanningGateMinTaskChars | 80 | Skip gate for trivial queries |
| | LessonRoundsLimit | 3 | Max extra rounds after lesson |
| **Web** | WebFetchTimeout | 20s | `web_fetch` request+redirects timeout |
| | WebFetchMaxBytes | 2 MB | `web_fetch` response body cap |
| | WebSearchTimeout | 8s | Per-backend timeout in the `web_search` downgrade chain |

---

## File Layout

```
go-code-agent/
├── cmd/agent/                    # Application layer (package main) - composition root only
│   ├── main.go                   # Entry point: flags, wiring, REPL loop, SIGINT cleanup
│   └── repl_commands.go          # Slash-command dispatcher (/session, /tasks, etc.)
│
├── internal/                     # Reusable infrastructure + the agent engine
│   ├── agent/                    # The agent engine (package agent)
│   │   ├── app.go                #   AppContext: root object + NewApp/ActivateSession/DeactivateActiveSession
│   │   ├── loop.go               #   Core multi-round execution loop (Run)
│   │   ├── plan.go               #   Think-gate + planning-gate logic
│   │   ├── reflection.go         #   Mini-reflect, strategy-change, stuck detection
│   │   ├── judge.go              #   LLM-as-Judge post-completion verifier
│   │   ├── decisions.go          #   Autonomous-decision audit trail (decisions.jsonl, /decisions)
│   │   ├── compression.go        #   microCompact + autoCompact (token management)
│   │   ├── subagent.go           #   Read-only sub-agent spawner (task tool)
│   │   ├── team.go               #   TeammateManager: WORK/IDLE autonomous loop
│   │   ├── tool_registry.go      #   Tool definitions (30+ tools registered here)
│   │   ├── tool_base.go          #   Base tool handlers (bash, files, think, etc.)
│   │   ├── web_tools.go          #   web_fetch/web_search tools (formatting + secrets redaction)
│   │   ├── system_prompt.go      #   System prompt assembly + memory recall
│   │   ├── security.go           #   Tool security registry, checkToolApproval, HITL gate glue
│   │   ├── snapshot.go           #   Saga-pattern snapshot/rollback via git-stash
│   │   └── log_file.go           #   Per-session file logging (session.log)
│   ├── llm/                      # LLM abstraction layer
│   │   ├── llm_types.go          #   Neutral types: Message, ToolCall, ToolDef, Role
│   │   ├── llm_client.go         #   Retry wrapper, trace ID, streaming interface
│   │   ├── provider.go           #   Provider registry + PickProvider logic
│   │   ├── provider_openai.go    #   OpenAI/compatible backend (streaming + tools)
│   │   ├── provider_anthropic.go #   Anthropic backend (streaming + tools)
│   │   ├── provider_gemini.go    #   Gemini stub (placeholder)
│   │   └── tool_helpers.go       #   Tool parameter extraction helpers (MkOk/MkErr/ParseArgs)
│   ├── session/                  # Session lifecycle
│   │   ├── session.go            #   Per-session aggregate (owns task/team/history)
│   │   └── session_manager.go    #   CRUD, BootstrapOrCreate, activate/deactivate, index
│   ├── history/                  # Conversation persistence
│   │   └── history.go            #   Append-only JSONL + checkpoint compaction
│   ├── hitlaudit/                # Human-in-the-loop + audit
│   │   ├── hitlaudit.go          #   Audit log (JSONL) + approval gate
│   │   └── human_approval.go     #   4-mode approval logic + risk classification
│   ├── security/                 # Security primitives (no internal/agent dependency)
│   │   ├── approval.go           #   ApprovalLevel + ApprovalState (auto/safe/danger gating)
│   │   ├── bash_policy.go        #   Command allowlist + danger-pattern detection
│   │   ├── path_sandbox.go       #   SecurePath: traversal/symlink-escape prevention
│   │   ├── secrets.go            #   SecretsSanitizer: redact API keys/tokens/private keys
│   │   ├── permissions.go        #   User-editable permissions.json (tool+pattern allow/deny/ask)
│   │   └── ssrf.go               #   SSRF policy: private/internal IP blocklist (see Web Access section)
│   ├── web/                      # Outbound web access for web_fetch/web_search
│   │   ├── client.go             #   SSRF-hardened http.Client (dial-time IP check, redirect guard)
│   │   ├── fetch.go               #   web_fetch: GET + content-type dispatch
│   │   ├── html.go               #   HTML → readable text extraction (golang.org/x/net/html)
│   │   └── search.go             #   Searcher interface + DDG/SearXNG/Tavily/Brave + downgrade chain
│   ├── task/                     # Task management
│   │   ├── task.go               #   TaskManager: CRUD, file persistence
│   │   ├── task_scheduler.go     #   DAGScheduler: topological sort, stage execution
│   │   └── todo.go               #   Simple TodoList (legacy, pre-DAG)
│   ├── team/                     # Multi-agent infrastructure
│   │   ├── team_bus.go           #   MessageBus: JSONL inbox per agent, dead-letter
│   │   └── team_protocols.go     #   ProtocolStore: plan approval with TTL
│   ├── memory/                   # Persistent memory
│   │   └── memory.go             #   Two-layer store + BM25 + hash-vector search
│   ├── mcp/                      # Model Context Protocol
│   │   ├── mcp.go                #   MCPManager: server lifecycle, circuit breaker
│   │   └── mcp_jsonrpc.go        #   JSON-RPC 2.0 over stdio transport
│   ├── prompt/                   # Template engine
│   │   └── prompt.go             #   Loader + renderer ({{.Var}} substitution)
│   ├── skill/                    # Skill system
│   │   └── skills.go             #   SkillLoader: reads SKILL.md files
│   ├── background/               # Background task runner
│   │   └── background.go         #   Goroutine pool + result collection
│   ├── usage/                    # Token usage telemetry
│   │   └── usage.go              #   UsageRecorder: per-call JSONL logging
│   └── logging/                  # Terminal output
│       └── log.go                #   Colored output helpers (system/error/tool/agent)
│
├── infra/                        # Cross-cutting constants
│   └── consts.go                 # All tunable thresholds in one place
│
├── utils/                        # Shared utilities
│   └── utils.go                  # Path helpers (JoinWorkdir, Truncate, etc.)
│
├── prompts/                      # Prompt templates (*.md)
│   ├── system.md                 # Main system prompt template
│   ├── think_required.md         # Think-gate injection
│   ├── planning_required.md      # Planning-gate injection
│   ├── strategy_change.md        # Strategy-change reflection
│   ├── auto_lesson.md            # Auto-lesson prompt
│   ├── session_to_memory.md      # Session deactivation → memory extraction
│   ├── judge_system.md           # Judge system prompt
│   ├── judge_critical.md         # Judge critical feedback injection
│   ├── teammate.md               # Teammate system prompt template
│   ├── todo_nag.md               # Todo reminder injection
│   ├── human_modify.md           # HITL modification feedback
│   └── human_reject.md           # HITL rejection feedback
│
├── skills/                       # Skill definitions (loaded at startup)
│   ├── agent-builder/SKILL.md    # Meta-skill: building agents
│   ├── code-review/SKILL.md      # Code review methodology
│   ├── judge/SKILL.md            # Judging/evaluation skill
│   └── pdf/SKILL.md              # PDF processing skill
│
├── go.mod / go.sum               # Go module definition
└── .go-code-agent/               # Runtime state directory (gitignored)
    └── sessions/                 # Per-session data (tasks, team, history, etc.)
```

---

## REPL Commands

| Command | Description |
|---------|-------------|
| `/session` | List all sessions with status and last-active time |
| `/session new [title]` | Create and switch to a new session |
| `/session switch <id>` | Switch to an existing session (deactivates current) |
| `/session rename [id] <title>` | Rename a session |
| `/session archive [id]` | Archive a session (removes from active list) |
| `/compact` | Manually trigger conversation compaction (LLM summarization) |
| `/tasks` | List all tasks in current session with status |
| `/dag` | Show DAG execution plan (topological stages) |
| `/decisions` | Show the active session's autonomous-decision audit trail (decisions.jsonl) |
| `/team` | List active teammates with state (WORK/IDLE) |
| `/inbox` | Read the lead agent's inbox messages |
| `/memory` | Show memory statistics (evergreen chars, daily files, entries) |
| `/search <query>` | Search memories with hybrid BM25+vector |
| `/mcp` | List connected MCP servers and their tools |
| `/mcp connect <name> <cmd>` | Connect a new MCP server at runtime |
| `/mcp disconnect <name>` | Disconnect an MCP server at runtime |
| `/usage` | Token usage summary (by source, model, session) |
| `/approve [safe\|danger\|off]` | Toggle auto-approval level for tool calls |
| `/security` | Show current security configuration status |
| `/security test-bash <cmd>` | Dry-run a command through the bash allowlist/danger-pattern policy |

---

## Built-in Tools

### Core (File & Shell)

| Tool | Description |
|------|-------------|
| `bash` | Execute shell commands (allowlist-gated, 120s timeout) |
| `read_file` | Read file contents with optional line limit (optimized: buffered reading, memory-efficient) |
| `write_file` | Create/overwrite a file (diff preview shown) |
| `edit_file` | Surgical edit with search/replace (optimized: line-by-line processing, diff shown for files < 500KB) |
| `delete_file` | Delete a file (requires confirmation if HITL enabled) |

**Performance Optimizations**:
- `read_file`: Uses `bufio.Scanner` for memory-efficient line-by-line reading. Stops early when line limit is reached.
- `edit_file`: Processes files line-by-line instead of loading entire file into memory. Only generates diff preview for files < 500KB to avoid performance degradation on large files.
- Both tools use dynamic buffer allocation and early-exit strategies for optimal performance.

### Reasoning

| Tool | Description |
|------|-------------|
| `think` | Scratchpad for reasoning (not sent to user, persisted in context) |
| `compress` | Manually trigger context compression |

### Planning & Tasks

| Tool | Description |
|------|-------------|
| `TodoWrite` | Quick todo list (simple flat list) |
| `task_create` | Create a structured task with metadata |
| `task_get` | Get task details by ID |
| `task_update` | Update task status/description |
| `task_list` | List all tasks with optional status filter |
| `task_add_dep` | Add dependency edge (A must finish before B) |
| `task_remove_dep` | Remove a dependency edge |
| `task_ready` | List tasks whose dependencies are all satisfied |
| `task_dag` | Render the full DAG as topological stages |

### Sub-agent & Skills

| Tool | Description |
|------|-------------|
| `task` | Spawn a read-only sub-agent for research/analysis (30 round cap) |
| `load_skill` | Load a skill definition into context |

### Background Execution

| Tool | Description |
|------|-------------|
| `background_run` | Run a long command in background (non-blocking) |
| `check_background` | Check status/output of background tasks |

### Web Access

| Tool | Description |
|------|-------------|
| `web_fetch` | Fetch a public URL and return its readable text (HTML → plain text; blocked for private/internal addresses by default) |
| `web_search` | Search the web via a zero-config downgrade chain (SearXNG → DuckDuckGo), or a paid backend (Tavily/Brave) if configured |

See [Web Access & SSRF Protection](#web-access--ssrf-protection) for the security model behind these two tools.

### Multi-Agent Collaboration

| Tool | Description |
|------|-------------|
| `spawn_teammate` | Create a new teammate agent with a role |
| `list_teammates` | List all teammates and their states |
| `send_message` | Send a message to a specific teammate |
| `read_inbox` | Read messages from the lead's inbox |
| `broadcast` | Send a message to all active teammates |
| `shutdown_request` | Request a teammate to shut down gracefully |
| `plan_approval` | Approve/reject a teammate's execution plan |
| `claim_task` | Teammate claims a ready task from the DAG |

### Memory

| Tool | Description |
|------|-------------|
| `memory_write` | Write a memory entry (evergreen or daily) |
| `memory_search` | Search memories with query |
| `memory_delete` | Delete a memory entry by ID |
| `session_save_memory` | Extract and save session learnings to memory |

### MCP (Dynamic)

MCP tools are dynamically registered as `mcp__<server>__<tool>` when servers connect. They follow the same security/HITL/timeout pipeline as built-in tools.

---

## LLM Providers

| Provider | Status | Streaming | Tool Use | Models |
|----------|--------|-----------|----------|--------|
| Anthropic | Full | ✅ SSE | ✅ Native | claude-* (opus, sonnet, haiku) |
| OpenAI | Full | ✅ SSE | ✅ Native | gpt-*, o*, any compatible endpoint |
| Gemini | Stub | ❌ | ❌ | gemini-* (placeholder for future) |

### Provider Selection Logic

```
LLM_PROVIDER env set?
  → yes: use that provider directly
  → no:  infer from MODEL_ID prefix:
           "claude-*"  → anthropic
           "gemini-*"  → gemini
           otherwise   → openai (covers gpt-*, o*, compatible)
```

### Retry Strategy

All providers share the same retry wrapper:
- **Max retries**: 5
- **Backoff**: exponential (1s → 2s → 4s → 8s → 16s cap 60s)
- **Retryable**: 429 (rate limit), 500/502/503 (server errors), network timeouts
- **Non-retryable**: 400 (bad request), 401 (auth), 404

---

## Security Model

Four independent layers, all enabled by default:

### 1. Bash Policy (Command Allowlist)

```
User query → agent calls bash("rm -rf /") →
  BashPolicy.Validate(cmd):
    ├─ allowlist check (ls, cat, grep, go, git, etc.)
    ├─ danger pattern detection (rm -rf, sudo, etc.)
    ├─ if dangerous + HITL enabled → approval prompt
    └─ if blocked → reject with explanation
```

### 2. Path Sandbox

- Blocks access to sensitive paths: `.ssh/`, `.aws/`, `.gnupg/`, etc.
- Prevents symlink escape (resolves real path before checking)
- All file tools (read/write/edit/delete) pass through this gate

### 3. Tool Approval Levels

| Level | Behavior |
|-------|----------|
| `auto` | All tools execute without confirmation |
| `safe` | Only "safe" tools auto-execute; others prompt |
| `danger` | All tools prompt for confirmation |
| `off` | Approval system disabled |

Toggle at runtime with `/approve [level]`.

### 4. Secrets Sanitizer

Scans all tool output for patterns matching:
- API keys (`sk-*`, `key-*`, bearer tokens)
- AWS credentials, private keys
- Database connection strings with passwords
- Generic `password=`, `secret=` patterns

Detected secrets are replaced with `[REDACTED]` before being added to conversation context.

### Additional Security Features

- **Diff Preview**: Unified diff shown in terminal before any file modification
- **HITL Approval**: 4 modes (interactive/auto-approve/auto-reject/notify-only)
- **Snapshot/Rollback**: Git-stash-based Saga pattern for write tools (opt-in via `SNAPSHOT_ENABLED=1`)
- **Per-tool Timeout**: 5-minute hard ceiling prevents hung handlers from freezing the REPL
- **User Permission Rules**: `{workdir}/.go-code-agent/permissions.json` lets you allow/deny/ask per tool+pattern (e.g. allow `git commit -m *` but deny `git push --force*`) — layered *after* the hard bash danger-pattern blacklist, so a user `allow` rule can never resurrect a command the blacklist forbids. See `/permissions` and `/permissions reload`.

---

## Web Access & SSRF Protection

The `web_fetch` and `web_search` tools (see [Built-in Tools](#built-in-tools)) are the agent's only way to reach the public internet. Both are funneled through a single SSRF-hardened HTTP client (`internal/web/client.go`) so the security guarantee below holds regardless of what either tool does with the result.

### Default-deny private networks

Every outbound connection is checked **at dial time, against the actually-resolved IP** (not the hostname string) — this is what defeats DNS rebinding, where a domain's DNS answer can differ between "looks safe" and "connects somewhere private" between the first check and the actual connection.

| Address space | Default | Override |
|----------------|---------|----------|
| Loopback (`127.0.0.0/8`, `::1`), RFC1918 private (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), and `9.*`/`11.*`/`21.*`/`30.*` (explicitly listed in project security rules) | Blocked | `WEB_ALLOW_PRIVATE_IPS=1` |
| Link-local / cloud metadata (`169.254.0.0/16`, `fe80::/10`) — the address space real-world SSRF exploits target (e.g. stealing cloud IAM credentials via `169.254.169.254`) | **Always blocked** | No override — never opt-outable |

Additional guards: redirects are capped at 5 hops and non-`http(s)` redirect targets are rejected outright; response bodies are read through `io.LimitReader` (never fully buffered) and capped at `WebFetchMaxBytes` (2 MB default).

### web_search downgrade chain

`web_search` never requires configuration to be usable:

```
WEB_SEARCH_PROVIDER=tavily|brave + WEB_SEARCH_API_KEY set?
  → yes: use ONLY that backend (an explicitly configured paid backend
         is never silently downgraded)
  → no:  1. SEARXNG_URL if set (a trusted/self-hosted instance), tried alone
         2. else the built-in public SearXNG instance list
            (or SEARXNG_INSTANCES override), tried in order
         3. DuckDuckGo Lite (zero-key, always available) as the final fallback
         4. all backends failed → explicit error, never a silent empty result
```

### Untrusted content handling

`web_fetch`'s extracted page text is wrapped in an explicit `BEGIN/END UNTRUSTED PAGE CONTENT` marker before being added to the conversation — the model is instructed to treat it as data to read, never as instructions to follow (defense against prompt injection from a fetched page). Both tools also run their output through the same `SecretsSanitizer` used for bash/file output, since a fetched page could echo back something that looks like a credential.

---

## Agentic Features

### Think → Plan → Act → Reflect Cycle

```
Round 0:  [think-gate] → forces agent to use `think` tool first
Round 1:  [planning-gate] → forces task creation (if query > 80 chars)
Round 2+: [execution] → agent works through tasks
Every 5:  [periodic reflection] → agent reviews progress
On fail:  [mini-reflect] → immediate reflection on tool failure
After 3:  [strategy-change] → forced strategy pivot
After 10: [stuck detection] → agent notified it's stuck
Round 100: [maxRounds] → hard stop
```

### DAG Scheduler

Tasks can have dependency edges forming a Directed Acyclic Graph:

```
task_create("Design API")     → T1
task_create("Implement API")  → T2
task_create("Write tests")    → T3
task_add_dep(T2, T1)          → T2 depends on T1
task_add_dep(T3, T2)          → T3 depends on T2

task_dag() output:
  Stage 0: [T1: Design API]
  Stage 1: [T2: Implement API]
  Stage 2: [T3: Write tests]
```

The scheduler provides `task_ready` to list tasks whose dependencies are all completed, enabling parallel execution by teammates.

### Auto-Lesson

After completing work (≥3 tool rounds), the agent is prompted to write lessons-learned to memory. These lessons are recalled in future sessions via hybrid search, enabling cross-session learning.

### LLM-as-Judge

When `JUDGE_ENABLED` is set:
1. Agent completes all tasks
2. A second LLM call evaluates: "Did the agent actually achieve the user's goal?"
3. Scores 1-10; below threshold (default 7) → critical feedback injected → agent retries
4. Max 2 retry injections per agentLoop run to prevent infinite loops

---

## Memory System

### Two-Layer Architecture

```
┌─────────────────────────────────────────┐
│  Evergreen Layer (MEMORY.md)            │
│  - Long-lived facts, preferences        │
│  - Injected into EVERY system prompt    │
│  - Max 8000 chars (truncated if larger) │
│  - Manual write via memory_write        │
└─────────────────────────────────────────┘

┌─────────────────────────────────────────┐
│  Daily Layer (memory/daily/*.jsonl)     │
│  - Timestamped entries                  │
│  - 90-day TTL (auto-deleted)           │
│  - Searchable via hybrid search         │
│  - Auto-populated by session lessons    │
└─────────────────────────────────────────┘
```

### Hybrid Search

Memory recall uses a two-signal merge:

1. **BM25 Keyword Search** (weight 0.65): TF-IDF-based scoring with configurable k1/b parameters
2. **Hash-Vector Similarity** (weight 0.35): Random-projection bag-of-words for semantic approximation

Both scores are normalized to [0,1] before weighted merge. Results are deduplicated (Jaccard > 0.7) and top-3 are injected into the system prompt before each LLM call.

### Auto-Recall Flow

```
user message → extract keywords → hybrid search → top-3 results
  → inject as "Relevant memories:" section in system prompt
  → LLM sees context from past sessions
```

---

## Session Management

### Lifecycle

```
SessionManager.BootstrapOrCreate(forceNew, explicitID)
  → forceNew (no explicit id): NewSession("New session")
  → otherwise: BootstrapSession resolves explicit id > most recent > fresh
  → NewSession creates directory: .go-code-agent/sessions/<uuid>/
    subdirs: tasks/, team/, history/, transcripts/
  │
AppContext.ActivateSession(session)
  → SessionManager.Activate: binds session, persists it as active in the index
  → rebuilds TeammateManager from the session's Bus/TaskMgr/DagSched/Protocols
  → regenerates System (system prompt) for the now-active session
  │
Work (multiple turns)
  → history appended as JSONL (fsync on each write)
  → tasks persisted as JSON files
  │
AppContext.DeactivateActiveSession()
  → ShutdownTeammates: stops all running teammates
  → SessionManager.Deactivate: session_to_memory prompt extracts learnings,
    saves to MemoryStore, updates session index (last-active timestamp)
  │
Archive(session)
  → marks as archived in index
  → data preserved but hidden from /session list
```

### History Persistence

- **Format**: Append-only JSONL (one JSON object per line)
- **Crash safety**: fsync after each write; tail-dangling tool_calls trimmed on restore
- **Compaction**: Checkpoint-based — when autoCompact runs, it writes a checkpoint entry containing the LLM summary, then subsequent LoadRuntime starts from the latest checkpoint
- **Kinds**: system, user, assistant, tool, checkpoint

---

## Multi-Agent Collaboration

### TeammateManager

Each teammate is an autonomous agent with its own:
- Conversation context (separate from lead)
- WORK/IDLE state machine
- Inbox (via MessageBus)
- Access to shared TaskManager and DAGScheduler

### State Machine

```
IDLE ──(message received)──→ WORK
  │                            │
  │ (IdleTimeout=60s)          │ (task complete / no more work)
  ▼                            ▼
SHUTDOWN                     IDLE
```

### Communication Protocol

```
Lead ──send_message──→ Teammate inbox (JSONL)
Lead ──broadcast────→ All teammate inboxes
Teammate ──send_message──→ Lead inbox
Teammate ──plan_approval──→ ProtocolStore (TTL=30min)
Lead ──plan_approval──→ approve/reject (TTL=24h)
```

### Plan Approval Protocol

Teammates must submit execution plans before performing write operations:
1. Teammate creates plan → stored in ProtocolStore with 30-min TTL
2. Lead receives notification in inbox
3. Lead reviews and approves/rejects via `plan_approval` tool
4. Teammate proceeds only after approval

### DAG-Aware Auto-Claim

Idle teammates automatically claim ready tasks (dependencies satisfied) from the shared DAG board, enabling parallel execution without explicit lead coordination.

---

## MCP Integration

### Configuration

Create `.mcp.json` in the workspace root:

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

### How It Works

1. On startup, `MCPManager` reads `.mcp.json` and launches each server as a subprocess
2. Communication via JSON-RPC 2.0 over stdio (stdin/stdout)
3. Server capabilities are discovered via `initialize` + `tools/list` calls
4. Tools are registered as `mcp__<server>__<tool>` in the tool registry
5. They pass through the same security/HITL/timeout pipeline as built-in tools

### Circuit Breaker

Each MCP server has a circuit breaker:
- **CLOSED** (normal): calls pass through
- **OPEN** (after 3 consecutive failures): calls rejected for 30s cooldown
- **HALF-OPEN** (after cooldown): next call is a probe; success → CLOSED, failure → OPEN

### Runtime Management

```
/mcp                        # List all servers + status + tool count
/mcp connect <name> <cmd>   # Connect a new server at runtime
```

---

## Token Management

### microCompact (every round)

Collapses old tool result messages to brief summaries, keeping only the N most recent (default 15) in full. This prevents unbounded context growth during long tool-use sessions.

### autoCompact (threshold-triggered)

When estimated total tokens exceed 300K:
1. Sends the full conversation to LLM with a "summarize" instruction
2. Replaces all messages (except system + recent) with a single checkpoint message
3. Checkpoint is persisted to history JSONL for crash recovery
4. Token count drops dramatically; agent continues with full context awareness

### Token Estimation

Uses a simple heuristic: `len(content) / 4` (approximates GPT tokenization). Re-checked every 3 rounds to avoid O(N) overhead on every iteration.

---

## Usage Tracking

Every LLM call is logged to `memory/usage.jsonl` with:

| Field | Description |
|-------|-------------|
| `trace_id` | Correlates all calls for one user request |
| `session_id` | Which session generated this call |
| `source` | `agent` / `judge` / `subagent` / `team` / `compress` / `memory-save` |
| `provider` | `openai` / `anthropic` |
| `model` | Actual model ID used |
| `input_tokens` | Prompt tokens consumed |
| `output_tokens` | Completion tokens generated |
| `duration_ms` | Wall-clock time for the call |
| `timestamp` | ISO 8601 timestamp |

View aggregated stats with the `/usage` REPL command.

---

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/anthropics/anthropic-sdk-go` | Anthropic API client |
| `github.com/openai/openai-go` | OpenAI API client |
| `github.com/chzyer/readline` | Interactive REPL with history |
| `github.com/tidwall/gjson` | Fast JSON path queries |
| `github.com/tidwall/sjson` | JSON mutation |
| `github.com/invopop/jsonschema` | JSON Schema generation for tool defs |
| `golang.org/x/net` | HTML parsing for `web_fetch`/`web_search` (`golang.org/x/net/html`) |
| `golang.org/x/sync` | Concurrency primitives (errgroup) |

---

## Example Workflow

```
$ JUDGE_ENABLED=1 ./agent --human

> Implement a REST API for user management with CRUD endpoints

[think] Analyzing requirements: REST API, user CRUD, need to plan...
[task_create] "Design API schema" → T1
[task_create] "Implement handlers" → T2  (depends on T1)
[task_create] "Write tests" → T3  (depends on T2)
[task_create] "Add documentation" → T4  (depends on T2)

[working on T1: Design API schema]
  bash: go mod init user-api
  write_file: internal/model/user.go
  write_file: internal/handler/user.go (skeleton)

[T1 complete → T2 ready]
[working on T2: Implement handlers]
  edit_file: internal/handler/user.go (full implementation)
  bash: go build ./...  ✓

[T2 complete → T3, T4 ready]
[working on T3: Write tests]
  write_file: internal/handler/user_test.go
  bash: go test ./... -v  ✓

[all tasks complete]
[judge] Evaluating completion... Score: 9/10 ✓
[auto-lesson] Writing lessons to memory for future reference
[memory_write] "REST API pattern: model → handler → router → test"

Done.
```