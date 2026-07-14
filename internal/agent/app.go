package agent

import (
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/background"
	"go-code-agent/internal/history"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/mcp"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/security"
	"go-code-agent/internal/session"
	"go-code-agent/internal/skill"
	"go-code-agent/internal/task"
	"go-code-agent/internal/team"
	"go-code-agent/utils"
)

// AppContext is the agent's root object: process-wide config, workdir-global
// subsystems, and session management. Reached via the package-level `App`.
type AppContext struct {
	// Process-wide config
	Model   string
	Workdir string // project root the agent edits / runs commands in
	DataDir string // per-project state root (sessions, memory, etc.); normally under the user-level config dir
	System  string // assembled system prompt, rebuilt per-session

	// Embedded holds documentation baked into the binary (e.g. the
	// README) so commands like /readme can serve it without needing the
	// source file on disk. Set by main() at startup if available.
	Embedded []byte

	// Workdir-global subsystems (shared across sessions)
	Skills       *skill.SkillLoader
	MemStore     *memory.MemoryStore
	MCPMgr       *mcp.MCPManager
	PromptLoader *prompt.Loader

	// Session management
	SessionManager *session.SessionManager

	// Saga snapshot/rollback toggle for risky write tools (opt-in via
	// SNAPSHOT_ENABLED, see main.go / snapshot.go).
	Snapshot *snapshotState

	// LLM-as-Judge post-completion verifier. Starts disabled (see
	// NewApp); main() reassigns it when JUDGE_ENABLED is set.
	Judge *Judge

	// Per-session team manager (cmd layer)
	teamMgr *TeammateManager
}

// App is the singleton AppContext for this process. Set once by
// main() via NewApp after CLI/env config is parsed.
var App *AppContext

// NewApp constructs the AppContext and every workdir-global subsystem
// it owns: Skills, MemStore, MCPMgr (with its on-disk config loaded),
// PromptLoader and SessionManager. main supplies the model id, the
// project workdir, the resolved dataDir (per-project state directory)
// and the bash command validator (security policy is a cmd-layer
// choice); it gets back a fully wired root object. main never needs to
// know any subsystem's constructor signature.
//
// Note the split: workdir is the project the agent edits and runs
// commands in (skills, MCP config, file tools all live there), while
// dataDir is where persistent state (memory, sessions, permissions,
// usage, HITL audit) is stored - normally under the user-level config
// dir rather than inside the project. This keeps the code directory
// clean and lets the same project map to the same state across launches.
func NewApp(model, workdir, dataDir string, bashValidate session.BashValidator) *AppContext {
	skills := skill.NewSkillLoader(utils.JoinWorkdir(workdir, "skills"))
	if skills.Len() == 0 {
		logging.PrintSystem("Warning: no skills loaded from " + utils.JoinWorkdir(workdir, "skills"))
	}
	memStore := memory.NewMemoryStore(dataDir)
	mcpMgr := mcp.NewMCPManager(workdir)
	mcpMgr.LoadConfig(workdir)

	// Load user-editable permission rules from
	// {dataDir}/permissions.json (see security.PermissionRules). Absent
	// file -> no rules -> behavior unchanged. A malformed file is
	// reported but non-fatal so a typo never bricks startup.
	permPath := utils.JoinWorkdir(dataDir, "permissions.json")
	if warn, err := security.GlobalPermissions.Load(permPath); err != nil {
		logging.PrintSystem(fmt.Sprintf("[permissions] %v - ignoring rules", err))
	} else {
		if warn != "" {
			logging.PrintSystem("[permissions] " + warn)
		}
		if n := security.GlobalPermissions.Count(); n > 0 {
			logging.PrintSystem(fmt.Sprintf("[permissions] loaded %d rule(s) from %s", n, permPath))
		}
	}

	pl := prompt.NewLoader(utils.JoinWorkdir(workdir, "prompts"))
	return &AppContext{
		Model:          model,
		Workdir:        workdir,
		DataDir:        dataDir,
		Skills:         skills,
		MemStore:       memStore,
		MCPMgr:         mcpMgr,
		PromptLoader:   pl,
		SessionManager: session.NewSessionManager(workdir, dataDir, model, pl, memStore, bashValidate),
		Snapshot:       &snapshotState{},
		// Judge starts disabled; main() replaces it when JUDGE_ENABLED
		// is set. Disabled Verify never touches the prompt loader, but
		// we still pass it so a runtime SetEnabled(true) works.
		Judge: NewJudge(false, "", infra.JudgeMinScore, pl),
	}
}

// ActivateSession makes sess the active session: binds it into
// SessionManager, rebuilds the per-session TeammateManager, and
// regenerates the system prompt from it. These three steps must
// always happen together (the system prompt and TeamMgr are only
// ever valid for whichever session is currently active) - this
// method is the single place that performs them, called both from
// main()'s initial bootstrap and from repl_commands.go's
// sessionSwitchTo (previously duplicated verbatim in both places).
func (a *AppContext) ActivateSession(sess *session.Session) {
	a.SessionManager.Activate(sess)
	wm := NewWorktreeManager(sess.Dir(), a.Workdir)
	tm := NewTeamMgr(
		sess.TeamDir(), sess.Bus, sess.TaskMgr, sess.DagSched,
		sess.TasksDir(), sess.Protocols, wm,
	)
	wm.CleanupOrphans(tm.MemberNames())
	a.SetTeamMgr(tm)
	a.System = BuildSystemPrompt()
}

// DeactivateActiveSession is ActivateSession's mirror: it shuts down
// any running teammates and removes worktrees. It does NOT call
// SaveToMemory — that was a synchronous LLM call that blocked the exit
// path. Unsaved sessions are picked up by BackfillMemory on next
// startup. No-op if no session is active.
func (a *AppContext) DeactivateActiveSession() {
	ShutdownTeammates()
	if tm := a.TeamMgr(); tm != nil {
		if wm := tm.worktrees; wm != nil {
			wm.RemoveAll()
		}
	}
	if s := a.SessionManager.Active(); s != nil {
		a.SessionManager.Deactivate(s)
	}
}

// ActiveSessionID returns the ID of the currently active session, or
// "" if none is active - safe to call even before App/SessionManager
// exist (nil receiver, nil SessionManager). Centralizes a nil-chain
// (App != nil && SessionManager != nil && SessionManager.Active() !=
// nil, then .Active().ID()) that was previously duplicated at every
// call site needing the active session's ID without caring whether
// one exists yet (main.go's usage session-id callback and post-turn
// Touch, repl_commands.go's /session rename|archive, security.go's
// HITL gate).
func (a *AppContext) ActiveSessionID() string {
	if a == nil || a.SessionManager == nil {
		return ""
	}
	s := a.SessionManager.Active()
	if s == nil {
		return ""
	}
	return s.ID()
}

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
