package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-code-agent/internal/tool"
	"go-code-agent/internal/worktree"
)

func TestTeammateSpawnFailsClosedWithoutWorktreeService(t *testing.T) {
	tm := NewTeammateManager(t.TempDir(), nil, nil, nil, nil, nil, nil, "", nil)

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
	tm := NewTeammateManager(t.TempDir(), nil, nil, nil, nil, wt, nil, "", nil)

	got := tm.Spawn(context.Background(), "worker", "coder", "do work")
	if !strings.Contains(got, "worktree isolation failed") {
		t.Fatalf("Spawn result = %q", got)
	}
	if names := tm.MemberNames(); len(names) != 0 {
		t.Fatalf("failed spawn persisted members: %v", names)
	}
}

func TestExploreToolNamesDropBashWithoutApproval(t *testing.T) {
	hasBash := func(names []string) bool {
		for _, n := range names {
			if n == "bash" {
				return true
			}
		}
		return false
	}
	if !hasBash(exploreToolNames("explore", true)) {
		t.Fatal("explore with approval should keep bash (HITL-gated)")
	}
	if hasBash(exploreToolNames("explore", false)) {
		t.Fatal("explore without approval must fail closed and drop bash")
	}
}

func TestTeammatePlanApprovalUsesToolEffects(t *testing.T) {
	noop := func(*tool.ToolScope, json.RawMessage) tool.Result { return tool.Succeeded("ok") }
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{Name: "read_file", Effects: tool.Effects(tool.EffectReadFile), Handler: noop},
		{Name: "insert_file", Effects: tool.Effects(tool.EffectWriteFile), Handler: noop},
		{Name: "background_run", Effects: tool.Effects(tool.EffectExecuteProcess), Handler: noop},
		{Name: "dynamic", Handler: noop},
	})
	executor := tool.NewExecutor(catalog, nil, nil)

	if requiresApprovedPlan(executor, "read_file") {
		t.Fatal("read-only tool should not require teammate plan approval")
	}
	for _, name := range []string{"insert_file", "background_run", "dynamic", "missing"} {
		if !requiresApprovedPlan(executor, name) {
			t.Errorf("%s should require teammate plan approval", name)
		}
	}
}
