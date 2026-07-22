package agent

import (
	"context"
	"strings"
	"testing"

	"go-code-agent/internal/worktree"
)

func TestTeammateSpawnFailsClosedWithoutWorktreeService(t *testing.T) {
	tm := NewTeammateManager(t.TempDir(), nil, nil, nil, nil, nil, nil, "")

	got := tm.Spawn(context.Background(), "worker", "coder", "do work")
	if !strings.Contains(got, "worktree service unavailable") {
		t.Fatalf("Spawn result = %q", got)
	}
	if names := tm.MemberNames(); len(names) != 0 {
		t.Fatalf("failed spawn persisted members: %v", names)
	}
}

func TestTeammateSpawnFailsClosedWhenWorktreeAcquireFails(t *testing.T) {
	nonRepo := t.TempDir()
	wt := worktree.New(nonRepo, t.TempDir())
	tm := NewTeammateManager(t.TempDir(), nil, nil, nil, nil, wt, nil, "")

	got := tm.Spawn(context.Background(), "worker", "coder", "do work")
	if !strings.Contains(got, "worktree isolation failed") {
		t.Fatalf("Spawn result = %q", got)
	}
	if names := tm.MemberNames(); len(names) != 0 {
		t.Fatalf("failed spawn persisted members: %v", names)
	}
}
