package tool

import (
	"context"
	"encoding/json"
	"go-code-agent/internal/llm"
	"testing"
)

func newTestCatalog() *ToolCatalog {
	c := NewToolCatalog()
	noop := func(scope *ToolScope, args json.RawMessage) Result { return Succeeded("ok") }
	c.RegisterAll([]ToolDefinition{
		{Name: "read", Description: "", Effects: Effects(EffectReadFile), Handler: noop},
		{Name: "write", Description: "", Effects: Effects(EffectWriteFile), Handler: noop},
		{Name: "delete", Description: "", Effects: Effects(EffectDeleteFile), Handler: noop},
		{Name: "bash", Description: "", Effects: Effects(EffectExecuteProcess), Handler: noop},
		{Name: "fetch", Description: "", Effects: Effects(EffectNetworkAccess), Handler: noop},
		{Name: "memory", Description: "", Effects: Effects(EffectMemoryMutation), Handler: noop},
		{Name: "team", Description: "", Effects: Effects(EffectTeamMutation), Handler: noop},
	})
	return c
}

// --- Capability gating ---

func TestExecutor_DeniesWhenMissingCapability(t *testing.T) {
	catalog := newTestCatalog()
	exec := NewExecutor(catalog, nil, nil)

	tests := []struct {
		tool  string
		scope *ToolScope
	}{
		{"read", &ToolScope{Role: "explore", CanRead: false}},
		{"write", &ToolScope{Role: "explore", CanWrite: false}},
		{"delete", &ToolScope{Role: "explore", CanWrite: false}},
		{"bash", &ToolScope{Role: "explore", CanExecute: false}},
		{"fetch", &ToolScope{Role: "explore", CanNetwork: false}},
		{"memory", &ToolScope{Role: "explore", CanMemory: false}},
		{"team", &ToolScope{Role: "explore", CanTeam: false}},
	}
	for _, tt := range tests {
		r := exec.Execute(context.Background(), tt.scope, llm.ToolCall{Name: tt.tool, Arguments: "{}"})
		if r.Status != StatusDenied {
			t.Errorf("%s: expected StatusDenied, got %s", tt.tool, r.Status)
		}
	}
}

func TestExecutor_AllowsWhenCapabilityGranted(t *testing.T) {
	catalog := newTestCatalog()
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{Role: "lead", CanRead: true, CanWrite: true, CanExecute: true, CanNetwork: true, CanMemory: true, CanTeam: true}

	tools := []string{"read", "write", "delete", "bash", "fetch", "memory", "team"}
	for _, name := range tools {
		r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: name, Arguments: "{}"})
		if r.Status != StatusSucceeded {
			t.Errorf("%s: expected StatusSucceeded, got %s: %s", name, r.Status, r.Output)
		}
	}
}

// --- HITL / Approval ---

func TestExecutor_RejectsWhenApprovalDenies(t *testing.T) {
	catalog := newTestCatalog()
	denyAll := &staticApproval{allow: false, reason: "blocked"}
	exec := NewExecutor(catalog, denyAll, nil)
	scope := &ToolScope{Role: "lead", CanWrite: true}

	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "write", Arguments: `{"path":"x"}`})
	if r.Status != StatusRejected {
		t.Fatalf("expected StatusRejected, got %s", r.Status)
	}
	if r.Output == "" {
		t.Fatal("expected non-empty rejection reason")
	}
}

func TestExecutor_ScopeApprovalOverrides(t *testing.T) {
	catalog := newTestCatalog()
	// Global allows all; scope denies all → scope wins
	globalOK := &staticApproval{allow: true, reason: ""}
	scopeDeny := &staticApproval{allow: false, reason: "scope-block"}
	exec := NewExecutor(catalog, globalOK, nil)
	scope := &ToolScope{Role: "lead", CanWrite: true, ApprovalPolicy: scopeDeny}

	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "write", Arguments: `{}`})
	if r.Status != StatusRejected {
		t.Fatalf("expected StatusRejected from scope approval, got %s", r.Status)
	}
}

// --- Network policy ---

func TestExecutor_BlocksURLWithNetworkPolicy(t *testing.T) {
	catalog := NewToolCatalog()
	catalog.RegisterAll([]ToolDefinition{{
		Name: "web_fetch", Description: "", Effects: Effects(EffectNetworkAccess),
		Handler: func(scope *ToolScope, args json.RawMessage) Result { return Succeeded("ok") },
	}})
	// Global block: deny URL
	netDeny := &testNetwork{allow: false}
	exec := NewExecutor(catalog, nil, netDeny)
	scope := &ToolScope{Role: "lead", CanNetwork: true}

	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "web_fetch", Arguments: `{"url":"https://evil.com"}`})
	if r.Status != StatusDenied {
		t.Fatalf("expected StatusDenied for blocked URL, got %s", r.Status)
	}
}

func TestExecutor_ScopeNetworkPolicyBlocks(t *testing.T) {
	catalog := NewToolCatalog()
	catalog.RegisterAll([]ToolDefinition{{
		Name: "web_fetch", Description: "", Effects: Effects(EffectNetworkAccess),
		Handler: func(scope *ToolScope, args json.RawMessage) Result { return Succeeded("ok") },
	}})
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{Role: "lead", CanNetwork: true, NetworkPolicy: &testNetwork{allow: false}}

	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "web_fetch", Arguments: `{"url":"https://example.com"}`})
	if r.Status != StatusDenied {
		t.Fatalf("expected StatusDenied from scope network policy, got %s", r.Status)
	}
}

// --- Invalid input ---

func TestExecutor_RejectsTruncatedJSON(t *testing.T) {
	catalog := newTestCatalog()
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{Role: "lead", CanRead: true}

	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "read", Arguments: `{"path":`}) // truncated
	if r.Status != StatusInvalidArgs {
		t.Fatalf("expected StatusInvalidArgs, got %s", r.Status)
	}
}

func TestExecutor_ReturnsUnavailableForUnknownTool(t *testing.T) {
	exec := NewExecutor(newTestCatalog(), nil, nil)
	scope := &ToolScope{Role: "lead"}

	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "nonexistent", Arguments: `{}`})
	if r.Status != StatusUnavailable {
		t.Fatalf("expected StatusUnavailable, got %s", r.Status)
	}
}

// --- Panic recovery ---

func TestExecutor_RecoversFromPanic(t *testing.T) {
	catalog := NewToolCatalog()
	catalog.RegisterAll([]ToolDefinition{{
		Name: "crash", Description: "", Effects: Effects(EffectExecuteProcess),
		Handler: func(scope *ToolScope, args json.RawMessage) Result { panic("boom") },
	}})
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{Role: "lead", CanExecute: true}

	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "crash", Arguments: `{}`})
	if r.Status != StatusFailed {
		t.Fatalf("expected StatusFailed after panic, got %s", r.Status)
	}
}

// --- ExecuteAll ---

func TestExecutor_ExecuteAll(t *testing.T) {
	catalog := newTestCatalog()
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{Role: "lead", CanRead: true, CanWrite: true}

	results := exec.ExecuteAll(context.Background(), scope, []llm.ToolCall{
		{Name: "read", Arguments: "{}"},
		{Name: "write", Arguments: "{}"},
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Status != StatusSucceeded || results[1].Status != StatusSucceeded {
		t.Fatalf("expected both Succeeded, got %s / %s", results[0].Status, results[1].Status)
	}
}

// --- ToolCatalog thread safety ---

func TestToolCatalog_ConcurrentAccess(t *testing.T) {
	catalog := NewToolCatalog()
	done := make(chan bool)

	// Register while reading concurrently
	go func() {
		for i := 0; i < 100; i++ {
			catalog.RegisterAll([]ToolDefinition{{
				Name: "t", Description: "", Effects: Effects(),
				Handler: func(scope *ToolScope, args json.RawMessage) Result { return Succeeded("") },
			}})
		}
		done <- true
	}()
	go func() {
		for i := 0; i < 1000; i++ {
			_ = catalog.ToolDefs()
			_ = catalog.LLMToolDefs()
			_, _ = catalog.Resolve("t")
			_ = catalog.IsKnown("t")
		}
		done <- true
	}()
	<-done
	<-done
}

// --- helpers ---

type staticApproval struct {
	allow  bool
	reason string
}

func (a *staticApproval) AllowTool(toolName string, args json.RawMessage) (bool, string) {
	return a.allow, a.reason
}

type testNetwork struct {
	allow bool
}

func (n *testNetwork) AllowURL(url string) bool   { return n.allow }
func (n *testNetwork) AllowHost(host string) bool { return n.allow }
