package main

import (
	"encoding/json"
	"go-code-agent/internal/background"
	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/mcp"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/session"
	"go-code-agent/internal/skill"
	"go-code-agent/internal/task"
	"go-code-agent/internal/team"
)

// AppContext is the agent's root object.
//
// Fields are grouped into:
//   - Process-wide config: Model, Workdir, System
//   - Workdir-global subsystems (shared across sessions): Skills, MemStore, MCPMgr
//   - Per-session subsystems (rebound on session switch): accessed via Active()
//   - Cmd-layer subsystems: TeamMgr
//
// The active LLM provider is reached via the package-level activeProvider
// (see provider.go). Tool handlers and REPL commands reach subsystems
// through the package-level `app` variable.

type AppContext struct {
	// Process-wide config
	Model   string
	Workdir string
	System  string

	// Workdir-global subsystems
	Skills       *skill.SkillLoader
	MemStore     *memory.MemoryStore
	MCPMgr       *mcp.MCPManager
	PromptLoader *prompt.Loader

	// Session management
	SessionManager *session.SessionManager

	// Per-session team manager (cmd layer)
	teamMgr *TeammateManager

	// Tool registry
	ToolDefs     []llm.ToolDef
	ToolHandlers map[string]func(rawJSON json.RawMessage) string
}

// app is the singleton AppContext for this process.
var app *AppContext

// Convenience accessors delegating to the active session.

func (a *AppContext) DagSched() *task.DAGScheduler {
	s := a.SessionManager.Active()
	if s == nil {
		return nil
	}
	return s.DagSched
}

func (a *AppContext) TaskMgr() *task.TaskManager {
	s := a.SessionManager.Active()
	if s == nil {
		return nil
	}
	return s.TaskMgr
}

func (a *AppContext) Todo() *task.TodoManager {
	s := a.SessionManager.Active()
	if s == nil {
		return nil
	}
	return s.Todo
}

func (a *AppContext) BgMgr() *background.BackgroundManager {
	s := a.SessionManager.Active()
	if s == nil {
		return nil
	}
	return s.BgMgr
}

func (a *AppContext) Bus() *team.MessageBus {
	s := a.SessionManager.Active()
	if s == nil {
		return nil
	}
	return s.Bus
}

func (a *AppContext) TeamMgr() *TeammateManager {
	return a.teamMgr
}

// SetTeamMgr sets the active TeammateManager (called on session switch).
func (a *AppContext) SetTeamMgr(tm *TeammateManager) {
	a.teamMgr = tm
}

func (a *AppContext) Protocols() *team.ProtocolStore {
	s := a.SessionManager.Active()
	if s == nil {
		return nil
	}
	return s.Protocols
}

func (a *AppContext) History() *history.HistoryStore {
	s := a.SessionManager.Active()
	if s == nil {
		return nil
	}
	return s.History
}
