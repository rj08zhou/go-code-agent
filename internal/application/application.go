// Package application provides the composition root (Application).
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go-code-agent/internal/agent"
	"go-code-agent/internal/config"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
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

	return NewWithGateway(cfgDir, workdir, cfg, gw, reg)
}

// NewWithGateway constructs an Application from a pre-built gateway/registry.
// Production code uses New(); tests inject a fake provider without API keys.
func NewWithGateway(cfgDir, workdir string, cfg *config.Config, gw *model.Gateway, reg *provider.Registry) (*Application, error) {
	if cfg == nil {
		cfg = &config.Config{ModelID: "default"}
	}
	if gw == nil {
		return nil, fmt.Errorf("gateway is required")
	}
	if reg == nil {
		reg = provider.NewRegistry()
	}

	dataDir := resolveDataDir(cfgDir, workdir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	return &Application{
		cfg:         cfg,
		cfgDir:      cfgDir,
		workdir:     workdir,
		dataDir:     dataDir,
		gateway:     gw,
		registry:    reg,
		sessionRepo: session.NewRepository(dataDir),
	}, nil
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

// DataDir returns the per-workspace state root.
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

// resolveDataDir computes a stable, isolated state directory from the
// canonical absolute workspace path. The basename keeps it recognizable while
// the hash prevents same-named workspaces from sharing state.
func resolveDataDir(cfgDir, workdir string) string {
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
func (app *Application) Build(opts BuildOptions) (*BuiltRunner, *SessionRuntime, error) {
	if opts.NewSession && opts.SessionID != "" {
		return nil, nil, errors.New("--session and --new-session cannot be used together")
	}
	for _, w := range app.Config().Validate() {
		fmt.Fprintf(os.Stderr, "[warn] %s\n", w)
	}

	repo := app.SessionRepo()
	idx, err := repo.LoadIndex()
	if err != nil {
		return nil, nil, fmt.Errorf("load sessions: %w", err)
	}

	var st *session.State
	switch {
	case opts.NewSession:
		// Force a new session below.
	case opts.SessionID != "":
		st, err = repo.LoadSessionMeta(opts.SessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("resume session %q: %w", opts.SessionID, err)
		}
		if err := repo.SwitchActive(opts.SessionID); err != nil {
			return nil, nil, fmt.Errorf("activate session %q: %w", opts.SessionID, err)
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
			return nil, nil, fmt.Errorf("create session %q: %w", st.ID, err)
		}
		idx.ActiveID = st.ID
		idx.Sessions = append(idx.Sessions, *st)
		if err := repo.SaveIndex(idx); err != nil {
			return nil, nil, fmt.Errorf("save sessions index: %w", err)
		}
	}
	if err := repo.EnsureSessionDir(st.ID); err != nil {
		return nil, nil, fmt.Errorf("ensure session directory: %w", err)
	}
	sessionDir, err := repo.SessionDir(st.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve session directory: %w", err)
	}

	// Each session gets its own ToolCatalog so MCP/builtin registration
	// cannot leak across session switches.
	catalog := tool.NewToolCatalog()
	rt := NewSessionRuntime(app.gateway, app.workdir, catalog, repo, st)

	workdir := app.workdir
	promptLoader := prompt.NewLoader()
	cfg := app.cfg
	if cfg == nil {
		cfg = &config.Config{ModelID: "default"}
	}

	hitlMgr := hitlaudit.NewHITLManager(promptLoader)
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
		if mode, modeErr := hitlaudit.ParseMode(opts.HumanMode); modeErr == nil {
			hitlMgr.SetMode(mode)
			syncApprovalWithHITLMode(approval, mode)
		} else {
			fmt.Fprintf(os.Stderr, "[warn] %v\n", modeErr)
		}
	}
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
	params := newSessionParams(app, sessionDir, workdir, hitlMgr, approval, perms, diffPreview, decisionLog, msgBus, promptLoader, cfg)

	built := rt.BuildRunner(params, sessionDir)
	built.Session.Usage = usageTracker
	built.Security.ReloadPermissions = func() error {
		// In-place reload: bash handler closes over this same pointer.
		return perms.Load(app.dataDir)
	}
	app.SetRuntime(rt)

	return built, rt, nil
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
