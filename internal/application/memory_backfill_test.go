package application

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/config"
	"go-code-agent/internal/gateway"
	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/session"
)

type backfillProvider struct {
	content string
	calls   int
}

func (p *backfillProvider) Name() string { return "openai" }
func (p *backfillProvider) Capabilities() gateway.ProviderCapabilities {
	return gateway.ProviderCapabilities{}
}
func (p *backfillProvider) Call(context.Context, llm.CallParams) (*llm.Completion, error) {
	p.calls++
	return &llm.Completion{Content: p.content, FinishReason: "stop"}, nil
}
func (p *backfillProvider) Stream(context.Context, llm.CallParams, gateway.StreamSink) (*llm.StreamResult, error) {
	return &llm.StreamResult{Content: p.content, FinishReason: "stop"}, nil
}

func TestParseMemoryItemsAcceptsFencedJSON(t *testing.T) {
	items, err := parseMemoryItems("```json\n[{\"content\":\"use gofmt\",\"category\":\"preference\"}]\n```")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["content"] != "use gofmt" || items[0]["category"] != "preference" {
		t.Fatalf("items = %#v", items)
	}
}

func TestSaveSessionToMemoryEndToEnd(t *testing.T) {
	root := t.TempDir()
	repo := session.NewRepository(root)
	active := &session.State{ID: "active", Title: "Active", Status: session.StatusActive}
	old := &session.State{ID: "old-session", Title: "Old", Status: session.StatusActive}
	for _, st := range []*session.State{active, old} {
		if err := repo.CreateSession(st); err != nil {
			t.Fatal(err)
		}
		if err := repo.EnsureSessionDir(st.ID); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := repo.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx.ActiveID = active.ID
	idx.Sessions = []session.State{*active, *old}
	if err := repo.SaveIndex(idx); err != nil {
		t.Fatal(err)
	}

	oldDir, err := repo.SessionDir(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(filepath.Join(oldDir, "history", history.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.AppendUser("please use gofmt"); err != nil {
		t.Fatal(err)
	}
	if err := hist.AppendAssistant("acknowledged", nil); err != nil {
		t.Fatal(err)
	}
	_ = hist.Close()

	prov := &backfillProvider{content: `[{"content":"user prefers gofmt","category":"preference"}]`}
	app := &Application{
		cfg:         &config.Config{ModelID: "gpt-test"},
		sessionRepo: repo,
		memStore:    memory.NewStore(root),
		gateway:     gateway.NewGateway(prov, gateway.NewRoleThrottle(1)),
		runtime:     &SessionRuntime{SessionState: active},
	}

	msg, err := app.saveSessionToMemory(context.Background(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "Saved 1 insights") {
		t.Fatalf("msg = %q", msg)
	}
	if prov.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", prov.calls)
	}
	st, err := repo.LoadSessionMeta(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.MemorySaved {
		t.Fatal("expected MemorySaved after successful backfill")
	}

	// Second call should skip LLM work.
	msg, err = app.saveSessionToMemory(context.Background(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "Memory already saved for this session." {
		t.Fatalf("second save = %q", msg)
	}
	if prov.calls != 1 {
		t.Fatalf("provider calls after saved session = %d, want 1", prov.calls)
	}
}

func TestSaveSessionToMemorySkipsActiveSession(t *testing.T) {
	root := t.TempDir()
	repo := session.NewRepository(root)
	active := &session.State{ID: "active", Title: "Active", Status: session.StatusActive}
	if err := repo.CreateSession(active); err != nil {
		t.Fatal(err)
	}
	idx, err := repo.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx.ActiveID = active.ID
	idx.Sessions = []session.State{*active}
	if err := repo.SaveIndex(idx); err != nil {
		t.Fatal(err)
	}
	prov := &backfillProvider{content: `[]`}
	app := &Application{
		sessionRepo: repo,
		memStore:    memory.NewStore(root),
		gateway:     gateway.NewGateway(prov, gateway.NewRoleThrottle(1)),
	}
	msg, err := app.saveSessionToMemory(context.Background(), active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "session is now active, skipping backfill." {
		t.Fatalf("msg = %q", msg)
	}
	if prov.calls != 0 {
		t.Fatalf("active session should not call the model")
	}
}

func TestStartMemoryBackfillRunsInBackground(t *testing.T) {
	root := t.TempDir()
	repo := session.NewRepository(root)
	active := &session.State{ID: "active", Title: "Active", Status: session.StatusActive}
	old := &session.State{ID: "old-session", Title: "Old", Status: session.StatusActive}
	for _, st := range []*session.State{active, old} {
		if err := repo.CreateSession(st); err != nil {
			t.Fatal(err)
		}
		if err := repo.EnsureSessionDir(st.ID); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := repo.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx.ActiveID = active.ID
	idx.Sessions = []session.State{*active, *old}
	if err := repo.SaveIndex(idx); err != nil {
		t.Fatal(err)
	}
	oldDir, err := repo.SessionDir(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := history.New(filepath.Join(oldDir, "history", history.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := hist.AppendUser("prefer dark mode"); err != nil {
		t.Fatal(err)
	}
	_ = hist.Close()

	prov := &backfillProvider{content: `[{"content":"prefers dark mode","category":"preference"}]`}
	app := &Application{
		cfg:         &config.Config{ModelID: "gpt-test"},
		sessionRepo: repo,
		memStore:    memory.NewStore(root),
		gateway:     gateway.NewGateway(prov, gateway.NewRoleThrottle(1)),
		runtime:     &SessionRuntime{SessionState: active},
	}
	app.StartMemoryBackfill(active.ID)
	app.WaitMemoryBackfill()

	st, err := repo.LoadSessionMeta(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.MemorySaved {
		t.Fatal("background backfill did not mark session MemorySaved")
	}
}
