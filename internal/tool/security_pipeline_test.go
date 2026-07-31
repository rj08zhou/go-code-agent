package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
)

type previewApproval struct {
	decision    ApprovalDecision
	seen        ApprovalPreview
	replacement *string
	onDecide    func(ApprovalPreview)
}

func (a *previewApproval) AllowTool(string, json.RawMessage) (bool, string) { return true, "" }
func (a *previewApproval) DecideTool(_ string, _ json.RawMessage, preview ApprovalPreview) ApprovalResult {
	a.seen = preview
	if a.onDecide != nil {
		a.onDecide(preview)
	}
	return ApprovalResult{
		Decision:           a.decision,
		Reason:             "rejected by test",
		Feedback:           "modify requested",
		ReplacementContent: a.replacement,
	}
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
	if approval.seen.Text == "" || approval.seen.Mutation == nil {
		t.Fatal("approval did not receive a structured diff preview")
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

func TestExecutorAppliesApprovedFullContentForExistingFileMutations(t *testing.T) {
	tests := []struct {
		name string
		call llm.ToolCall
	}{
		{
			name: "write",
			call: llm.ToolCall{Name: "write_file", Arguments: `{"path":"file.txt","content":"proposed write"}`},
		},
		{
			name: "edit",
			call: llm.ToolCall{Name: "edit_file", Arguments: `{"path":"file.txt","old_text":"before","new_text":"proposed edit"}`},
		},
		{
			name: "insert",
			call: llm.ToolCall{Name: "insert_file", Arguments: `{"path":"file.txt","insert_at":2,"content":"proposed insert"}`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			original := "before\nunchanged\n"
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			approved := "approved first block\nunchanged\n"
			approval := &previewApproval{decision: ApprovalAllowed, replacement: &approved}
			executor := newFilesystemTestExecutor(approval)

			result := executor.Execute(context.Background(), filesystemTestScope(dir), tc.call)
			if !result.Succeeded() {
				t.Fatalf("status=%s output=%s", result.Status, result.Output)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != approved {
				t.Fatalf("file content = %q, want approved content %q", got, approved)
			}
			if approval.seen.Mutation == nil || !approval.seen.Mutation.Existed ||
				string(approval.seen.Mutation.OriginalContent) != original {
				t.Fatalf("approval preview = %#v", approval.seen.Mutation)
			}
		})
	}
}

func TestExecutorAppliesApprovedEmptyFileContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := ""
	executor := newFilesystemTestExecutor(&previewApproval{
		decision: ApprovalAllowed, replacement: &empty,
	})

	result := executor.Execute(
		context.Background(),
		filesystemTestScope(dir),
		llm.ToolCall{Name: "edit_file", Arguments: `{"path":"file.txt","old_text":"before","new_text":"proposed"}`},
	)
	if !result.Succeeded() {
		t.Fatalf("status=%s output=%s", result.Status, result.Output)
	}
	got, err := os.ReadFile(path)
	if err != nil || len(got) != 0 {
		t.Fatalf("approved empty content was not applied: %q err=%v", got, err)
	}
}

func TestExecutorUsesWholePreviewForNewAndDeletedFiles(t *testing.T) {
	t.Run("new file", func(t *testing.T) {
		dir := t.TempDir()
		approval := &previewApproval{decision: ApprovalAllowed}
		executor := newFilesystemTestExecutor(approval)
		result := executor.Execute(
			context.Background(),
			filesystemTestScope(dir),
			llm.ToolCall{Name: "write_file", Arguments: `{"path":"new.txt","content":"new content"}`},
		)
		if !result.Succeeded() {
			t.Fatalf("status=%s output=%s", result.Status, result.Output)
		}
		if approval.seen.Mutation == nil || approval.seen.Mutation.Existed || approval.seen.Mutation.Delete {
			t.Fatalf("new file preview = %#v", approval.seen.Mutation)
		}
		got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
		if err != nil || string(got) != "new content" {
			t.Fatalf("new file = %q err=%v", got, err)
		}
	})

	t.Run("delete file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "delete.txt")
		if err := os.WriteFile(path, []byte("delete me"), 0o644); err != nil {
			t.Fatal(err)
		}
		approval := &previewApproval{decision: ApprovalAllowed}
		executor := newFilesystemTestExecutor(approval)
		result := executor.Execute(
			context.Background(),
			filesystemTestScope(dir),
			llm.ToolCall{Name: "delete_file", Arguments: `{"path":"delete.txt"}`},
		)
		if !result.Succeeded() {
			t.Fatalf("status=%s output=%s", result.Status, result.Output)
		}
		if approval.seen.Mutation == nil || !approval.seen.Mutation.Existed || !approval.seen.Mutation.Delete {
			t.Fatalf("delete preview = %#v", approval.seen.Mutation)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("file still exists after approved deletion: %v", err)
		}
	})
}

func TestExecutorRejectsMutationWhenFileChangesAfterPreview(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	approval := &previewApproval{decision: ApprovalAllowed}
	approval.onDecide = func(ApprovalPreview) {
		if err := os.WriteFile(path, []byte("concurrent update"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	executor := newFilesystemTestExecutor(approval)

	result := executor.Execute(
		context.Background(),
		filesystemTestScope(dir),
		llm.ToolCall{Name: "edit_file", Arguments: `{"path":"file.txt","old_text":"before","new_text":"proposed"}`},
	)
	if result.Status != StatusFailed || !strings.Contains(result.Output, "changed since preview") {
		t.Fatalf("status=%s output=%q", result.Status, result.Output)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "concurrent update" {
		t.Fatalf("concurrent content was overwritten: %q err=%v", got, err)
	}
}

func newFilesystemTestExecutor(approval ApprovalChecker) *Executor {
	catalog := NewToolCatalog()
	catalog.RegisterAll(filesystemWriteTools(builtinDeps{}))
	return NewExecutor(catalog, approval, nil)
}

func filesystemTestScope(dir string) *ToolScope {
	return &ToolScope{
		Workdir:     dir,
		Role:        "lead",
		CanWrite:    true,
		DiffPreview: &testDiff{dir: dir},
	}
}

type testDiff struct{ dir string }

func (d *testDiff) PreviewChange(path string, oldContent, newContent []byte) (string, error) {
	return "--- before\n+++ after\n", nil
}
