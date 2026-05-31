// Reflection module: self-evaluation triggers.
//
// Triggers:
//  1. consecutiveFailures == 1 -> mini-reflect
//  2. consecutiveFailures >= max -> force strategy change
//  3. roundsSinceLastComplete >= stuckThreshold -> stuck detection
//  4. toolRounds % reflectInterval == 0 -> periodic reflection
//  5. roundsWithoutTodo >= 3 && hasOpenItems -> task nag
//
// Pure function: returns prompts + reset flags without side effects.
package main

import (
	"fmt"
	"strings"
)

// reflect evaluates agent state and returns reflection prompts to inject.
func reflect(
	consecutiveFailures int, lastFailedTool string, maxConsecutiveFailures int,
	toolRounds int, totalFailures int,
	roundsSinceLastComplete int, roundsWithoutTodo int,
	stuckThreshold int, reflectInterval int,
	hasOpenItems bool,
) (prompts []string, resetFailures, resetTodoNag, resetStuck bool) {

	// 1) Immediate mini-reflect on first failure.
	if consecutiveFailures == 1 {
		prompts = append(prompts, fmt.Sprintf(
			"<mini-reflect>Tool '%s' failed. Before retrying, consider: "+
				"Is the approach correct? Are the arguments right? "+
				"Would a different tool or method work better?</mini-reflect>",
			lastFailedTool))
	}

	// 2) Force strategy change after repeated failures.
	if consecutiveFailures >= maxConsecutiveFailures {
		// Fallback ensures reflection still works if the prompt file
		// is missing — silent empty injection here would defeat the
		// whole point of the strategy-change gate.
		tmpl := app.PromptLoader.LoadOr("strategy_change",
			"<strategy-change>Tool '{{tool}}' has failed {{count}} times in a row. "+
				"STOP retrying the same approach. Re-read the error, then either: "+
				"(a) try a different tool, (b) gather more context first, or "+
				"(c) ask the user for clarification.</strategy-change>")
		msg := strings.Replace(strings.Replace(tmpl,
			"{{tool}}", lastFailedTool, 1),
			"{{count}}", fmt.Sprintf("%d", consecutiveFailures), 1)
		prompts = append(prompts, msg)
		resetFailures = true
	}

	// 3) Stuck detection: too many rounds without completing any DAG task.
	if roundsSinceLastComplete >= stuckThreshold && app.DagSched().TaskCount() > 0 {
		prompts = append(prompts, fmt.Sprintf(
			"<stuck>You have spent %d rounds without completing a task. "+
				"Step back and reconsider your approach. Use task_dag to review the plan. "+
				"Consider: Is the current task too large? Should you break it down further? "+
				"Is there a blocker you can work around?</stuck>",
			roundsSinceLastComplete))
		resetStuck = true
	}

	// 4) Periodic reflection / 5) task nag.
	needReflect := toolRounds%reflectInterval == 0
	needNag := hasOpenItems && roundsWithoutTodo >= 3

	if needReflect {
		// Build context-aware reflection with actual data.
		var rb strings.Builder
		rb.WriteString("<reflect>Pause and evaluate (round ")
		rb.WriteString(fmt.Sprintf("%d", toolRounds))
		rb.WriteString("):\n")
		if totalFailures > 0 {
			rb.WriteString(fmt.Sprintf("- %d tool failures so far this session.\n", totalFailures))
		}
		if ps := app.DagSched().ProgressSummary(); ps != "" {
			rb.WriteString(fmt.Sprintf("- %s\n", ps))
		}
		rb.WriteString("1. Are your actions achieving the intended goal?\n")
		rb.WriteString("2. Did any tool call fail? If so, change strategy.\n")
		rb.WriteString("3. What should you do next? Is there a more efficient approach?\n")
		if needNag {
			rb.WriteString("4. You have open todos - update them now.\n")
			resetTodoNag = true
		}
		rb.WriteString("</reflect>")
		prompts = append(prompts, rb.String())
	} else if needNag {
		prompts = append(prompts, app.PromptLoader.LoadOr("todo_nag",
			"<task-nag>You have open task items. Update them via TodoWrite before continuing.</task-nag>"))
		resetTodoNag = true
	}

	return prompts, resetFailures, resetTodoNag, resetStuck
}
