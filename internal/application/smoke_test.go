package application_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go-code-agent/internal/application"
	"go-code-agent/internal/config"
	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/model/provider"
)

// stubProvider satisfies model.Provider without network I/O.
type stubProvider struct{}

func (stubProvider) Name() string { return "openai" }

func (stubProvider) Capabilities() model.ProviderCapabilities {
	return model.ProviderCapabilities{
		Streaming: true,
	}
}

func (stubProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	return &llm.Completion{Content: "ok", FinishReason: "stop"}, nil
}

func (stubProvider) Stream(ctx context.Context, params llm.CallParams, sink model.StreamSink) (*llm.StreamResult, error) {
	sink.OnTextDelta("ok")
	sink.OnDone()
	return &llm.StreamResult{Content: "ok", FinishReason: "stop"}, nil
}

func newTestApp(t *testing.T) (*application.Application, string, string) {
	t.Helper()
	t.Setenv("MCP_SERVERS", "")

	cfgDir := t.TempDir()
	workdir := t.TempDir()

	reg := provider.NewRegistry()
	reg.Register(stubProvider{})
	throttle := model.NewRoleThrottle(4)
	gw := model.NewGateway(stubProvider{}, throttle)

	cfg := &config.Config{
		ModelID:           "gpt-test",
		LLMProvider:       "openai",
		OpenAIAPIKey:      "test-not-used", // satisfies Validate; gateway is stubbed
		LLMMaxConcurrency: 4,
	}
	app, err := application.NewWithGateway(cfgDir, workdir, cfg, gw, reg)
	if err != nil {
		t.Fatalf("NewWithGateway: %v", err)
	}
	return app, cfgDir, workdir
}

func TestBuildExposesSanitizedRuntimeStatus(t *testing.T) {
	t.Setenv("MCP_SERVERS", "")
	cfgDir := t.TempDir()
	workdir := t.TempDir()
	baseURL := "https://user:password@Proxy.Example:8443/v1/chat?api_key=secret#private"
	primary := provider.NewOpenAI("test-not-used", baseURL)
	registry := provider.NewRegistry()
	registry.Register(primary)
	app, err := application.NewWithGateway(
		cfgDir,
		workdir,
		&config.Config{
			ModelID:           "gpt-5.2",
			LLMProvider:       "openai",
			OpenAIAPIKey:      "test-not-used",
			OpenAIBaseURL:     baseURL,
			ReasoningEnabled:  true,
			ReasoningEffort:   " HIGH ",
			LLMMaxConcurrency: 1,
		},
		model.NewGateway(primary, model.NewRoleThrottle(1)),
		registry,
	)
	if err != nil {
		t.Fatalf("NewWithGateway: %v", err)
	}
	defer app.Shutdown(context.Background())

	built, rt, err := app.Build(application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer rt.Close(context.Background())

	if built.Runtime.ProviderName != "openai" {
		t.Fatalf("ProviderName = %q, want openai", built.Runtime.ProviderName)
	}
	if built.Runtime.EndpointHost != "proxy.example:8443" {
		t.Fatalf("EndpointHost = %q, want proxy.example:8443", built.Runtime.EndpointHost)
	}
	if !built.Runtime.ReasoningRequested || !built.Runtime.ReasoningAvailable || built.Runtime.ReasoningEffort != "high" {
		t.Fatalf("reasoning runtime status = requested:%v available:%v effort:%q",
			built.Runtime.ReasoningRequested, built.Runtime.ReasoningAvailable, built.Runtime.ReasoningEffort)
	}
}

func TestBuild_Smoke_NewBuildClose(t *testing.T) {
	app, cfgDir, workdir := newTestApp(t)

	built, rt, err := app.Build(application.BuildOptions{NewSession: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if built == nil || rt == nil {
		t.Fatal("Build returned nil BuiltRunner or SessionRuntime")
	}

	// --- critical deps non-nil ---
	mustNonNil := map[string]any{
		"Runner":       built.Runtime.Runner,
		"HistStore":    built.Session.HistStore,
		"TeamMgr":      built.Team.Mgr,
		"Subagent":     built.Runtime.Subagent,
		"BGSvc":        built.Team.BG,
		"MCPMgr":       built.Team.MCP,
		"WorktreeSvc":  built.Team.Worktree,
		"Catalog":      built.Session.Catalog,
		"TaskSvc":      built.Tasks.Service,
		"TodoSvc":      built.Tasks.Todos,
		"MemoryStore":  built.Tasks.Memory,
		"HitlMgr":      built.Security.HITL,
		"Approval":     built.Security.Approval,
		"Permissions":  built.Security.Permissions,
		"DiffPreview":  built.Security.DiffPreview,
		"Bus":          built.Team.Bus,
		"SessionRepo":  built.Session.Repo,
		"WebService":   built.Runtime.Web,
		"Judge":        built.Runtime.Judge,
		"UsageTracker": built.Session.Usage,
		"DecisionLog":  built.Security.DecisionLog,
		"Compact":      built.Session.Compact,
	}
	for name, v := range mustNonNil {
		if v == nil {
			t.Errorf("BuiltRunner.%s is nil", name)
		}
	}
	if built.Session.ID == "" {
		t.Error("SessionID is empty")
	}
	if built.Session.AgentID != "lead" {
		t.Errorf("AgentID = %q, want lead", built.Session.AgentID)
	}
	if built.Session.Workdir != workdir {
		t.Errorf("Workdir = %q, want %q", built.Session.Workdir, workdir)
	}
	if built.Session.SysPrompt == "" {
		t.Error("SysPrompt is empty")
	}
	if built.Runtime.ProviderName != "openai" {
		t.Errorf("ProviderName = %q, want openai", built.Runtime.ProviderName)
	}
	if built.Runtime.EndpointHost != "api.openai.com" {
		t.Errorf("EndpointHost = %q, want api.openai.com", built.Runtime.EndpointHost)
	}
	if built.Runtime.ReasoningRequested || built.Runtime.ReasoningAvailable {
		t.Errorf("unexpected reasoning status: requested=%v available=%v", built.Runtime.ReasoningRequested, built.Runtime.ReasoningAvailable)
	}
	if snap := built.Session.Catalog.Load(); snap == nil || len(snap.Order) == 0 {
		t.Error("Catalog has no registered tools")
	}

	// --- isolated workspace/session directory structure ---
	dataDir := app.DataDir()
	if filepath.Dir(dataDir) != filepath.Join(cfgDir, "go-code-agent") {
		t.Errorf("DataDir parent = %q, want %q", filepath.Dir(dataDir), filepath.Join(cfgDir, "go-code-agent"))
	}
	if dataDir == filepath.Join(cfgDir, "go-code-agent", filepath.Base(workdir)) {
		t.Errorf("DataDir still uses collision-prone basename-only path: %q", dataDir)
	}
	sessionDir := filepath.Join(dataDir, "sessions", built.Session.ID)
	mustExist := []string{
		filepath.Join(dataDir, "sessions.json"),
		filepath.Join(sessionDir, "meta.json"),
		filepath.Join(sessionDir, "history"),
		filepath.Join(sessionDir, "tasks"),
		filepath.Join(sessionDir, "team", "inbox"),
	}
	for _, path := range mustExist {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected path missing: %s (%v)", path, err)
		}
	}
	// history.jsonl is created lazily on first append; lock the intended path.
	wantHist := filepath.Join(sessionDir, "history", history.FileName)
	if built.Session.HistStore.Path() != wantHist {
		t.Errorf("HistStore.Path() = %q, want %q", built.Session.HistStore.Path(), wantHist)
	}

	// --- Close idempotent ---
	ctx := context.Background()
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown after Close: %v", err)
	}
}
