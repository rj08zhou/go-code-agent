package agent

import (
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/task"
	"strings"
)

// PlanGate enforces think-before-plan discipline.
// It is injected into the runner and evaluated at specific rounds.
type PlanGate struct {
	promptLoader *prompt.Loader
	taskSvc      *task.Service
}

func NewPlanGate(pl *prompt.Loader, ts *task.Service) *PlanGate {
	return &PlanGate{promptLoader: pl, taskSvc: ts}
}

// Eval returns a prompt to inject (or "" if nothing).
// Called by the runner every turn with the latest state snapshot.
func (g *PlanGate) Eval(
	toolRounds int,
	usedPlanning, usedThink, usedExplore bool,
	originalTask string,
) string {
	// --- Phase 1: round-0 gate ---
	result := g.checkPlanningGate(toolRounds, usedPlanning, usedThink, usedExplore, originalTask)
	if result != "" {
		return result
	}

	// --- Phase 2: round-1 DAG nudge ---
	return g.checkDAGDependency(toolRounds)
}

func (g *PlanGate) checkPlanningGate(toolRounds int, usedPlanning, usedThink, usedExplore bool, originalTask string) string {
	if toolRounds != 0 {
		return ""
	}
	if isTrivialQuery(originalTask) {
		return ""
	}
	if usedPlanning && !usedThink && !usedExplore {
		return g.promptLoader.MustLoad("think_required")
	}
	if !usedPlanning {
		return g.promptLoader.MustLoad("planning_required")
	}
	return ""
}

func (g *PlanGate) checkDAGDependency(toolRounds int) string {
	if toolRounds != 1 {
		return ""
	}
	if g.taskSvc == nil {
		return ""
	}
	taskCount := g.taskSvc.TaskCount()
	edgeCount := g.taskSvc.EdgeCount()
	if taskCount > 1 && edgeCount == 0 {
		return prompt.Render(g.promptLoader.MustLoad("dag_required"), map[string]string{
			"count": fmt.Sprintf("%d", taskCount),
		})
	}
	return ""
}

func isTrivialQuery(task string) bool {
	t := strings.TrimSpace(task)
	if t == "" {
		return true
	}
	if len(t) >= config.PlanningGateMinTaskChars {
		return false
	}
	if strings.Contains(t, "\n") {
		return false
	}
	lower := strings.ToLower(t)
	for _, k := range []string{
		"implement", "refactor", "build", "design", "fix",
		"deploy", "migrate", "rewrite", "重构", "实现", "修复", "设计",
	} {
		if strings.Contains(lower, k) {
			return false
		}
	}
	return true
}
