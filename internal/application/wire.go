package application

import (
	"context"
	"fmt"
	"net"
	"net/url"
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
	Console      *event.ConsoleSink
	Interactive  security.InteractiveIO
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
func (rt *SessionRuntime) BuildRunner(params RunnerParams, sessionDir string) (*BuiltRunner, error) {
	st := rt.SessionState
	cfg := params.Config
	if cfg == nil {
		cfg = &config.Config{ModelID: "default"}
	}

	histStore, err := wireHistory(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("initialize history: %w", err)
	}
	wb := &wireBundle{catalog: rt.catalog, histStore: histStore}
	wb.executor, wb.hitlAdpt = wireSecurity(rt, params)
	wb.subagent, wb.teamMgr = wireTeam(rt, params, sessionDir, wb.hitlAdpt)
	wireTools(rt, params, wb)
	wb.sysPrompt = wireSystemPrompt(rt, params)
	wb.runner, wb.judge = wireAgent(rt, params, wb, st.ID, sessionDir)
	sessionLog := wireObservability(rt, params, wb, sessionDir)
	registerSessionShutdownHooks(rt, params, wb, sessionLog)

	providerName := rt.gateway.ProviderName("lead")
	return &BuiltRunner{
		Session: SessionFacade{
			Context:   rt.Ctx,
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
			Runner:             wb.runner,
			Subagent:           wb.subagent,
			Judge:              wb.judge,
			JudgeEnabled:       cfg.JudgeEnabled,
			Web:                params.WebService,
			ProviderName:       providerName,
			EndpointHost:       providerEndpointHost(cfg, providerName),
			ReasoningRequested: cfg.ReasoningEnabled,
			ReasoningAvailable: rt.gateway.ReasoningAvailable("lead"),
			ReasoningEffort:    strings.ToLower(strings.TrimSpace(cfg.ReasoningEffort)),
		},
	}, nil
}

func providerEndpointHost(cfg *config.Config, providerName string) string {
	if cfg == nil {
		return "unknown"
	}
	switch strings.ToLower(strings.TrimSpace(providerName)) {
	case "openai":
		return safeEndpointHost(cfg.OpenAIBaseURL, "api.openai.com")
	case "anthropic":
		return safeEndpointHost(cfg.AnthropicBaseURL, "api.anthropic.com")
	case "gemini":
		return "generativelanguage.googleapis.com"
	default:
		return "unknown"
	}
}

func safeEndpointHost(rawURL, defaultHost string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return defaultHost
	}
	candidate := rawURL
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Hostname() == "" {
		return "invalid"
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "invalid"
	}
	host := strings.ToLower(parsed.Hostname())
	if port := parsed.Port(); port != "" {
		return net.JoinHostPort(host, port)
	}
	return host
}

func wireSecurity(rt *SessionRuntime, params RunnerParams) (*tool.Executor, *hitlaudit.HITLApprovalAdapter) {
	hitlApproval := hitlaudit.NewHITLApprovalAdapter(params.HITLMgr, params.Interactive)
	hitlApproval.SetApproval(params.Approval)
	hitlApproval.SetCatalog(rt.catalog)
	hitlApproval.SetPermissions(params.Permissions)
	exec := tool.NewExecutor(rt.catalog, hitlApproval, security.NewSSRFNetworkChecker()).
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
	if err := params.MCPMgr.LoadAndStart(rt.Ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] MCP startup: %v\n", err)
	}
}

func wireSystemPrompt(rt *SessionRuntime, params RunnerParams) string {
	return agent.NewSystemPromptBuilder(
		params.PromptLoader, params.SkillLoader, nil,
	).Build(rt.workdir)
}

func wireHistory(sessionDir string) (*history.Store, error) {
	return history.New(filepath.Join(sessionDir, "history", history.FileName))
}

func wireAgent(rt *SessionRuntime, params RunnerParams, wb *wireBundle, sessionID, sessionDir string) (*agent.Runner, *agent.Judge) {
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

	runner.SetCompression(agent.NewCompression(rt.gateway, wb.histStore, sessionDir, cfg.ModelID, params.PromptLoader))
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

func wireObservability(rt *SessionRuntime, params RunnerParams, wb *wireBundle, sessionDir string) *event.SessionLogSink {
	// session.log is the authoritative structured event record. Keep the
	// terminal sink for user-facing summaries; audit and usage events are
	// already captured by SessionLogSink and should not be duplicated in
	// agent.log.
	consoleSink := params.Console
	if consoleSink == nil {
		consoleSink = event.NewConsoleSink()
	}
	sinks := []event.Sink{consoleSink}
	var sessionLog *event.SessionLogSink
	if logSink, logErr := event.NewSessionLogSink(filepath.Join(sessionDir, "session.log")); logErr != nil {
		fmt.Fprintf(os.Stderr, "[warn] session.log: %v\n", logErr)
	} else {
		sessionLog = logSink
		sinks = append(sinks, sessionLog)
	}
	allEvents := event.NewMultiSink(sinks...)
	wb.runner.SetEventSink(allEvents)
	wb.subagent.SetEventSink(allEvents)
	wb.teamMgr.SetEventSink(allEvents)
	rt.gateway.SetEventSink(allEvents)
	return sessionLog
}

// registerSessionShutdownHooks records all runner-owned resources in creation
// order. SessionRuntime closes hooks in reverse, so teammates and background
// work stop before MCP, worktrees, history, and the event log are released.
func registerSessionShutdownHooks(
	rt *SessionRuntime,
	params RunnerParams,
	wb *wireBundle,
	sessionLog *event.SessionLogSink,
) {
	if sessionLog != nil {
		rt.AddHook("session-log", func(context.Context) error { return sessionLog.Close() })
	}
	if wb.histStore != nil {
		rt.AddHook("history", func(context.Context) error { return wb.histStore.Close() })
	}
	rt.AddHook("worktree", func(context.Context) error { return params.WorktreeSvc.RemoveAll() })
	rt.AddHook("mcp", func(context.Context) error { params.MCPMgr.Shutdown(); return nil })
	rt.AddHook("background", func(ctx context.Context) error {
		params.BGSvc.StopAll()
		return params.BGSvc.Wait(ctx)
	})
	rt.AddHook("team", func(ctx context.Context) error {
		wb.teamMgr.ShutdownAll()
		return wb.teamMgr.Wait(ctx)
	})
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
		Console:     app.consoleSink,
		Interactive: app.interactive(),
		DecisionLog: decisionLog,
		MemoryStore: app.memStore,
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
