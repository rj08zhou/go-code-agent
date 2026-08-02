# go-code-agent

An autonomous coding agent built in Go featuring multi-round planning, self-reflection, multi-agent collaboration, persistent memory, and MCP (Model Context Protocol) integration.

This project is a CLI/REPL application — not an HTTP service and not an IDE plugin.

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
- [Example Workflow](#example-workflow)

---

## Quick Start

### Prerequisites

- Go `1.25.3` (see `go.mod`)
- At least one LLM API key (`OPENAI_API_KEY` or `ANTHROPIC_API_KEY`)
- Git recommended (teammate worktrees and optional snapshots depend on it)

### Installation

```bash
git clone https://github.com/rj08zhou/go-code-agent.git
cd go-code-agent
go mod download
go build -o agent ./cmd/agent
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
export LLM_PROVIDER="anthropic"  # openai | anthropic
./agent

# Enable LLM-as-Judge with default settings
export JUDGE_ENABLED=1
./agent

# Enable Judge with custom model and threshold
export JUDGE_ENABLED=1
export JUDGE_MODEL=claude-haiku-4.5
export JUDGE_MIN_SCORE=8
./agent
```

### Session Management

```bash
./agent --session <id>     # Resume a specific session
./agent --new-session      # Force a fresh session
./agent --workdir /path/to/project
```

Default persistent state root:

```text
~/.config/go-code-agent/<basename(workdir)>/
```

Override the config root with `--data-dir` (resolved as `{data-dir}/go-code-agent/<basename(workdir)>`).

### Optional Features

```bash
# LLM-as-Judge: post-completion verification
export JUDGE_ENABLED=1

# Escalate HITL to interactive (all tools require confirmation)
./agent --human

# Explicit HITL mode
./agent --human-mode=auto-approve

# Git-stash-based snapshot/rollback for risky write tools
export SNAPSHOT_ENABLED=1
./agent
```

Default approval mode: **`safe-auto`** (lower-risk reviews continue automatically; high-risk operations and write diffs are reviewed). Inspect or change it with `/approval`.

---

## Architecture

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

### Component Ownership

`application.Application` is the composition root. `cmd/agent` only parses flags, calls `application.New` / `Build`, and runs the REPL.

| Lifetime | Owner | Examples |
|----------|-------|----------|
| Process | `Application` | Gateway, provider registry, session repository, dataDir |
| Session | `SessionRuntime` + `BuiltRunner` | Runner, catalog, HITL, ApprovalState, MCP, team, history, tasks |
| Per `Run()` | `Runner.turn` (`turnState`) | rounds, failures, readCounts, lesson/judge counters |

`ApprovalState` is **session-scoped** (created in `Build`, held on `BuiltRunner` and wired into `HITLApprovalAdapter`). It is not a process-global singleton.

---

## Execution Flow

### Single User Message Lifecycle

```
user msg
  │
  ├─ inject memory recall (UserMessage, cache-friendly)
  ├─ AgentStarted event
  ▼
Runner.Run(ctx, messages, traceID)
  │
  for each round:
  │  ├─ prepareRound
  │  │    drop consumed nudges → max-rounds / soft-deadline wrap-up
  │  │    autoCompact / microCompact → plan gate → task note → reflection
  │  ├─ callModel
  │  │    repair tool_call pairing → explore prompt budget → Stream
  │  ├─ truncation? synthesize tool errors + continue
  │  ├─ no tools? auto-lesson → judge → TurnComplete / return
  │  └─ tools? executeToolBatch → afterTools (manual compress, lesson budget)
  ▼
REPL appends new messages to HistoryStore
```

### Multi-Round Conversation

```
Session Start (Build)
  → ensure session dir, wire sinks (session.log, usage.jsonl)
  → load history if resuming
  → REPL banner

Each User Turn
  → drain background notifications + inbox
  → Runner.Run
  → persist assistant/tool messages + optional checkpoint

Session Switch / New / Archive
  → close SessionRuntime hooks
  → Build again with new options
```

---

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MODEL_ID` | `claude-opus-4.7` | Model identifier (also helps infer provider) |
| `LLM_PROVIDER` | auto-infer | Force provider: `openai` \| `anthropic` |
| `ANTHROPIC_API_KEY` | — | Required for Anthropic models |
| `ANTHROPIC_BASE_URL` | SDK default | Gateway/proxy override for Anthropic |
| `OPENAI_API_KEY` | — | Required for OpenAI / compatible models |
| `OPENAI_BASE_URL` | SDK default | Proxy/local model endpoint |
| `REASONING_ENABLED` | off | Opt into provider-native reasoning for agent calls; thinking streams to the terminal in dim magenta (`[thinking]`), opaque state is never persisted in history |
| `REASONING_EFFORT` | `medium` | OpenAI-compatible effort hint: `minimal` \| `low` \| `medium` \| `high` |
| `LLM_MAX_QPS` | `4.0` | Process-wide LLM requests/sec |
| `LLM_MAX_BURST` | `8` | Token-bucket burst |
| `LLM_MAX_CONCURRENCY` | `4` | Max in-flight LLM calls |
| `SNAPSHOT_ENABLED` | off | Set `1` to enable git-stash rollback |
| `CONTEXT_WINDOW_TOKENS` | model default | Override context window size |
| `WEB_ALLOW_PRIVATE_IPS` | off | Allow `web_*` to private/internal IPs |
| `WEB_SEARCH_PROVIDER` | auto | `tavily` \| `brave` (needs API key) or auto chain |
| `WEB_SEARCH_API_KEY` | — | Key for forced search provider |
| `SEARXNG_URL` | — | Trusted SearXNG instance |
| `SEARXNG_INSTANCES` | built-in list | Comma-separated SearXNG fallbacks |

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--workdir <path>` | current dir | Project directory the agent edits |
| `--data-dir <path>` | `~/.config` (or `$XDG_CONFIG_HOME`) | Config root; state lives under `{data-dir}/go-code-agent/<project>-<path-hash>` |
| `--session <id>` | — | Resume a specific session |
| `--new-session` | false | Force a new session |
| `--human` | false | Escalate approval mode to `manual` |
| `--human-mode` | (keep default) | `interactive` \| `safe-only` \| `auto-approve` \| `auto-reject` \| `notify-only` |

### LLM-as-Judge Env Vars

| Env Var | Default | Description |
|---------|---------|-------------|
| `JUDGE_ENABLED` | unset | Enable judge (`1` / `true` / `yes` / `on`) |
| `JUDGE_MODEL` | main model | Model id for judge calls |
| `JUDGE_MIN_SCORE` | 7 | Score below this triggers a retry (1–10) |
| `JUDGE_PROVIDER` | inferred | `openai` / `anthropic` |
| `JUDGE_API_KEY` | main key | Judge-only key |
| `JUDGE_BASE_URL` | main url | Judge-only endpoint |

### Tunable Constants (`internal/config/config.go`)

Compile-time constants (not env-configurable). Selected values:

| Category | Constant | Value | Description |
|----------|----------|-------|-------------|
| **Loop** | `MaxRounds` | 100 | Hard safety cap for agent loop |
| | `StuckThreshold` | 12 | Rounds without progress → stuck |
| | `ReflectInterval` | 12 | Periodic reflection every N rounds |
| | `MaxConsecutiveFailures` | 3 | Same tool failing → strategy change |
| | `LessonThreshold` | 3 | Min rounds before auto-lesson |
| | `SubagentMaxRounds` | 16 | Explore soft round ceiling |
| | `TeammateWorkMaxRounds` | 50 | Teammate work phase cap |
| | `DefaultMaxOutputTokens` | 16384 | Default max output tokens |
| | `SubagentPromptTokenBudget` | 80000 | Cumulative explore prompt budget |
| | `ExploreBudgetWarnFrac` | 0.70 | Soft “start synthesizing” nudge |
| **Tokens** | `TokenThreshold` | 300000 | autoCompact upper bound helper |
| | `CompactionThresholdFrac` | 0.75 | Fraction of context window for compact |
| | `MicroCompactThresholdFrac` | 0.5 | Pressure gate for microCompact |
| | `MicroCompactMinClearBytes` | 4096 | Skip microCompact if reclaim too small |
| | `KeepRecent` | 15 | Recent tool msgs kept full after compact |
| | `TokenCheckInterval` | 3 | Re-check tokens every N rounds |
| **Timing** | `PerToolTimeout` | 5 min | Hard ceiling per tool handler |
| | `BashTimeout` | 120s | bash / background_run timeout |
| | `LlmMaxRetries` | 5 | LLM call retry attempts |
| | `LlmCallTimeout` | 5 min | Per Call/Stream attempt |
| | `WebFetchSubagentBudget` | 60s | Lead `web_fetch` subagent wall time |
| | `WebSearchTimeout` | 25s | Overall search budget |
| | `WebSearchPerBackendTimeout` | 7s | Per-backend search cap |
| **Memory** | `MemoryTTLDays` | 90 | Daily files auto-deleted after this |
| | `MaxEvergreenChars` | 8000 | MEMORY.md injection truncation |
| | `MaxMemoryContentLen` | 2000 | Max chars per memory entry |
| **Judge** | `JudgeMaxRetryInjects` | 2 | Max verification-failed retries |
| **Team** | `PollInterval` | 5s | Idle teammate inbox poll |
| | `IdleTimeout` | 60s | Idle teammate auto-shutdown |

---

## File Layout

```
go-code-agent/
├── cmd/
│   ├── agent/
│   │   ├── main.go              # Flags, Application.New, Build loop
│   │   └── repl.go              # Slash commands + conversation loop
│   └── eval/
│       └── main.go              # Eval harness entry
├── internal/
│   ├── application/             # Composition root (Application, SessionRuntime)
│   ├── agent/                   # Runner loop, explore, teammates, judge, compression
│   ├── tool/                    # Catalog, executor, builtin handlers
│   ├── model/                   # Gateway + provider implementations
│   ├── hitlaudit/               # HITL manager + ApprovalAdapter
│   ├── security/                # Path sandbox, bash policy, ApprovalState, SSRF, diff
│   ├── session/                 # Session index + meta.json
│   ├── history/                 # Conversation JSONL + checkpoints
│   ├── memory/                  # Evergreen + daily memory
│   ├── task/                    # Persistent task DAG + TodoManager
│   ├── team/                    # Message bus + protocols
│   ├── mcp/                     # MCP stdio client/manager
│   ├── web/                     # web_fetch / web_search
│   ├── event/                   # Event sinks (console, audit, usage, session.log)
│   ├── config/                  # Env config + compile-time constants
│   ├── prompt/                  # Prompt templates
│   ├── skill/                   # SKILL.md loader
│   ├── worktree/                # Teammate git worktrees
│   ├── background/              # Background job supervisor
│   ├── store/                   # Atomic file writes
│   ├── llm/                     # Shared LLM message types
│   ├── logging/                 # Structured logging
│   ├── utils/                   # Terminal + helpers
│   └── eval/                    # Regression eval helpers
├── skills/                      # Example workspace skills
├── go.mod
└── README.md
```

---

## REPL Commands

| Command | Description |
|---------|-------------|
| `/help` | Show command help |
| `/session` | Session info |
| `/session list` | List sessions |
| `/session switch <id>` | Switch active session (rebuilds runtime) |
| `/session new` | Start a fresh session |
| `/session rename <title>` | Rename active session |
| `/session archive` | Archive active session |
| `/tasks` | Render short-term todos |
| `/dag` | Show persistent task DAG |
| `/task clear\|reset` | Clear completed / wipe all tasks |
| `/memory` | Memory stats |
| `/mcp` | List MCP servers |
| `/mcp pending` / `approve <name>` | Pending MCP approval |
| `/mcp connect` / `disconnect` | Runtime MCP connect |
| `/team` | List / spawn / shutdown / message teammates |
| `/inbox` | Read lead inbox |
| `/judge` | Toggle LLM-as-Judge |
| `/approval [manual\|safe-auto\|all-auto\|reject\|notify-only]` | Show or set the effective approval mode (`all-auto` requires `confirm`) |
| `/approve ...` / `/hitl ...` | Compatibility aliases for legacy commands |
| `/permissions [reload]` | Show / reload permissions.json |
| `/security` | Security status |
| `/security test-bash <cmd>` | Dry-run bash policy |
| `/decisions` | Recent decision log |
| `/usage` | Token usage summary |
| `/compact` | Manual context compaction |
| `/search <query>` | One-shot web search |
| `/exit` `/quit` | Exit |

---

## Built-in Tools

39 built-in tools (stable catalog order for prompt-cache friendliness), plus dynamic `mcp__*` tools.

### Core (File & Shell)

| Tool | Description |
|------|-------------|
| `read_file` / `list_dir` | Read file / list directory |
| `search_file` / `search_content` | Glob / content search |
| `write_file` / `edit_file` / `insert_file` / `delete_file` | Mutations (diff preview when enabled) |
| `bash` | Foreground shell (timeout + policy) |
| `background_run` / `check_background` | Background jobs |

### Reasoning / Meta

| Tool | Description |
|------|-------------|
| `explore` | Isolated read-only investigation subagent |
| `compress` | Request conversation compaction |
| `load_skill` | Load a workspace skill |

### Planning & Tasks

| Tool | Description |
|------|-------------|
| `TodoWrite` | Short-term checklist |
| `task_create` / `task_list` / `task_update` / `task_get` | Persistent tasks |
| `task_add_dep` / `task_remove_dep` / `task_ready` / `task_dag` / `claim_task` | DAG scheduling |

### Multi-Agent Collaboration

| Tool | Description |
|------|-------------|
| `spawn_teammate` / `list_teammates` | Lifecycle |
| `send_message` / `read_inbox` / `broadcast` | Messaging |
| `shutdown_request` / `plan_approval` / `submit_plan` | Protocols |

### Memory

| Tool | Description |
|------|-------------|
| `memory_write` / `memory_search` / `memory_delete` / `memory_stats` | Long-term memory |
| `session_save_memory` | Archive session learnings |

### Web Access

| Tool | Description |
|------|-------------|
| `web_fetch` | Fetch URL via isolated subagent (`web_fetch` + `web_search` only) |
| `web_search` | Multi-backend search with fallbacks |

### MCP (Dynamic)

Tools registered at runtime as `mcp__<server>__<tool>` after MCP servers start / are approved.

---

## LLM Providers

Supported backends:

- Anthropic API (`anthropic-sdk-go`)
- OpenAI API and OpenAI-compatible endpoints (`openai-go`)

### Provider Selection Logic

1. Explicit `LLM_PROVIDER` if set
2. Otherwise infer from `MODEL_ID` prefix / available keys
3. Role-level throttling on the gateway (`lead`, `explore`, `teammate`, `judge`)

### Retry Strategy

Exponential backoff with dedicated handling for rate limits (`LlmMaxRetries`, `LlmBaseDelay`, `LlmRateLimitDelay`, `LlmMaxDelay`), plus per-call and HTTP timeouts.

---

## Security Model

Defense in depth around tool execution.

### 1. Bash Policy (Command Allowlist)

Whitelist of common safe base commands plus deny/confirm regexps for destructive patterns (`rm -rf /`, privilege escalation, force-push style git, etc.).

### 2. Path Sandbox

`security.SecurePath` confines file operations to the workdir / allowed roots. Absolute paths under the workspace are preserved correctly (Go 1.25 `filepath.Join` absolute-element behavior is handled).

### 3. Approval Levels & HITL

Two cooperating pieces:

| Piece | Role |
|-------|------|
| `security.ApprovalState` | Internal auto-approval flags and diff-preview posture |
| `hitlaudit.HITLManager` | Implements the effective `manual`, `safe-auto`, `all-auto`, `reject`, and `notify-only` modes |

`HITLApprovalAdapter` adapts both into `tool.ApprovalChecker` for the executor (including chunked diff confirmation).

### 4. Secrets Sanitizer

Tool outputs are sanitized before returning to the model / logs.

### Additional Security Features

- Diff preview for mutating file tools (skipped under confirmed `all-auto`)
- Optional git snapshot + rollback on failure (`SNAPSHOT_ENABLED=1`)
- Decision audit log (`decisions.jsonl`)
- Session event log (`session.log`)
- Permissions rules file (`permissions.json` under dataDir)

---

## Web Access & SSRF Protection

### Default-deny private networks

`web_fetch` / `web_search` block private/link-local/metadata addresses unless `WEB_ALLOW_PRIVATE_IPS` is enabled.

### web_search downgrade chain

Auto chain across configured backends (API providers and SearXNG instances) with an overall budget and per-backend timeout so one hung backend cannot starve fallbacks.

### Untrusted content handling

Fetched pages are framed as untrusted content, size-capped, and scrubbed of obvious secrets before injection into the model context.

---

## Agentic Features

### Think → Plan → Act → Reflect Cycle

- Plan gate nudges early rounds toward planning tools when the task is non-trivial
- Reflection injects stuck / failure / todo-nag prompts
- Repeated identical tool calls and explore re-reads are rate-limited

### DAG Scheduler

Persistent tasks with dependency edges (`task_*` tools). Ready tasks can be claimed; `/dag` shows topology and progress.

### Auto-Lesson

After enough rounds, the lead agent may be nudged to write lessons via `memory_write` (memory-capable profiles only).

### LLM-as-Judge

Optional post-completion verifier. Low scores inject retry feedback up to `JudgeMaxRetryInjects`.

---

## Memory System

### Two-Layer Architecture

| Layer | Storage | Purpose |
|-------|---------|---------|
| Evergreen | `MEMORY.md` | Stable preferences / facts |
| Daily | `daily/YYYY-MM-DD.jsonl` | Ephemeral notes with TTL cleanup |

### Search & Dedup

Keyword / similarity style search with Jaccard dedup on write. Daily files older than `MemoryTTLDays` are cleaned on store init.

### Auto-Recall Flow

On each user turn, relevant memories are injected once as a `UserMessage` (“Relevant memory: …”) so the system prompt block stays stable for prompt caching.

---

## Session Management

### Lifecycle

1. `Application.New` creates process services and dataDir
2. `Build` resolves/creates session, ensures session directory, wires `BuiltRunner`
3. REPL runs until exit or session switch
4. Session switch / new / archive closes hooks and rebuilds

### History Persistence

- Append-only `history/history.jsonl`
- Compression writes transcripts under `transcripts/` and checkpoint entries for resume
- `session.log` (JSONL events), `usage.jsonl`, `decisions.jsonl` live under the session dir
- Index: `{dataDir}/sessions.json`; meta: `{dataDir}/sessions/<id>/meta.json`

---

## Multi-Agent Collaboration

### TeammateManager

Spawns teammates with optional isolated git worktrees, shared message bus, and the same HITL/approval adapter as lead.

### Communication

JSONL inboxes via `team.MessageBus`. Protocol helpers cover shutdown requests and plan approval.

### Explore Subagent

Read-oriented isolation with prompt/token budgets, soft deadline wrap-up, and restricted tools (no arbitrary write/memory).

---

## MCP Integration

### Configuration

Workspace `.mcp.json` and/or `MCP_SERVERS` style configuration (see manager for current schema). Untrusted servers may require `/mcp approve`.

### How It Works

1. Manager starts stdio MCP servers
2. Tools merge into the session `ToolCatalog` **after** builtins (additive register; order kept stable)
3. Runtime connect/disconnect via REPL

### Circuit Breaker / Safety

Failed servers are surfaced in `/mcp`; tool names are prefixed `mcp__` for clear attribution. Treat third-party MCP tools as an expanded capability surface under the same HITL/path/network gates where applicable.

---

## Token Management

### microCompact (pressure-gated)

Clears old tool results only when estimated usage crosses `MicroCompactThresholdFrac` of the context window **and** at least `MicroCompactMinClearBytes` can be reclaimed — avoids busting the prompt cache on short sessions.

### autoCompact (threshold-triggered)

When compaction is needed (window fraction / explore thresholds):

1. Save full transcript under `transcripts/`
2. Summarize older turns via LLM
3. Keep recent turns; invalidate cached token estimate
4. Persist a history checkpoint for resume

### Explore budgets

Explore subagents track cumulative prompt tokens, emit a budget-warn nudge near 70%, and finalize with a no-tools summary when the hard budget or soft deadline is hit.

### Token Estimation

Heuristic estimate of request size (`EstimateRequestTokens`), refreshed on an interval rather than every round.

---

## Usage Tracking

LLM calls append to `{sessionDir}/usage.jsonl` and also emit usage events to sinks.

Typical fields include role/source, model, prompt/completion/cache tokens, duration, and trace id.

View aggregates with `/usage`.

---

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/anthropics/anthropic-sdk-go` | Anthropic API client |
| `github.com/openai/openai-go` | OpenAI / compatible API client |
| `github.com/chzyer/readline` | Interactive REPL with history |
| `golang.org/x/net` | HTML parsing for web fetch/search |

---

## Example Workflow

```
$ JUDGE_ENABLED=1 ./agent

> Analyze this repo's tool pipeline, then propose one focused refactor. Do not edit yet.

[explore] read-only investigation…
[memory_search] prior notes about executor pipeline
[TodoWrite] outline findings

> OK, extract HITLApprovalAdapter into its own file.

[hitl] reviewing write_file [safe]
[write_file] internal/hitlaudit/approval_adapter.go
[edit_file] internal/hitlaudit/human_approval.go
[bash] go test ./internal/hitlaudit/

[judge] score=9 approved
Done.
```
