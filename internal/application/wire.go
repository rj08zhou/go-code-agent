package application

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go-code-agent/internal/agent"
	"go-code-agent/internal/background"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/history"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/mcp"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/security"
	"go-code-agent/internal/skill"
	"go-code-agent/internal/task"
	"go-code-agent/internal/team"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/web"
	"go-code-agent/internal/worktree"
)

// RunnerParams bundles session-scoped services needed by BuildRunner.
type RunnerParams struct {
	TaskSvc      *task.Service
	TodoSvc      *task.TodoManager
	DiffPreview  tool.DiffPreview
	DecisionLog  *agent.DecisionLog
	MemoryStore  *memory.Store
	SkillLoader  *skill.Loader
	BGSvc        *background.Supervisor
	Bus          *team.MessageBus
	WebService   tool.WebService
	HITLMgr      *hitlaudit.HITLManager
	Approval     *security.ApprovalState
	MCPMgr       *mcp.Manager
	WorktreeSvc  *worktree.Service
	Protocols    *team.ProtocolStore
	PromptLoader *prompt.Loader
	Permissions  *security.Permissions
	Config       *config.Config
}

// wireBundle is the intermediate product of staged wiring.
type wireBundle struct {
	catalog   *tool.ToolCatalog
	executor  *tool.Executor
	subagent  *agent.SubagentRunner
	teamMgr   *agent.TeammateManager
	sysPrompt string
	runner    *agent.Runner
	histStore *history.Store
	judge     *agent.Judge
	hitlAdpt  *hitlaudit.HITLApprovalAdapter
}

// BuildRunner wires a single session run via staged helpers.
func (rt *SessionRuntime) BuildRunner(params RunnerParams) *BuiltRunner {
	st := rt.SessionState
	cfg := params.Config
	if cfg == nil {
		cfg = &config.Config{ModelID: "default"}
	}
	sessionDir := rt.sessionRepo.SessionDir(st.ID)

	wb := &wireBundle{catalog: rt.catalog}
	wb.executor, wb.hitlAdpt = wireSecurity(rt, params)
	wb.subagent, wb.teamMgr = wireTeam(rt, params, sessionDir, wb.hitlAdpt)
	wireTools(rt, params, wb)
	wb.sysPrompt = wireSystemPrompt(rt, params)
	wb.histStore = wireHistory(rt, sessionDir)
	wb.runner, wb.judge = wireAgent(rt, params, wb, st.ID)
	wireObservability(rt, params, wb, sessionDir)

	return &BuiltRunner{
		Session: SessionFacade{
			ID:        st.ID,
			Title:     st.Title,
			AgentID:   "lead",
			Workdir:   rt.workdir,
			ModelID:   cfg.ModelID,
			SysPrompt: wb.sysPrompt,
			HistStore: wb.histStore,
			Repo:      rt.sessionRepo,
			Catalog:   wb.catalog,
			Compact:   wb.runner.ManualCompact,
		},
		Security: SecurityFacade{
			HITL:        params.HITLMgr,
			Approval:    params.Approval,
			Permissions: params.Permissions,
			DecisionLog: params.DecisionLog,
			DiffPreview: diffPreviewConcrete(params.DiffPreview),
		},
		Team: TeamFacade{
			Mgr:      wb.teamMgr,
			Bus:      params.Bus,
			BG:       params.BGSvc,
			MCP:      params.MCPMgr,
			Worktree: params.WorktreeSvc,
		},
		Tasks: TaskFacade{
			Service: params.TaskSvc,
			Todos:   params.TodoSvc,
			Memory:  params.MemoryStore,
		},
		Runtime: RuntimeFacade{
			Runner:       wb.runner,
			Subagent:     wb.subagent,
			Judge:        wb.judge,
			JudgeEnabled: cfg.JudgeEnabled,
			Web:          params.WebService,
		},
	}
}

func wireSecurity(rt *SessionRuntime, params RunnerParams) (*tool.Executor, *hitlaudit.HITLApprovalAdapter) {
	hitlApproval := hitlaudit.NewHITLApprovalAdapter(params.HITLMgr)
	hitlApproval.SetWorkdir(rt.workdir)
	hitlApproval.SetApproval(params.Approval)
	exec := tool.NewExecutor(rt.catalog, hitlApproval, nil).
		WithSanitizer(security.NewSecretsSanitizer()).
		WithDecisionLogger(params.DecisionLog)
	return exec, hitlApproval
}

func wireTeam(
	rt *SessionRuntime,
	params RunnerParams,
	sessionDir string,
	hitlApproval *hitlaudit.HITLApprovalAdapter,
) (*agent.SubagentRunner, *agent.TeammateManager) {
	cfg := params.Config
	subagentRunner := agent.NewSubagentRunner(rt.gateway, rt.catalog, cfg, params.PromptLoader)
	subagentRunner.SetCompression(agent.NewCompression(rt.gateway, nil, sessionDir, cfg.ModelID, params.PromptLoader))
	subagentRunner.SetApproval(hitlApproval)

	teamMgr := agent.NewTeammateManager(
		filepath.Join(sessionDir, "team"), rt.gateway,
		params.Bus, params.TaskSvc, params.Protocols, params.WorktreeSvc,
		rt.catalog, cfg.ModelID, params.PromptLoader,
	)
	teamMgr.SetSessionCtx(rt.Ctx)
	teamMgr.SetDiffPreview(params.DiffPreview)
	teamMgr.SetApproval(hitlApproval)
	teamMgr.SetReasoningConfig(cfg)
	return subagentRunner, teamMgr
}

func wireTools(rt *SessionRuntime, params RunnerParams, wb *wireBundle) {
	builtinDefs := tool.BuiltinTools(
		params.TaskSvc, params.TodoSvc, params.MemoryStore, params.SkillLoader, params.BGSvc, params.Bus,
		wb.subagent,
		wb.teamMgr,
		params.Protocols,
		params.WebService,
		params.Permissions,
	)
	wb.catalog.RegisterAll(builtinDefs)

	// MCP tools merge AFTER builtins so RegisterAll does not wipe them.
	// Order stays stable for prompt-prefix caching.
	mcpAdapter := mcp.NewToolCatalogAdapter(wb.catalog, params.MCPMgr)
	params.MCPMgr.SetRegistry(mcpAdapter)
	params.MCPMgr.LoadAndStart(rt.Ctx)
}

func wireSystemPrompt(rt *SessionRuntime, params RunnerParams) string {
	return agent.NewSystemPromptBuilder(
		params.PromptLoader, params.SkillLoader, nil,
	).Build(rt.workdir)
}

func wireHistory(rt *SessionRuntime, sessionDir string) *history.Store {
	histStore, histErr := history.New(filepath.Join(sessionDir, "history", history.FileName))
	if histErr != nil {
		fmt.Fprintf(os.Stderr, "[warn] history store: %v\n", histErr)
	}
	return histStore
}

func wireAgent(rt *SessionRuntime, params RunnerParams, wb *wireBundle, sessionID string) (*agent.Runner, *agent.Judge) {
	cfg := params.Config
	profile := agent.NewLeadProfile(wb.sysPrompt)
	scope := &tool.ToolScope{
		Role:        "lead",
		Workdir:     rt.workdir,
		SessionID:   sessionID,
		AgentID:     "lead",
		ProjectID:   rt.workdir,
		DiffPreview: params.DiffPreview,
	}
	runner := agent.NewRunner(profile, rt.gateway, wb.executor, scope, cfg)
	if params.MemoryStore != nil {
		runner.SetLessonWriter(agent.NewLLMLessonWriter(rt.gateway, params.MemoryStore, params.PromptLoader, cfg.ModelID))
		runner.SetMemoryRecall(func(query string) string { return params.MemoryStore.Search(query, 3, 90, "") })
	}
	if params.TodoSvc != nil {
		runner.SetTodoState(func() (bool, string) {
			return params.TodoSvc.HasOpenItems(), params.TodoSvc.Render()
		})
	}
	if params.TaskSvc != nil || params.TodoSvc != nil {
		runner.SetTaskProgress(func() string {
			var parts []string
			if params.TodoSvc != nil {
				parts = append(parts, params.TodoSvc.Render())
			}
			if params.TaskSvc != nil {
				if progress := params.TaskSvc.ProgressSummary(); progress != "" {
					parts = append(parts, progress)
				}
			}
			return strings.Join(parts, "\n")
		})
	}
	runner.SetDynamicContext(func() string {
		evergreen := ""
		if params.MemoryStore != nil {
			evergreen = params.MemoryStore.GetEvergreen()
		}
		var taskParts []string
		if params.TodoSvc != nil {
			taskParts = append(taskParts, params.TodoSvc.Render())
		}
		if params.TaskSvc != nil {
			if progress := params.TaskSvc.ProgressSummary(); progress != "" {
				taskParts = append(taskParts, progress)
			}
		}
		mcp := ""
		if params.MCPMgr != nil {
			mcp = strings.TrimSpace(params.MCPMgr.List() + "\n" + params.MCPMgr.ServerInstructions())
		}
		return agent.BuildSessionContext(evergreen, strings.Join(taskParts, "\n"), mcp)
	})

	runner.SetCompression(agent.NewCompression(rt.gateway, wb.histStore, rt.sessionRepo.SessionDir(sessionID), cfg.ModelID, params.PromptLoader))
	runner.SetPromptLoader(params.PromptLoader)
	runner.SetReflection(agent.NewReflection(params.PromptLoader))
	runner.SetSnapshot(agent.NewSnapshotManager(cfg.SnapshotEnabled, rt.workdir))
	runner.SetSubagentRunner(wb.subagent)
	runner.SetPlanGate(agent.NewPlanGate(params.PromptLoader, params.TaskSvc))

	judgeInst := agent.NewJudge(cfg.JudgeEnabled, cfg.JudgeModel,
		cfg.JudgeMinScore, params.PromptLoader, rt.gateway)
	runner.SetJudge(judgeInst)
	return runner, judgeInst
}

func wireObservability(rt *SessionRuntime, params RunnerParams, wb *wireBundle, sessionDir string) {
	sinks := []event.Sink{event.NewConsoleSink(), event.NewAuditSink(), event.NewUsageSink()}
	if sessionLog, logErr := event.NewSessionLogSink(filepath.Join(sessionDir, "session.log")); logErr != nil {
		fmt.Fprintf(os.Stderr, "[warn] session.log: %v\n", logErr)
	} else {
		sinks = append(sinks, sessionLog)
		rt.AddHook("session-log", sessionLog.Close)
	}
	allEvents := event.NewMultiSink(sinks...)
	wb.runner.SetEventSink(allEvents)
	wb.subagent.SetEventSink(allEvents)
	wb.teamMgr.SetEventSink(allEvents)

	rt.AddHook("team", func() error { wb.teamMgr.ShutdownAll(); wb.teamMgr.Wait(); return nil })
	rt.AddHook("mcp", func() error { params.MCPMgr.Shutdown(); return nil })
	rt.AddHook("background", func() error { params.BGSvc.StopAll(); return nil })
	rt.AddHook("worktree", func() error { params.WorktreeSvc.RemoveAll(); return nil })
	if wb.histStore != nil {
		rt.AddHook("history", func() error { return wb.histStore.Close() })
	}
}

func diffPreviewConcrete(dp tool.DiffPreview) *security.DiffPreview {
	if v, ok := dp.(*security.DiffPreview); ok {
		return v
	}
	return nil
}

// newSessionParams constructs RunnerParams for Application.Build.
func newSessionParams(
	app *Application,
	sessionDir, workdir string,
	hitlMgr *hitlaudit.HITLManager,
	approval *security.ApprovalState,
	perms *security.Permissions,
	diffPreview *security.DiffPreview,
	decisionLog *agent.DecisionLog,
	msgBus *team.MessageBus,
	promptLoader *prompt.Loader,
	cfg *config.Config,
) RunnerParams {
	return RunnerParams{
		TaskSvc:     task.NewService(filepath.Join(sessionDir, "tasks")),
		TodoSvc:     &task.TodoManager{},
		DiffPreview: diffPreview,
		DecisionLog: decisionLog,
		MemoryStore: memory.NewStore(app.dataDir),
		SkillLoader: skill.NewLoader(filepath.Join(workdir, "skills")),
		BGSvc:       background.New(workdir),
		Bus:         msgBus,
		WebService: web.NewService(web.NewSearchProvider(web.SearchConfig{
			Provider:         cfg.WebSearchProvider,
			APIKey:           cfg.WebSearchAPIKey,
			SearxngURL:       cfg.SearxngURL,
			SearxngInstances: cfg.SearxngInstances,
		})),
		HITLMgr:      hitlMgr,
		Approval:     approval,
		MCPMgr:       mcp.NewManager(workdir),
		WorktreeSvc:  worktree.New(workdir, sessionDir),
		Protocols:    team.NewProtocolStore(msgBus),
		PromptLoader: promptLoader,
		Permissions:  perms,
		Config:       cfg,
	}
}
