package task

import (
	"strings"
	"testing"
)

func TestTodoManagerRenderAndProgress(t *testing.T) {
	mgr := &TodoManager{}
	output, err := mgr.Update([]map[string]string{
		{"content": "create file", "status": "completed", "activeForm": "creating file"},
		{"content": "edit file", "status": "in_progress", "activeForm": "editing file"},
		{"content": "delete file", "status": "pending", "activeForm": "deleting file"},
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if output != mgr.Render() {
		t.Fatalf("Update() output differs from Render()")
	}
	if !mgr.HasOpenItems() {
		t.Fatal("expected open todo items")
	}
	for _, want := range []string{"[x] create file", "[>] edit file <- editing file", "[ ] delete file", "(1/3 completed)"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q does not contain %q", output, want)
		}
	}
}

func TestTodoManagerRejectsMultipleInProgress(t *testing.T) {
	mgr := &TodoManager{}
	if _, err := mgr.Update([]map[string]string{
		{"content": "one", "status": "in_progress", "activeForm": "doing one"},
		{"content": "two", "status": "in_progress", "activeForm": "doing two"},
	}); err == nil {
		t.Fatal("expected multiple in_progress error")
	}
}
