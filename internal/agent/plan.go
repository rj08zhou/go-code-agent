package agent

import (
	"fmt"
	"go-code-agent-refactor/internal/config"
	"go-code-agent-refactor/internal/prompt"
	"go-code-agent-refactor/internal/task"
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
		tmpl := g.promptLoader.Load("think_required")
		if tmpl == "" {
			return "<think-first>Before jumping into tools, think about what needs to be done and make a plan.</think-first>"
		}
		return tmpl
	}
	if !usedPlanning {
		tmpl := g.promptLoader.Load("planning_required")
		if tmpl == "" {
			return "<planning-required>You've started exploring but haven't created a plan yet. Use task_create to break down the work into tasks.</planning-required>"
		}
		return tmpl
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
		return fmt.Sprintf(
			`<planning-required>You created %d tasks but NO dependencies. Before executing any task, you MUST:
1. Think: what is the execution order? Which task must finish before another can start?
2. Call task_add_dep(from, to) to define at least one dependency edge.
3. Call task_dag to review the DAG.
4. Only then start working on ready tasks.
If tasks can run in parallel, that's fine — but you must still call task_dag to confirm.</planning-required>`,
			taskCount)
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
