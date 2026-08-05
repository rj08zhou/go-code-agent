// Package application provides the composition root (Application).
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go-code-agent/internal/agent"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/model"
	"go-code-agent/internal/model/provider"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/security"
	"go-code-agent/internal/session"
	"go-code-agent/internal/team"
	"go-code-agent/internal/tool"
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
	gateway       *model.Gateway
	registry      *provider.Registry
	sessionRepo   *session.Repository
	memStore      *memory.Store
	consoleSink   *event.ConsoleSink
	interactiveIO security.InteractiveIO

	// Active runtime
	runtime         *SessionRuntime
	runtimeCloseErr error

	// Background session→memory distillation (non-blocking on exit/switch).
	backfillWG sync.WaitGroup
}

// Option configures optional process-level dependencies at construction time.
type Option func(*bootConfig)

// bootConfig accumulates construction options before Application fields are
// finalized. ConsoleSink is owned by Application; the CLI only supplies a
// line reader and TTY flag.
type bootConfig struct {
	consoleSink *event.ConsoleSink
	readLine    func(string) (string, error)
	isTTY       bool
	hasReader   bool
}

// WithConsoleSink replaces the default process console event sink. Rarely
// needed by the CLI; useful for tests or custom hosts.
func WithConsoleSink(sink *event.ConsoleSink) Option {
	return func(c *bootConfig) {
		if sink != nil {
			c.consoleSink = sink
		}
	}
}

// WithInteractiveReader attaches the host line reader used by HITL and diff
// review. Application builds InteractiveIO from its own ConsoleSink so event
// output and approval prompts share one writer.
func WithInteractiveReader(readLine func(string) (string, error), isTTY bool) Option {
	return func(c *bootConfig) {
		c.readLine = readLine
		c.isTTY = isTTY
		c.hasReader = true
	}
}

// New constructs the Application with all project-level services.
func New(cfgDir, workdir string, opts ...Option) (*Application, error) {
	cfg := config.Load()

	reg := provider.NewRegistry()
	if cfg.OpenAIAPIKey != "" {
		reg.Register(provider.NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL))
	}
	if cfg.AnthropicAPIKey != "" {
		reg.Register(provider.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL))
	}

	gw, throttle, err := provider.BuildGateway(cfg, reg)
	if err != nil {
		return nil, fmt.Errorf("build gateway: %w", err)
	}
	for _, role := range []string{"lead", "explore", "teammate", "judge"} {
		fmt.Fprintf(os.Stderr, "[throttle] %s capacity=%d\n", role, throttle.Capacity(role))
	}

	return NewWithGateway(cfgDir, workdir, cfg, gw, reg, opts...)
}

// NewWithGateway constructs an Application from a pre-built gateway/registry.
// Production code uses New(); tests inject a fake provider without API keys.
func NewWithGateway(cfgDir, workdir string, cfg *config.Config, gw *model.Gateway, reg *provider.Registry, opts ...Option) (*Application, error) {
	if cfg == nil {
		cfg = &config.Config{ModelID: "default"}
	}
	if gw == nil {
		return nil, fmt.Errorf("gateway is required")
	}
	if reg == nil {
		reg = provider.NewRegistry()
	}

	dataDir := ResolveDataDir(cfgDir, workdir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	var boot bootConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&boot)
		}
	}
	consoleSink := boot.consoleSink
	if consoleSink == nil {
		consoleSink = event.NewConsoleSink()
	}
	var interactiveIO security.InteractiveIO
	if boot.hasReader {
		interactiveIO = security.NewInteractiveIO(consoleSink, boot.readLine, boot.isTTY)
	}

	return &Application{
		cfg:           cfg,
		cfgDir:        cfgDir,
		workdir:       workdir,
		dataDir:       dataDir,
		gateway:       gw,
		registry:      reg,
		sessionRepo:   session.NewRepository(dataDir),
		memStore:      memory.NewStore(dataDir),
		consoleSink:   consoleSink,
		interactiveIO: interactiveIO,
	}, nil
}

// Gateway returns the model gateway.
func (a *Application) Gateway() *model.Gateway { return a.gateway }

// SessionRepo returns the session repository.
func (a *Application) SessionRepo() *session.Repository { return a.sessionRepo }

// Workdir returns the project root.
func (a *Application) Workdir() string { return a.workdir }

// DataDir returns the per-workspace state root.
func (a *Application) DataDir() string { return a.dataDir }

// Config returns the process-wide configuration.
func (a *Application) Config() *config.Config { return a.cfg }

func (a *Application) interactive() security.InteractiveIO {
	if a != nil && a.interactiveIO != nil {
		return a.interactiveIO
	}
	if a != nil && a.consoleSink != nil {
		return security.NewInteractiveIO(a.consoleSink, nil, false)
	}
	return security.DefaultInteractiveIO()
}

// CloseSession stops the active session runtime and releases its ownership.
func (a *Application) CloseSession(ctx context.Context) error {
	rt := a.runtime
	if rt == nil {
		return a.runtimeCloseErr
	}
	a.runtime = nil
	a.runtimeCloseErr = rt.Close(ctx)
	return a.runtimeCloseErr
}

// Shutdown gracefully stops all services.
func (a *Application) Shutdown(ctx context.Context) error {
	return a.CloseSession(ctx)
}

// ResolveDataDir computes a stable, isolated state directory from the
// canonical absolute workspace path. The basename keeps it recognizable while
// the hash prevents same-named workspaces from sharing state. Exported so the
// CLI can open the terminal against the same path before constructing
// Application.
func ResolveDataDir(cfgDir, workdir string) string {
	canonical, err := filepath.Abs(workdir)
	if err != nil {
		canonical = workdir
	}
	canonical = filepath.Clean(canonical)
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		canonical = resolved
	}

	base := filepath.Base(canonical)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "workspace"
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(cfgDir, "go-code-agent", fmt.Sprintf("%s-%x", base, sum[:8]))
}

// --- SessionRuntime ---

// ShutdownHook is a named cleanup function called during session teardown.
// Hooks run in reverse registration order.
type ShutdownHook struct {
	Name string
	Fn   func(context.Context) error
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
	closeErr     error
}

// NewSessionRuntime creates a runtime for the given session state.
// It receives only the shared services it uses — no Application pointer.
func NewSessionRuntime(parent context.Context, gw *model.Gateway, workdir string, catalog *tool.ToolCatalog, repo *session.Repository, st *session.State) *SessionRuntime {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
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
func (rt *SessionRuntime) AddHook(name string, fn func(context.Context) error) {
	rt.hooks = append(rt.hooks, ShutdownHook{Name: name, Fn: fn})
}

// Close stops all session resources in order:
// 1. Cancel the context (signals all goroutines)
// 2. Run shutdown hooks in reverse registration order
func (rt *SessionRuntime) Close(ctx context.Context) error {
	if rt.closed {
		return rt.closeErr
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rt.closed = true
	if rt.Cancel != nil {
		rt.Cancel()
	}
	// Run hooks in reverse order (last-registered = first-shutdown)
	var errs []string
	for i := len(rt.hooks) - 1; i >= 0; i-- {
		h := rt.hooks[i]
		done := make(chan error, 1)
		go func() {
			done <- h.Fn(ctx)
		}()
		var err error
		select {
		case err = <-done:
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h.Name, err))
		}
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) > 0 {
		rt.closeErr = fmt.Errorf("shutdown errors: %v", errs)
	}
	return rt.closeErr
}

// BuildOptions control session startup behaviour.
type BuildOptions struct {
	SessionID  string // resume specific session; empty = use active or create new
	NewSession bool   // always create a fresh session
	Human      bool   // enable HITL on startup
	HumanMode  string // interactive / auto-approve / auto-reject
}

type openedSession struct {
	state              *session.State
	dir                string
	requiresActivation bool
}

type sessionSecurity struct {
	hitlMgr      *hitlaudit.HITLManager
	approval     *security.ApprovalState
	permissions  *security.Permissions
	diffPreview  *security.DiffPreview
	promptLoader *prompt.Loader
	cfg          *config.Config
}

// Build creates the session under ctx, wires all services, and returns a
// fully-configured BuiltRunner. Application owns the resulting SessionRuntime.
// Canceling ctx cancels the session lifetime; a user turn must use its own
// child context so interrupting one turn does not tear down the session.
// The caller owns the REPL loop and requests teardown through CloseSession.
func (app *Application) Build(ctx context.Context, opts BuildOptions) (*BuiltRunner, error) {
	ctx, err := validateBuildContext(ctx, opts)
	if err != nil {
		return nil, err
	}
	if app.runtime != nil {
		return nil, errors.New("active session runtime must be closed before building another session")
	}
	app.reportConfigWarnings()

	opened, err := app.openSession(opts)
	if err != nil {
		return nil, err
	}

	// Each session gets its own ToolCatalog so MCP/builtin registration
	// cannot leak across session switches.
	catalog := tool.NewToolCatalog()
	rt := NewSessionRuntime(ctx, app.gateway, app.workdir, catalog, app.sessionRepo, opened.state)
	var usageTracker *agent.UsageTracker
	rt.AddHook("usage", func(context.Context) error {
		if usageTracker == nil {
			return nil
		}
		return usageTracker.Close()
	})

	securitySetup := app.configureSessionSecurity(opts)
	decisionLog, _ := agent.NewDecisionLog(opened.dir)
	msgBus := team.NewBus(filepath.Join(opened.dir, "team", "inbox"))
	params := newSessionParams(
		app,
		opened.dir,
		app.workdir,
		securitySetup.hitlMgr,
		securitySetup.approval,
		securitySetup.permissions,
		securitySetup.diffPreview,
		decisionLog,
		msgBus,
		securitySetup.promptLoader,
		securitySetup.cfg,
	)

	built, err := rt.BuildRunner(params, opened.dir)
	if err != nil {
		return nil, closeRuntimeAfterBuildFailure(rt, fmt.Errorf("build runner: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return nil, closeRuntimeAfterBuildFailure(rt, err)
	}

	usageTracker, usageErr := agent.NewUsageTracker(opened.dir)
	if usageErr != nil {
		fmt.Fprintf(os.Stderr, "[warn] usage tracker: %v\n", usageErr)
	}
	built.Session.Usage = usageTracker
	built.Security.ReloadPermissions = func() error {
		// In-place reload: bash handler closes over this same pointer.
		return securitySetup.permissions.Load(app.dataDir)
	}

	if err := app.activateSessionRuntime(opened, rt, usageTracker); err != nil {
		return nil, closeRuntimeAfterBuildFailure(rt, err)
	}

	// Distill inactive unsaved sessions in the background so exit/switch
	// paths stay non-blocking (legacy SessionManager.BackfillMemory).
	app.StartMemoryBackfill(opened.state.ID)

	return built, nil
}

func validateBuildContext(ctx context.Context, opts BuildOptions) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.NewSession && opts.SessionID != "" {
		return nil, errors.New("--session and --new-session cannot be used together")
	}
	return ctx, nil
}

func (app *Application) reportConfigWarnings() {
	for _, warning := range app.Config().Validate() {
		fmt.Fprintf(os.Stderr, "[warn] %s\n", warning)
	}
}

func (app *Application) openSession(opts BuildOptions) (openedSession, error) {
	repo := app.SessionRepo()
	idx, err := repo.LoadIndex()
	if err != nil {
		return openedSession{}, fmt.Errorf("load sessions: %w", err)
	}

	var st *session.State
	switch {
	case opts.NewSession:
		// Force a new session below.
	case opts.SessionID != "":
		st, err = repo.LoadSessionMeta(opts.SessionID)
		if err != nil {
			return openedSession{}, fmt.Errorf("resume session %q: %w", opts.SessionID, err)
		}
	case idx.ActiveID != "":
		st, err = repo.LoadSessionMeta(idx.ActiveID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] active session %q is unavailable: %v; creating a new session\n", idx.ActiveID, err)
			st = nil
		}
	}
	if st == nil {
		sid := session.NewSessionID()
		st = &session.State{
			ID:     sid,
			Title:  "Session " + sid[:10],
			Status: session.StatusActive,
		}
		if err := repo.CreateSession(st); err != nil {
			return openedSession{}, fmt.Errorf("create session %q: %w", st.ID, err)
		}
		idx.Sessions = append(idx.Sessions, *st)
		if err := repo.SaveIndex(idx); err != nil {
			return openedSession{}, fmt.Errorf("save sessions index: %w", err)
		}
	}
	if err := repo.EnsureSessionDir(st.ID); err != nil {
		return openedSession{}, fmt.Errorf("ensure session directory: %w", err)
	}
	sessionDir, err := repo.SessionDir(st.ID)
	if err != nil {
		return openedSession{}, fmt.Errorf("resolve session directory: %w", err)
	}
	return openedSession{
		state:              st,
		dir:                sessionDir,
		requiresActivation: idx.ActiveID != st.ID,
	}, nil
}

func (app *Application) configureSessionSecurity(opts BuildOptions) sessionSecurity {
	promptLoader := prompt.NewLoader()
	cfg := app.cfg
	if cfg == nil {
		cfg = &config.Config{ModelID: "default"}
	}

	hitlMgr := hitlaudit.NewHITLManager(promptLoader, app.interactive())
	approval := security.NewApprovalState()
	// Default approval mode is safe-auto. --human alone escalates to manual.
	// --human-mode is retained as the advanced compatibility override.
	hitlMgr.SetEnabled(true)
	hitlMgr.SetMode(hitlaudit.HITLModeSafeOnly)
	approval.ApplyPreset("safe-auto")
	if opts.Human && opts.HumanMode == "" {
		hitlMgr.SetMode(hitlaudit.HITLModeInteractive)
		approval.ApplyPreset("manual")
	}
	if opts.HumanMode != "" {
		if mode, err := hitlaudit.ParseMode(opts.HumanMode); err == nil {
			hitlMgr.SetMode(mode)
			syncApprovalWithHITLMode(approval, mode)
		} else {
			fmt.Fprintf(os.Stderr, "[warn] %v\n", err)
		}
	}

	permissions := security.NewPermissions()
	_ = permissions.Load(app.dataDir)
	return sessionSecurity{
		hitlMgr:      hitlMgr,
		approval:     approval,
		permissions:  permissions,
		diffPreview:  security.NewDiffPreview(app.workdir),
		promptLoader: promptLoader,
		cfg:          cfg,
	}
}

func closeRuntimeAfterBuildFailure(rt *SessionRuntime, buildErr error) error {
	if closeErr := rt.Close(context.Background()); closeErr != nil {
		return fmt.Errorf("%w; cleanup runtime: %v", buildErr, closeErr)
	}
	return buildErr
}

func (app *Application) activateSessionRuntime(opened openedSession, rt *SessionRuntime, usageTracker *agent.UsageTracker) error {
	if opened.requiresActivation {
		if err := app.sessionRepo.SwitchActive(opened.state.ID); err != nil {
			return fmt.Errorf("activate session %q: %w", opened.state.ID, err)
		}
	}
	if usageTracker != nil {
		app.gateway.SetUsageRecorder(func(role, providerName, modelID, traceID string, usage llm.Usage, duration float64) {
			usageTracker.Record(providerName, role, modelID, traceID, usage, duration)
		})
	}
	app.runtime = rt
	app.runtimeCloseErr = nil
	return nil
}

// syncApprovalWithHITLMode keeps ApprovalState (diff-preview skip) aligned
// with the advanced --human-mode compatibility override.
func syncApprovalWithHITLMode(approval *security.ApprovalState, mode hitlaudit.HITLMode) {
	if approval == nil {
		return
	}
	switch mode {
	case hitlaudit.HITLModeAutoApprove:
		approval.ApplyPreset("all-auto")
	case hitlaudit.HITLModeSafeOnly:
		approval.ApplyPreset("safe-auto")
	default:
		approval.ApplyPreset("manual")
	}
}
