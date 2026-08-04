package hitlaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/security"
	"go-code-agent/internal/tool"
)

// --- HITLApprovalAdapter ---

func TestInteractiveDecisionPreservesNaturalLanguageFeedback(t *testing.T) {
	calls := setReviewAnswers(t, "m", "please use tabs")
	response := readInteractiveDecision()
	if response.Decision != HITLModify {
		t.Fatalf("decision = %v, want modify", response.Decision)
	}
	if response.Feedback != "please use tabs" {
		t.Fatalf("feedback = %q, want complete natural-language input", response.Feedback)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("input calls = %d, want choice and feedback", got)
	}
}

func TestInteractiveDecisionRejectsWhenFeedbackInputCloses(t *testing.T) {
	setReviewAnswers(t, "m")
	response := readInteractiveDecision()
	if response.Decision != HITLReject {
		t.Fatalf("decision = %v, want fail-closed rejection", response.Decision)
	}
}

func TestInteractiveDecisionTrimsChoiceWhitespace(t *testing.T) {
	setReviewAnswers(t, "  y   ")
	response := readInteractiveDecision()
	if response.Decision != HITLApprove {
		t.Fatalf("decision = %v, want approval", response.Decision)
	}
}

func TestInteractiveDecisionIgnoresBlankAndInvalidChoices(t *testing.T) {
	calls := setReviewAnswers(t, "", "please apply this", "y")
	response := readInteractiveDecision()
	if response.Decision != HITLApprove {
		t.Fatalf("decision = %v, want approval after ignored invalid input", response.Decision)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("input calls = %d, want blank + invalid + approve", got)
	}
}

func TestApprovalDetailsShowsShellCommandWithoutJSONEscaping(t *testing.T) {
	command := `grep -rn "len(.*line\|len(.*input" . 2>/dev/null | head -20`
	arguments, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}

	label, details := approvalDetails(HITLRequest{ToolName: "bash", Arguments: string(arguments)})
	if label != "Command" {
		t.Fatalf("label = %q, want Command", label)
	}
	if details != command {
		t.Fatalf("command details = %q, want original %q", details, command)
	}
}

func TestApprovalDetailsRemovesTerminalControlCharacters(t *testing.T) {
	arguments, err := json.Marshal(map[string]string{"command": "echo first\n\x1b[31msecond"})
	if err != nil {
		t.Fatal(err)
	}

	_, details := approvalDetails(HITLRequest{ToolName: "bash", Arguments: string(arguments)})
	if strings.ContainsAny(details, "\n\r\x1b") {
		t.Fatalf("command details contain terminal control characters: %q", details)
	}
}

func TestApprovalDetailsKeepsBackgroundRunTimeout(t *testing.T) {
	label, details := approvalDetails(HITLRequest{
		ToolName:  "background_run",
		Arguments: `{"command":"sleep 10","timeout":30}`,
	})
	if label != "Arguments" || !strings.Contains(details, `"timeout": 30`) {
		t.Fatalf("background details = (%q, %q), want complete arguments", label, details)
	}
}

func TestHITLApprovalAdapter_AllowsSafeTool(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	adapter := NewHITLApprovalAdapter(mgr)

	allowed, reason := adapter.AllowTool("read_file", json.RawMessage(`{}`))
	if !allowed {
		t.Errorf("expected read_file to be allowed, got: %s", reason)
	}
}

func TestHITLApprovalAdapter_UsesDefinitionRiskForMCP(t *testing.T) {
	t.Setenv("HITL_NON_TTY_FALLBACK", "")
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	mgr.SetMode(HITLModeSafeOnly)
	adapter := NewHITLApprovalAdapter(mgr)

	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{{
		Name:      "mcp__demo__delete_user",
		RiskLevel: tool.RiskDanger,
		Effects:   tool.Effects(tool.EffectNetworkAccess),
		Handler: func(scope *tool.ToolScope, args json.RawMessage) tool.Result {
			return tool.Succeeded("deleted")
		},
	}})
	adapter.SetCatalog(catalog)

	allowed, reason := adapter.AllowTool("mcp__demo__delete_user", json.RawMessage(`{"id":"42"}`))
	if allowed {
		t.Fatalf("dangerous MCP tool bypassed HITL: %q", reason)
	}
}

func TestHITLApprovalAdapter_EnforcesMCPPermissionRules(t *testing.T) {
	dir := t.TempDir()
	permissionsPath := filepath.Join(dir, "permissions.json")
	if err := os.WriteFile(permissionsPath, []byte(`[
		{"tool":"mcp__demo__write","level":"block"},
		{"tool":"mcp__demo__read","level":"confirm"}
	]`), 0o644); err != nil {
		t.Fatal(err)
	}
	permissions := security.NewPermissions()
	if err := permissions.Load(dir); err != nil {
		t.Fatal(err)
	}

	blockedMgr := NewHITLManager(nil)
	blockedAdapter := NewHITLApprovalAdapter(blockedMgr)
	blockedAdapter.SetPermissions(permissions)
	if allowed, reason := blockedAdapter.AllowTool("mcp__demo__write", json.RawMessage(`{}`)); allowed {
		t.Fatalf("blocked MCP tool was allowed: %q", reason)
	}

	confirmMgr := NewHITLManager(nil)
	confirmMgr.SetEnabled(true)
	confirmMgr.SetMode(HITLModeAutoReject)
	confirmAdapter := NewHITLApprovalAdapter(confirmMgr)
	confirmAdapter.SetPermissions(permissions)
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{{
		Name:      "mcp__demo__read",
		RiskLevel: tool.RiskAuto,
		Effects:   tool.Effects(tool.EffectNetworkAccess),
		Handler:   func(scope *tool.ToolScope, args json.RawMessage) tool.Result { return tool.Succeeded("read") },
	}})
	confirmAdapter.SetCatalog(catalog)
	if allowed, reason := confirmAdapter.AllowTool("mcp__demo__read", json.RawMessage(`{}`)); allowed {
		t.Fatalf("confirm MCP tool was auto-allowed: %q", reason)
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

func TestNonInteractiveModesDoNotOpenDiffReview(t *testing.T) {
	cases := []struct {
		name string
		mode HITLMode
		want tool.ApprovalDecision
	}{
		{name: "reject", mode: HITLModeAutoReject, want: tool.ApprovalRejected},
		{name: "notify-only", mode: HITLModeNotifyOnly, want: tool.ApprovalAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := NewHITLManager(nil)
			mgr.SetEnabled(true)
			mgr.SetMode(tc.mode)
			adapter := NewHITLApprovalAdapter(mgr)

			result := adapter.DecideTool(
				"write_file",
				json.RawMessage(`{"path":"ordinary.txt","content":"new"}`),
				tool.ApprovalPreview{
					Text: "diff preview",
					Mutation: &tool.PreviewRequest{
						Path: "ordinary.txt", Content: []byte("new"),
					},
				},
			)
			if result.Decision != tc.want {
				t.Fatalf("decision = %v, want %v", result.Decision, tc.want)
			}
		})
	}
}

func TestFileReviewUsesWholeOperationForCreateAndDelete(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		calls := setReviewAnswers(t, "a")
		adapter := newInteractiveApprovalAdapter()
		content := "first\nsecond\n"
		result := adapter.DecideTool(
			"write_file",
			json.RawMessage(`{"path":"new.txt","content":"first\nsecond\n"}`),
			tool.ApprovalPreview{
				Text: "--- original/new.txt\n+++ modified/new.txt\n@@ -0,0 +1 @@\n+first\n@@ -0,0 +2 @@\n+second\n",
				Mutation: &tool.PreviewRequest{
					Path: "new.txt", Content: []byte(content), Existed: false,
				},
			},
		)
		if result.Decision != tool.ApprovalAllowed || result.ReplacementContent == nil ||
			*result.ReplacementContent != content {
			t.Fatalf("create result = %#v", result)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("create prompts = %d, want one whole-file prompt", got)
		}
	})

	t.Run("delete", func(t *testing.T) {
		calls := setReviewAnswers(t, "d")
		adapter := newInteractiveApprovalAdapter()
		result := adapter.DecideTool(
			"delete_file",
			json.RawMessage(`{"path":"old.txt"}`),
			tool.ApprovalPreview{
				Text: "--- original/old.txt\n+++ modified/old.txt\n@@ -1 +0,0 @@\n-old\n",
				Mutation: &tool.PreviewRequest{
					Path: "old.txt", OriginalContent: []byte("old\n"), Existed: true, Delete: true,
				},
			},
		)
		if result.Decision != tool.ApprovalAllowed || result.ReplacementContent != nil {
			t.Fatalf("delete result = %#v", result)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("delete prompts = %d, want one whole-file prompt", got)
		}
	})

	for _, tc := range []struct {
		name     string
		toolName string
		answer   string
		request  tool.PreviewRequest
	}{
		{
			name: "empty create", toolName: "write_file", answer: "a",
			request: tool.PreviewRequest{Path: "empty.txt"},
		},
		{
			name: "empty delete", toolName: "delete_file", answer: "d",
			request: tool.PreviewRequest{Path: "empty.txt", Existed: true, Delete: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := setReviewAnswers(t, tc.answer)
			result := newInteractiveApprovalAdapter().DecideTool(
				tc.toolName,
				json.RawMessage(`{"path":"empty.txt","content":""}`),
				tool.ApprovalPreview{Mutation: &tc.request},
			)
			if result.Decision != tool.ApprovalAllowed {
				t.Fatalf("empty-file result = %#v", result)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("empty-file prompts = %d, want one whole-file prompt", got)
			}
		})
	}
}

func TestExistingFileReviewSupportsPartialAndAllApproval(t *testing.T) {
	original := strings.Join([]string{
		"old top", "keep 01", "keep 02", "keep 03", "keep 04",
		"keep 05", "keep 06", "keep 07", "keep 08", "keep 09",
		"keep 10", "keep 11", "keep 12", "keep 13", "keep 14",
		"keep 15", "keep 16", "keep 17", "keep 18", "old bottom",
	}, "\n") + "\n"
	proposed := strings.Replace(original, "old top", "new top", 1)
	proposed = strings.Replace(proposed, "old bottom", "new bottom", 1)
	diff := existingFileDiff(t, original, proposed)

	t.Run("partial", func(t *testing.T) {
		calls := setReviewAnswers(t, "a", "r")
		adapter := newInteractiveApprovalAdapter()
		result := adapter.DecideTool(
			"edit_file",
			json.RawMessage(`{"path":"file.txt","old_text":"old","new_text":"new","replace_all":true}`),
			tool.ApprovalPreview{
				Text: diff,
				Mutation: &tool.PreviewRequest{
					Path:            "file.txt",
					OriginalContent: []byte(original),
					Content:         []byte(proposed),
					Existed:         true,
				},
			},
		)
		want := strings.Replace(original, "old top", "new top", 1)
		if result.Decision != tool.ApprovalAllowed || result.ReplacementContent == nil ||
			*result.ReplacementContent != want {
			t.Fatalf("partial result = %#v\nwant content: %q", result, want)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("partial prompts = %d, want one per hunk", got)
		}
	})

	t.Run("all", func(t *testing.T) {
		calls := setReviewAnswers(t, "l")
		adapter := newInteractiveApprovalAdapter()
		result := adapter.DecideTool(
			"insert_file",
			json.RawMessage(`{"path":"file.txt","insert_at":1,"content":"unused"}`),
			tool.ApprovalPreview{
				Text: diff,
				Mutation: &tool.PreviewRequest{
					Path:            "file.txt",
					OriginalContent: []byte(original),
					Content:         []byte(proposed),
					Existed:         true,
				},
			},
		)
		if result.Decision != tool.ApprovalAllowed || result.ReplacementContent == nil ||
			*result.ReplacementContent != proposed {
			t.Fatalf("all result = %#v", result)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("all prompts = %d, want one apply-all selection", got)
		}
	})
}

func TestConcurrentFileReviewsAreSerializedAndKeepOwnContent(t *testing.T) {
	adapter := newInteractiveApprovalAdapter()
	var active atomic.Int32
	var maximum atomic.Int32
	security.SetReadLine(func(string) (string, error) {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return "a", nil
	})
	t.Cleanup(resetReviewInput)

	const workers = 6
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			content := fmt.Sprintf("content-%d", index)
			result := adapter.DecideTool(
				"write_file",
				json.RawMessage(fmt.Sprintf(`{"path":"file-%d.txt","content":%q}`, index, content)),
				tool.ApprovalPreview{
					Text: fmt.Sprintf("--- original/file-%d.txt\n+++ modified/file-%d.txt\n@@ -0,0 +1 @@\n+%s\n", index, index, content),
					Mutation: &tool.PreviewRequest{
						Path: fmt.Sprintf("file-%d.txt", index), Content: []byte(content),
					},
				},
			)
			if result.Decision != tool.ApprovalAllowed || result.ReplacementContent == nil ||
				*result.ReplacementContent != content {
				results <- fmt.Errorf("review %d returned %#v", index, result)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		t.Error(err)
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("concurrent terminal prompts = %d, want 1", got)
	}
}

func newInteractiveApprovalAdapter() *HITLApprovalAdapter {
	manager := NewHITLManager(nil)
	manager.SetEnabled(true)
	manager.SetMode(HITLModeInteractive)
	return NewHITLApprovalAdapter(manager)
}

func setReviewAnswers(t *testing.T, answers ...string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	security.SetReadLine(func(string) (string, error) {
		index := int(calls.Add(1)) - 1
		if index >= len(answers) {
			return "", io.EOF
		}
		return answers[index], nil
	})
	t.Cleanup(resetReviewInput)
	return &calls
}

func resetReviewInput() {
	security.SetReadLine(func(string) (string, error) { return "", io.EOF })
}

func existingFileDiff(t *testing.T, original, proposed string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := security.NewDiffPreview(dir).Preview("file.txt", []byte(proposed))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(diff, "@@") < 4 {
		t.Fatalf("test diff does not contain two hunks: %q", diff)
	}
	return diff
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
