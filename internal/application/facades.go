package application

import (
	"context"

	"go-code-agent/internal/agent"
	"go-code-agent/internal/background"
	"go-code-agent/internal/history"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/mcp"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/security"
	"go-code-agent/internal/session"
	"go-code-agent/internal/task"
	"go-code-agent/internal/team"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/worktree"
)

// BuiltRunner is the REPL-facing session handle. Fields are grouped by
// capability so callers depend on 5 facades instead of 30 flat fields.
type BuiltRunner struct {
	Session  SessionFacade
	Security SecurityFacade
	Team     TeamFacade
	Tasks    TaskFacade
	Runtime  RuntimeFacade
}

// SessionFacade covers identity, history, catalog, and compaction.
type SessionFacade struct {
	Context   context.Context
	ID        string
	Title     string
	AgentID   string
	Workdir   string
	ModelID   string
	SysPrompt string
	HistStore *history.Store
	Repo      *session.Repository
	Catalog   *tool.ToolCatalog
	Compact   func(context.Context, []llm.Message) []llm.Message
	Usage     *agent.UsageTracker
}

// SecurityFacade covers HITL, approvals, permissions, and audit.
type SecurityFacade struct {
	HITL              *hitlaudit.HITLManager
	Approval          *security.ApprovalState
	Permissions       *security.Permissions
	DiffPreview       *security.DiffPreview
	DecisionLog       *agent.DecisionLog
	ReloadPermissions func() error
}

// TeamFacade covers multi-agent, MCP, background jobs, and worktrees.
type TeamFacade struct {
	Mgr      *agent.TeammateManager
	Bus      *team.MessageBus
	BG       *background.Supervisor
	MCP      *mcp.Manager
	Worktree *worktree.Service
}

// TaskFacade covers todos, persistent tasks, and long-term memory.
type TaskFacade struct {
	Service *task.Service
	Todos   *task.TodoManager
	Memory  *memory.Store
}

// RuntimeFacade covers the lead runner and related execution helpers.
type RuntimeFacade struct {
	Runner             *agent.Runner
	Subagent           *agent.SubagentRunner
	Judge              *agent.Judge
	JudgeEnabled       bool
	Web                tool.WebService
	ProviderName       string
	EndpointHost       string
	ReasoningRequested bool
	ReasoningAvailable bool
	ReasoningEffort    string
}
