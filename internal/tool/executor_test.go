package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
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

func TestExecutor_NetworkPolicyChecksNestedDestinations(t *testing.T) {
	catalog := NewToolCatalog()
	catalog.RegisterAll([]ToolDefinition{{
		Name: "mcp__demo__fetch", Description: "", Effects: Effects(EffectNetworkAccess),
		Handler: func(scope *ToolScope, args json.RawMessage) Result { return Succeeded("ok") },
	}})
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{Role: "lead", CanNetwork: true, NetworkPolicy: &testNetwork{allow: false}}

	result := exec.Execute(
		context.Background(),
		scope,
		llm.ToolCall{
			Name:      "mcp__demo__fetch",
			Arguments: `{"request":{"endpoint":"https://example.com/api"}}`,
		},
	)
	if result.Status != StatusDenied {
		t.Fatalf("status = %s, want denied for nested endpoint", result.Status)
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

func TestExecutor_MCPUnclassifiedRequiresApprovalPolicy(t *testing.T) {
	catalog := NewToolCatalog()
	called := false
	catalog.RegisterAll([]ToolDefinition{{
		Name:    "mcp__demo__unknown",
		Effects: Effects(EffectNetworkAccess, EffectUnclassified),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			called = true
			return Succeeded("should not run")
		},
	}})
	exec := NewExecutor(catalog, nil, nil)

	result := exec.Execute(
		context.Background(),
		&ToolScope{Role: "lead", CanNetwork: true},
		llm.ToolCall{Name: "mcp__demo__unknown", Arguments: `{}`},
	)
	if result.Status != StatusDenied {
		t.Fatalf("status = %s, want denied", result.Status)
	}
	if called {
		t.Fatal("unclassified MCP handler ran without an approval policy")
	}
}

func TestExecutorRejectsMissingScopeWithoutPanic(t *testing.T) {
	exec := NewExecutor(newTestCatalog(), nil, nil)
	result := exec.Execute(context.Background(), nil, llm.ToolCall{Name: "read", Arguments: `{}`})
	if result.Status != StatusDenied {
		t.Fatalf("status = %s, want denied", result.Status)
	}
	if !strings.Contains(result.Output, "requires an execution scope") {
		t.Fatalf("denial output = %q", result.Output)
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

func TestExecutorSanitizesFailureAndPanicOutputs(t *testing.T) {
	catalog := NewToolCatalog()
	catalog.RegisterAll([]ToolDefinition{
		{
			Name: "failure",
			Handler: func(*ToolScope, json.RawMessage) Result {
				return Failed("request failed with secret-token")
			},
		},
		{
			Name: "panic",
			Handler: func(*ToolScope, json.RawMessage) Result {
				panic("secret-token")
			},
		},
	})
	executor := NewExecutor(catalog, nil, nil).WithSanitizer(maskSecretSanitizer{})
	for _, name := range []string{"failure", "panic"} {
		result := executor.Execute(context.Background(), &ToolScope{}, llm.ToolCall{Name: name, Arguments: `{}`})
		if result.Status != StatusFailed {
			t.Fatalf("%s status = %s, want failed", name, result.Status)
		}
		if strings.Contains(result.Output, "secret-token") || !strings.Contains(result.Output, "[REDACTED]") {
			t.Fatalf("%s output was not sanitized: %q", name, result.Output)
		}
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

func TestToolCatalogUnregisterPrefixPreservesOtherToolsAndOrder(t *testing.T) {
	catalog := NewToolCatalog()
	noop := func(scope *ToolScope, args json.RawMessage) Result { return Succeeded("ok") }
	catalog.RegisterAll([]ToolDefinition{
		{Name: "builtin", Handler: noop},
		{Name: "mcp__alpha__one", Handler: noop},
		{Name: "mcp__beta__one", Handler: noop},
		{Name: "mcp__alpha__two", Handler: noop},
	})

	if removed := catalog.UnregisterPrefix("mcp__alpha__"); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if catalog.IsKnown("mcp__alpha__one") || catalog.IsKnown("mcp__alpha__two") {
		t.Fatal("matching tools remained registered")
	}
	if !catalog.IsKnown("builtin") || !catalog.IsKnown("mcp__beta__one") {
		t.Fatal("unrelated tools were removed")
	}
	defs := catalog.ToolDefs()
	if len(defs) != 2 || defs[0]["name"] != "builtin" || defs[1]["name"] != "mcp__beta__one" {
		t.Fatalf("remaining tool order = %#v", defs)
	}
}

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

type maskSecretSanitizer struct{}

func (maskSecretSanitizer) Sanitize(value string) string {
	return strings.ReplaceAll(value, "secret-token", "[REDACTED]")
}
