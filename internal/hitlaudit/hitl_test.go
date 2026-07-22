package hitlaudit

import (
	"context"
	"encoding/json"
	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/tool"
	"os"
	"testing"
)

func init() { config.SetConfig(config.Load()) }

// --- HITLApprovalAdapter ---

func TestHITLApprovalAdapter_AllowsSafeTool(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	adapter := NewHITLApprovalAdapter(mgr)

	allowed, reason := adapter.AllowTool("read_file", json.RawMessage(`{}`))
	if !allowed {
		t.Errorf("expected read_file to be allowed, got: %s", reason)
	}
}

func TestHITLApprovalAdapter_NeedsReviewForDanger(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	adapter := NewHITLApprovalAdapter(mgr)

	allowed, _ := adapter.AllowTool("bash", json.RawMessage(`{"command":"rm -rf /"}`))
	if allowed {
		t.Error("expected bash rm -rf to need review (non-TTY → reject)")
	}
}

func TestHITLApprovalAdapter_AllowsWhenDisabled(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(false)
	adapter := NewHITLApprovalAdapter(mgr)

	allowed, _ := adapter.AllowTool("bash", json.RawMessage(`{}`))
	if !allowed {
		t.Error("expected bash to be allowed when HITL is disabled")
	}
}

func TestHITLApprovalAdapter_NonTTYApproveFallback(t *testing.T) {
	os.Setenv("HITL_NON_TTY_FALLBACK", "approve")
	defer os.Unsetenv("HITL_NON_TTY_FALLBACK")

	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	adapter := NewHITLApprovalAdapter(mgr)

	// With nonTTYFallback=approve, the approve path is hit instead of reject
	allowed, _ := adapter.AllowTool("bash", json.RawMessage(`{"command":"rm -rf /"}`))
	t.Logf("bash with approve fallback: allowed=%v", allowed)
}

// --- NeedsReview ---

func TestNeedsReview_Disabled(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(false)
	needs, _, _ := mgr.NeedsReview("bash", `{"command":"rm -rf /"}`)
	if needs {
		t.Error("NeedsReview should return false when HITL is disabled")
	}
}

func TestNeedsReview_SafeCommand(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	needs, _, _ := mgr.NeedsReview("bash", `{"command":"ls -la"}`)
	if needs {
		t.Error("ls should not need review")
	}
}

func TestNeedsReview_DangerousCommand(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	needs, risk, _ := mgr.NeedsReview("bash", `{"command":"rm -rf /var/tmp"}`)
	if !needs {
		t.Error("rm -rf should need review")
	}
	if risk == "" {
		t.Error("risk level should not be empty")
	}
}

func TestNeedsReview_DeleteFile(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	needs, _, _ := mgr.NeedsReview("delete_file", `{"path":"/etc/passwd"}`)
	if !needs {
		t.Error("delete_file should need review")
	}
}

// --- Toggle ---

func TestHITLManager_Toggle(t *testing.T) {
	mgr := NewHITLManager(nil)
	if mgr.IsEnabled() {
		t.Error("should be disabled by default")
	}
	mgr.SetEnabled(true)
	if !mgr.IsEnabled() {
		t.Error("should be enabled")
	}
	mgr.SetEnabled(false)
	if mgr.IsEnabled() {
		t.Error("should be disabled")
	}
}

// --- Executor integration ---

func TestExecutorIntegration_HITLRejectsDanger(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	adapter := NewHITLApprovalAdapter(mgr)

	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{{
		Name: "delete_file", Description: "", RiskLevel: tool.RiskDanger,
		Effects: tool.Effects(tool.EffectDeleteFile),
		Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
			return tool.Succeeded("deleted") // should never execute
		},
	}})

	exec := tool.NewExecutor(catalog, adapter, nil)
	scope := &tool.ToolScope{Role: "lead", CanWrite: true}
	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "delete_file", Arguments: `{"path":"/secret.txt"}`})

	if r.Status != tool.StatusRejected {
		t.Errorf("expected StatusRejected from HITL, got %s", r.Status)
	}
}

func TestExecutorIntegration_HITLDisabledAllowsDanger(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(false)
	adapter := NewHITLApprovalAdapter(mgr)

	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{{
		Name: "delete_file", Description: "", RiskLevel: tool.RiskDanger,
		Effects: tool.Effects(tool.EffectDeleteFile),
		Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
			return tool.Succeeded("deleted")
		},
	}})

	exec := tool.NewExecutor(catalog, adapter, nil)
	scope := &tool.ToolScope{Role: "lead", CanWrite: true}
	r := exec.Execute(context.Background(), scope, llm.ToolCall{Name: "delete_file", Arguments: `{}`})

	if r.Status != tool.StatusSucceeded {
		t.Errorf("expected StatusSucceeded when HITL disabled, got %s: %s", r.Status, r.Output)
	}
}
