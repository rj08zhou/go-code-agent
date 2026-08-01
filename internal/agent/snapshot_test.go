package agent

import (
	"os/exec"
	"strings"
	"testing"

	"go-code-agent/internal/tool"
)

func TestSnapshotCreationFailureAddsProtectionNotice(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	dir := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if _, err := takeSnapshot(dir); err == nil {
		t.Fatal("empty repository unexpectedly produced a snapshot")
	}

	runs := 0
	result := NewSnapshotManager(true, dir).WithSnapshot("write_file", func() tool.Result {
		runs++
		return tool.Succeeded("Wrote file")
	})

	if runs != 1 {
		t.Fatalf("tool runs = %d, want 1", runs)
	}
	if !result.Succeeded() {
		t.Fatalf("tool status changed to %s", result.Status)
	}
	if !strings.Contains(result.Output, "[snapshot] unavailable; tool ran without rollback protection") {
		t.Fatalf("tool output missing snapshot warning: %q", result.Output)
	}
	if strings.Contains(result.Output, "git stash") {
		t.Fatalf("internal git error leaked into tool output: %q", result.Output)
	}
}
