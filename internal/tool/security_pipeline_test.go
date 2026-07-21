package tool

import (
	"context"
	"encoding/json"
	"go-code-agent-refactor/internal/llm"
	"os"
	"path/filepath"
	"testing"
)

type previewApproval struct {
	decision ApprovalDecision
	seen     string
}

func (a *previewApproval) AllowTool(string, json.RawMessage) (bool, string) { return true, "" }
func (a *previewApproval) DecideTool(_ string, _ json.RawMessage, preview string) (ApprovalDecision, string, string) {
	a.seen = preview
	return a.decision, "rejected by test", "modify requested"
}

func TestExecutor_DiffPreviewBeforeMutationRejects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	approval := &previewApproval{decision: ApprovalRejected}
	catalog := NewToolCatalog()
	catalog.RegisterAll([]ToolDefinition{{Name: "mutate", Effects: Effects(EffectWriteFile), Preview: func(_ *ToolScope, _ json.RawMessage) (PreviewRequest, error) {
		return PreviewRequest{Path: "file.txt", Content: []byte("after\n")}, nil
	}, Handler: func(scope *ToolScope, _ json.RawMessage) Result {
		_ = os.WriteFile(filepath.Join(scope.Workdir, "file.txt"), []byte("after\n"), 0o644)
		return Succeeded("mutated")
	}}})
	exec := NewExecutor(catalog, approval, nil)
	result := exec.Execute(context.Background(), &ToolScope{Workdir: dir, Role: "lead", CanWrite: true, DiffPreview: &testDiff{dir: dir}}, llm.ToolCall{Name: "mutate", Arguments: `{}`})
	if result.Status != StatusRejected {
		t.Fatalf("status=%s, want rejected", result.Status)
	}
	if approval.seen == "" {
		t.Fatal("approval did not receive a diff preview")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "before\n" {
		t.Fatalf("mutation occurred before approval: %q", data)
	}
}

func TestExecutor_DiffPreviewModifyDoesNotMutate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	_ = os.WriteFile(path, []byte("before\n"), 0o644)
	approval := &previewApproval{decision: ApprovalModified}
	catalog := NewToolCatalog()
	catalog.RegisterAll([]ToolDefinition{{Name: "mutate", Effects: Effects(EffectWriteFile), Preview: func(_ *ToolScope, _ json.RawMessage) (PreviewRequest, error) {
		return PreviewRequest{Path: "file.txt", Content: []byte("after\n")}, nil
	}, Handler: func(scope *ToolScope, _ json.RawMessage) Result {
		_ = os.WriteFile(filepath.Join(scope.Workdir, "file.txt"), []byte("after\n"), 0o644)
		return Succeeded("mutated")
	}}})
	exec := NewExecutor(catalog, approval, nil)
	result := exec.Execute(context.Background(), &ToolScope{Workdir: dir, CanWrite: true, DiffPreview: &testDiff{dir: dir}}, llm.ToolCall{Name: "mutate", Arguments: `{}`})
	if result.Status != StatusModified {
		t.Fatalf("status=%s, want modified", result.Status)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "before\n" {
		t.Fatalf("mutation occurred after modify decision: %q", data)
	}
}

type testDiff struct{ dir string }

func (d *testDiff) Preview(path string, content []byte) (string, error) {
	return "--- before\n+++ after\n", nil
}
func (d *testDiff) PreviewDelete(path string) (string, error) {
	return "--- before\n+++ /dev/null\n", nil
}
