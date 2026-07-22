// Package application provides the composition root (Application).
package application

import (
	"context"
	"fmt"
	"go-code-agent/internal/agent"
	"go-code-agent/internal/background"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/history"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/mcp"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/model"
	"go-code-agent/internal/model/provider"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/security"
	"go-code-agent/internal/session"
	"go-code-agent/internal/skill"
	"go-code-agent/internal/task"
	"go-code-agent/internal/team"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/web"
	"go-code-agent/internal/worktree"
	"os"
	"path/filepath"
	"strings"
)

// Application is the composition root — a thin object that wires
// ProjectServices together and manages SessionRuntime lifecycle.
// It holds NO mutable per-call state.
type Application struct {
	cfg     *config.Config
	cfgDir  string
	workdir string
	dataDir string

	// Project-level services (process lifetime)
	gateway     *model.Gateway
	registry    *provider.Registry
	sessionRepo *session.Repository

	// Embedded documentation
	Embedded []byte

	// Active runtime
	runtime *SessionRuntime
}

// New constructs the Application with all project-level services.
func New(cfgDir, workdir string) (*Application, error) {
	cfg := config.Load()
	config.SetConfig(cfg)

	// Resolve data directory
	dataDir := resolveDataDir(cfgDir, workdir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	// Build provider registry
	reg := provider.NewRegistry()

	// Register providers based on available credentials
	if cfg.OpenAIAPIKey != "" {
		reg.Register(provider.NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL))
	}
	if cfg.AnthropicAPIKey != "" {
		reg.Register(provider.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL))
	}

	// Build gateway
	gw, throttle, err := provider.BuildGateway(cfg, reg)
	if err != nil {
		return nil, fmt.Errorf("build gateway: %w", err)
	}
	for _, role := range []string{"lead", "explore", "teammate", "judge"} {
		fmt.Fprintf(os.Stderr, "[throttle] %s capacity=%d\n", role, throttle.Capacity(role))
	}

	// Session repository
	sessionRepo := session.NewRepository(dataDir)

	app := &Application{
		cfg:         cfg,
		cfgDir:      cfgDir,
		workdir:     workdir,
		dataDir:     dataDir,
		gateway:     gw,
		registry:    reg,
		sessionRepo: sessionRepo,
	}

	return app, nil
}

// Gateway returns the model gateway.
func (a *Application) Gateway() *model.Gateway { return a.gateway }

// Catalog returns the active session's tool catalog, or nil if no session.
func (a *Application) Catalog() *tool.ToolCatalog {
	if a.runtime == nil {
		return nil
	}
	return a.runtime.catalog
}

// SessionRepo returns the session repository.
func (a *Application) SessionRepo() *session.Repository { return a.sessionRepo }

// Workdir returns the project root.
func (a *Application) Workdir() string { return a.workdir }

// DataDir returns the per-project state root.
func (a *Application) DataDir() string { return a.dataDir }

// Config returns the process-wide configuration.
func (a *Application) Config() *config.Config { return a.cfg }

// Runtime returns the active session runtime or nil.
func (a *Application) Runtime() *SessionRuntime { return a.runtime }

// SetRuntime sets the active session runtime.
func (a *Application) SetRuntime(rt *SessionRuntime) { a.runtime = rt }

// Shutdown gracefully stops all services.
func (a *Application) Shutdown(ctx context.Context) error {
	if a.runtime != nil {
		a.runtime.Close(ctx)
	}
	return nil
}

// resolveDataDir computes the per-project state directory.
func resolveDataDir(cfgDir, workdir string) string {
	h := filepath.Base(workdir)
	return filepath.Join(cfgDir, "go-code-agent", h)
}

// --- SessionRuntime ---

// ShutdownHook is a named cleanup function called during session teardown.
// Hooks run in reverse registration order.
type ShutdownHook struct {
	Name string
	Fn   func() error
}

// SessionRuntime supervises a single session's running resources.
// It holds only the shared services it actually needs, not a circular
// reference back to Application.
type SessionRuntime struct {
	gateway     *model.Gateway
	workdir     string
	catalog     *tool.ToolCatalog
	sessionRepo *session.Repository

	SessionState *session.State
	Ctx          context.Context
	Cancel       context.CancelFunc
	hooks        []ShutdownHook
	closed       bool
}

// NewSessionRuntime creates a runtime for the given session state.
// It receives only the shared services it uses — no Application pointer.
func NewSessionRuntime(gw *model.Gateway, workdir string, catalog *tool.ToolCatalog, repo *session.Repository, st *session.State) *SessionRuntime {
	ctx, cancel := context.WithCancel(context.Background())
	return &SessionRuntime{
		gateway:      gw,
		workdir:      workdir,
		catalog:      catalog,
		sessionRepo:  repo,
		SessionState: st,
		Ctx:          ctx,
		Cancel:       cancel,
	}
}

// AddHook registers a cleanup hook. Hooks run in reverse registration order on Close.
func (rt *SessionRuntime) AddHook(name string, fn func() error) {
	rt.hooks = append(rt.hooks, ShutdownHook{Name: name, Fn: fn})
}

// RunnerParams bundles all session-scoped services needed by BuildRunner.
// This keeps main.go thin — it only creates services and passes this struct.
// Pointer-to-struct is required because many tool interfaces use pointer receivers.
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
	MCPMgr       *mcp.Manager
	WorktreeSvc  *worktree.Service
	Protocols    *team.ProtocolStore
	PromptLoader *prompt.Loader
	Permissions  *security.Permissions
}

// BuiltRunner holds a fully-wired Runner together with all session services
// needed by the REPL and shutdown hooks.
type BuiltRunner struct {
	Runner            *agent.Runner
	Compact           func(context.Context, []llm.Message) []llm.Message
	SysPrompt         string
	HistStore         *history.Store
	TeamMgr           *agent.TeammateManager
	Subagent          *agent.SubagentRunner
	BGSvc             *background.Supervisor
	MCPMgr            *mcp.Manager
	WorktreeSvc       *worktree.Service
	Catalog           *tool.ToolCatalog
	TaskSvc           *task.Service
	TodoSvc           *task.TodoManager
	MemoryStore       *memory.Store
	HitlMgr           *hitlaudit.HITLManager
	Approval          *security.ApprovalState
	Judge             *agent.Judge
	Permissions       *security.Permissions
	DiffPreview       *security.DiffPreview
	UsageTracker      *agent.UsageTracker
	Bus               *team.MessageBus
	AgentID           string
	SessionID         string
	DecisionLog       *agent.DecisionLog
	WebService        tool.WebService
	ReloadPermissions func() error
	SessionRepo       *session.Repository
	SessionTitle      string
	Workdir           string
	ModelID           string
	JudgeEnabled      bool
}

// BuildRunner is the composition root for a single session run.
// It wires together all services, tools, modules, and returns a fully-configured Runner.
//
// Usage in main.go:
//
//	params := application.RunnerParams{...}
//	built := runtime.BuildRunner(params)
//	runner := built.Runner
//	defer built.Close()
func (rt *SessionRuntime) BuildRunner(params RunnerParams) *BuiltRunner {
	st := rt.SessionState
	cfg := config.CurrentConfig()
	sessionDir := rt.sessionRepo.SessionDir(st.ID)

	// --- Tool catalog ---
	catalog := rt.catalog

	// --- Executor with HITL + secrets sanitizer ---
	hitlApproval := hitlaudit.NewHITLApprovalAdapter(params.HITLMgr)
	hitlApproval.SetWorkdir(rt.workdir)
	exec := tool.NewExecutor(catalog, hitlApproval, nil).
		WithSanitizer(security.NewSecretsSanitizer()).
		WithDecisionLogger(params.DecisionLog)

	// --- Subagent & team ---
	subagentRunner := agent.NewSubagentRunner(rt.gateway, catalog, cfg.ModelID)
	subagentRunner.SetCompression(agent.NewCompression(rt.gateway, nil, sessionDir, cfg.ModelID))
	subagentRunner.SetApproval(hitlApproval)
	teamMgr := agent.NewTeammateManager(
		filepath.Join(sessionDir, "team"), rt.gateway,
		params.Bus, params.TaskSvc, params.Protocols, params.WorktreeSvc,
		catalog, cfg.ModelID,
	)
	teamMgr.SetSessionCtx(rt.Ctx)
	teamMgr.SetDiffPreview(params.DiffPreview)
	teamMgr.SetApproval(hitlApproval)

	// --- Register builtin tools FIRST (establishes base snapshot + order) ---
	builtinDefs := tool.BuiltinTools(
		params.TaskSvc, params.TodoSvc, params.MemoryStore, params.SkillLoader, params.BGSvc, params.Bus,
		subagentRunner,
		teamMgr,
		params.Protocols,
		params.WebService,
		params.Permissions,
	)
	catalog.RegisterAll(builtinDefs)

	// --- MCP tools are merged in AFTER builtins (Register is additive; if
	// this ran through RegisterAll it would wipe out the builtin tools
	// registered above, since RegisterAll always replaces the whole
	// snapshot). Order also matters for prompt-prefix caching: tool
	// schemas sit at the front of every LLM request, so the catalog's
	// tool order must stay stable across calls within a session.
	mcpAdapter := mcp.NewToolCatalogAdapter(catalog, params.MCPMgr)
	params.MCPMgr.SetRegistry(mcpAdapter)
	params.MCPMgr.LoadAndStart(rt.Ctx)

	// --- System prompt ---
	sysPrompt := agent.NewSystemPromptBuilder(
		params.PromptLoader, params.SkillLoader, params.MemoryStore,
		params.TaskSvc, func() string {
			return params.MCPMgr.List() + "\n" + params.MCPMgr.ServerInstructions()
		}, nil,
	).Build(rt.workdir)

	// --- Runner ---
	profile := agent.NewLeadProfile(sysPrompt)
	scope := &tool.ToolScope{
		Role:        "lead",
		Workdir:     rt.workdir,
		SessionID:   st.ID,
		AgentID:     "lead",
		ProjectID:   rt.workdir,
		DiffPreview: params.DiffPreview,
	}
	runner := agent.NewRunner(profile, rt.gateway, exec, scope)
	if params.MemoryStore != nil {
		runner.SetLessonWriter(agent.NewLLMLessonWriter(rt.gateway, params.MemoryStore, params.PromptLoader, cfg.ModelID))
	}
	if params.MemoryStore != nil {
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

	// --- Wire runner modules ---
	histStore, histErr := history.New(filepath.Join(sessionDir, "history", history.FileName))
	if histErr != nil {
		fmt.Fprintf(os.Stderr, "[warn] history store: %v\n", histErr)
	}
	runner.SetCompression(agent.NewCompression(rt.gateway, histStore, sessionDir, cfg.ModelID))
	runner.SetReflection(agent.NewReflection(params.PromptLoader))
	runner.SetSnapshot(agent.NewSnapshotManager(cfg.SnapshotEnabled, rt.workdir))
	runner.SetSubagentRunner(subagentRunner)
	runner.SetPlanGate(agent.NewPlanGate(params.PromptLoader, params.TaskSvc))

	// --- Event sinks ---
	sinks := []event.Sink{event.NewConsoleSink(), event.NewAuditSink(), event.NewUsageSink()}
	if sessionLog, logErr := event.NewSessionLogSink(filepath.Join(sessionDir, "session.log")); logErr != nil {
		fmt.Fprintf(os.Stderr, "[warn] session.log: %v\n", logErr)
	} else {
		sinks = append(sinks, sessionLog)
		rt.AddHook("session-log", sessionLog.Close)
	}
	allEvents := event.NewMultiSink(sinks...)
	runner.SetEventSink(allEvents)
	subagentRunner.SetEventSink(allEvents)
	teamMgr.SetEventSink(allEvents)

	// --- Shutdown hooks ---
	rt.AddHook("team", func() error { teamMgr.ShutdownAll(); teamMgr.Wait(); return nil })
	rt.AddHook("mcp", func() error { params.MCPMgr.Shutdown(); return nil })
	rt.AddHook("background", func() error { params.BGSvc.StopAll(); return nil })
	rt.AddHook("worktree", func() error { params.WorktreeSvc.RemoveAll(); return nil })
	if histStore != nil {
		rt.AddHook("history", func() error { return histStore.Close() })
	}

	judgeInst := agent.NewJudge(cfg.JudgeEnabled, cfg.JudgeModel,
		cfg.JudgeMinScore, params.PromptLoader, rt.gateway)
	runner.SetJudge(judgeInst)

	return &BuiltRunner{
		Runner:      runner,
		Compact:     runner.ManualCompact,
		SysPrompt:   sysPrompt,
		HistStore:   histStore,
		TeamMgr:     teamMgr,
		Subagent:    subagentRunner,
		BGSvc:       params.BGSvc,
		MCPMgr:      params.MCPMgr,
		WorktreeSvc: params.WorktreeSvc,
		Catalog:     catalog,
		TaskSvc:     params.TaskSvc,
		TodoSvc:     params.TodoSvc,
		MemoryStore: params.MemoryStore,
		HitlMgr:     params.HITLMgr,
		Judge:       judgeInst,
	}
}

// Close stops all session resources in order:
// 1. Cancel the context (signals all goroutines)
// 2. Run shutdown hooks in reverse registration order
func (rt *SessionRuntime) Close(ctx context.Context) error {
	if rt.closed {
		return nil
	}
	rt.closed = true
	if rt.Cancel != nil {
		rt.Cancel()
	}
	// Run hooks in reverse order (last-registered = first-shutdown)
	var errs []string
	for i := len(rt.hooks) - 1; i >= 0; i-- {
		h := rt.hooks[i]
		if err := h.Fn(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("shutdown errors: %v", errs)
	}
	return nil
}

// BuildOptions control session startup behaviour.
type BuildOptions struct {
	SessionID  string // resume specific session; empty = use active or create new
	NewSession bool   // always create a fresh session
	Human      bool   // enable HITL on startup
	HumanMode  string // interactive / auto-approve / auto-reject
}

// Build creates the session, wires all services, assembles the runner,
// and returns a fully-configured BuiltRunner together with the SessionRuntime.
// The caller (main / repl) owns the REPL loop and shutdown.
func (app *Application) Build(opts BuildOptions) (*BuiltRunner, *SessionRuntime) {
	for _, w := range app.Config().Validate() {
		fmt.Fprintf(os.Stderr, "[warn] %s\n", w)
	}

	repo := app.SessionRepo()
	idx, _ := repo.LoadIndex()

	var st *session.State
	if opts.NewSession {
		// Force new session
	} else if opts.SessionID != "" {
		st, _ = repo.LoadSessionMeta(opts.SessionID)
	} else if idx.ActiveID != "" {
		st, _ = repo.LoadSessionMeta(idx.ActiveID)
	}
	if st == nil {
		sid := session.NewSessionID()
		st = &session.State{
			ID:     sid,
			Title:  "Session " + sid[:10],
			Status: session.StatusActive,
		}
		if err := repo.CreateSession(st); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] create session: %v\n", err)
		}
		idx.ActiveID = st.ID
		idx.Sessions = append(idx.Sessions, *st)
		if err := repo.SaveIndex(idx); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] save sessions index: %v\n", err)
		}
	}
	// Resumed sessions (and create failures) must still have an on-disk dir
	// before usage/session.log/history open files under it.
	if err := repo.EnsureSessionDir(st.ID); err != nil {
		fmt.Fprintf(os.Stderr, "[warn] ensure session dir: %v\n", err)
	}

	// Each session gets its own ToolCatalog so MCP/builtin registration
	// cannot leak across session switches.
	catalog := tool.NewToolCatalog()
	rt := NewSessionRuntime(app.gateway, app.workdir, catalog, repo, st)
	app.SetRuntime(rt)

	workdir := app.workdir
	sessionDir := repo.SessionDir(st.ID)
	promptLoader := prompt.NewLoader()
	cfg := config.CurrentConfig()

	hitlMgr := hitlaudit.NewHITLManager(promptLoader)
	approval := security.NewApprovalState()
	// Default: HITL on + safe-only. --human alone escalates to interactive.
	// --human-mode overrides only when explicitly set (empty = keep default).
	hitlMgr.SetEnabled(true)
	hitlMgr.SetMode(hitlaudit.HITLModeSafeOnly)
	approval.ApplyPreset("safe")
	if opts.Human && opts.HumanMode == "" {
		hitlMgr.SetMode(hitlaudit.HITLModeInteractive)
		approval.ApplyPreset("off")
	}
	if opts.HumanMode != "" {
		if mode, modeErr := hitlaudit.ParseMode(opts.HumanMode); modeErr == nil {
			hitlMgr.SetMode(mode)
			syncApprovalWithHITLMode(approval, mode)
		} else {
			fmt.Fprintf(os.Stderr, "[warn] %v\n", modeErr)
		}
	}
	security.SetActiveApproval(approval)

	// Permissions + DiffPreview + Usage tracker
	perms := security.NewPermissions()
	_ = perms.Load(app.dataDir)

	diffPreview := security.NewDiffPreview(workdir)
	usageTracker, usageErr := agent.NewUsageTracker(sessionDir)
	if usageErr != nil {
		fmt.Fprintf(os.Stderr, "[warn] usage tracker: %v\n", usageErr)
	}
	decisionLog, _ := agent.NewDecisionLog(sessionDir)
	if usageTracker != nil {
		app.gateway.SetUsageRecorder(func(role, providerName, modelID, traceID string, usage llm.Usage, duration float64) {
			usageTracker.Record(providerName, role, modelID, traceID, usage, duration)
		})
	}

	msgBus := team.NewBus(filepath.Join(sessionDir, "team", "inbox"))
	params := RunnerParams{
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
		MCPMgr:       mcp.NewManager(workdir),
		WorktreeSvc:  worktree.New(workdir, sessionDir),
		Protocols:    team.NewProtocolStore(msgBus),
		PromptLoader: promptLoader,
		Permissions:  perms,
	}

	built := rt.BuildRunner(params)
	built.SessionID = st.ID
	built.SessionTitle = st.Title
	built.Workdir = workdir
	built.ModelID = cfg.ModelID
	built.JudgeEnabled = cfg.JudgeEnabled
	built.Permissions = perms
	built.Approval = approval
	built.DiffPreview = diffPreview
	built.UsageTracker = usageTracker
	built.AgentID = "lead"
	built.Bus = msgBus
	built.SessionRepo = repo
	built.DecisionLog = decisionLog
	built.WebService = params.WebService
	built.ReloadPermissions = func() error {
		// In-place reload: bash handler closes over this same pointer.
		return perms.Load(app.dataDir)
	}

	return built, rt
}

// syncApprovalWithHITLMode keeps ApprovalState (diff-preview skip) aligned
// with an explicit --human-mode / /hitl selection.
func syncApprovalWithHITLMode(approval *security.ApprovalState, mode hitlaudit.HITLMode) {
	if approval == nil {
		return
	}
	switch mode {
	case hitlaudit.HITLModeAutoApprove:
		approval.ApplyPreset("danger")
	case hitlaudit.HITLModeSafeOnly:
		approval.ApplyPreset("safe")
	default:
		// interactive / auto-reject / notify-only → keep manual posture for preview
		approval.ApplyPreset("off")
	}
}
