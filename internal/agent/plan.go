// Planning module: think-before-plan gates.
//
// Gates enforce "think -> plan -> act":
//  1. Round 0: if LLM planned without reasoning, inject <think-first>
//  2. Round 0: if LLM thought but didn't plan, inject <planning-required>
//  3. Round 1: if multiple tasks exist with no DAG edges, nudge to add deps
//
// Trivial queries (short single-line) are exempted.
package agent

import (
	"fmt"
	"go-code-agent/infra"
	"strings"
)

// checkPlanningGate enforces think-before-plan on round 0.
func checkPlanningGate(toolRounds int, usedPlanning, usedThink, usedExplore bool, originalTask string) string {
	if toolRounds != 0 {
		return ""
	}
	if isTrivialQuery(originalTask) {
		return ""
	}
	// Jumped straight to planning tools without thinking or exploring.
	if usedPlanning && !usedThink && !usedExplore {
		return App.PromptLoader.Load("think_required")
	}
	// Thought/explored but didn't plan yet.
	if !usedPlanning {
		return App.PromptLoader.Load("planning_required")
	}
	return ""
}

// isTrivialQuery returns true for short single-line questions.
func isTrivialQuery(task string) bool {
	t := strings.TrimSpace(task)
	if t == "" {
		return true
	}
	if len(t) >= infra.PlanningGateMinTaskChars {
		return false
	}
	if strings.Contains(t, "\n") {
		return false
	}
	lower := strings.ToLower(t)
	heavyKeywords := []string{
		"implement", "refactor", "build", "design", "fix",
		"deploy", "migrate", "rewrite", "重构", "实现", "修复", "设计",
	}
	for _, k := range heavyKeywords {
		if strings.Contains(lower, k) {
			return false
		}
	}
	return true
}

// checkDAGDependency nudges the agent to define task dependencies.
func checkDAGDependency(toolRounds int) string {
	if toolRounds != 1 {
		return ""
	}
	if App.DagSched().TaskCount() > 1 && App.DagSched().EdgeCount() == 0 {
		return `<planning-required>You created ` + fmt.Sprintf("%d", App.DagSched().TaskCount()) +
			` tasks but NO dependencies. Before executing any task, you MUST:\n` +
			`1. Think: what is the execution order? Which task must finish before another can start?\n` +
			`2. Call task_add_dep(from, to) to define at least one dependency edge.\n` +
			`3. Call task_dag to review the DAG.\n` +
			`4. Only then start working on ready tasks.\n` +
			`If tasks can run in parallel, that's fine — but you must still call task_dag to confirm.</planning-required>`
	}
	return ""
}
