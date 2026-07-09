// Reflection module: self-evaluation triggers.
//
// Triggers (each maintains an independent cool-down so the same kind
// of reflection prompt isn't injected into the conversation every
// round; without cool-downs the message list quickly fills with
// near-identical "<reflect>...</reflect>" blocks, drowning out the
// actual signal):
//
//  1. consecutiveFailures == 1 -> mini-reflect      (no cooldown — one-shot per first failure)
//  2. consecutiveFailures >= max -> strategy-change (cooldown: strategyChangeCooldown)
//  3. roundsSinceLastComplete >= stuckThreshold -> stuck   (cooldown: stuckCooldown)
//  4. toolRounds % reflectInterval == 0 -> periodic        (cooldown: periodicCooldown)
//  5. roundsWithoutTodo >= 3 && hasOpenItems -> todo-nag   (cooldown: todoNagCooldown)
//
// Pure function: returns prompts + reset flags + the set of trigger
// kinds that fired this round, without side effects. The caller
// records "kind -> toolRounds" in its loopState so the next invocation
// can honor the cool-down.
package agent

import (
	"fmt"
	"strings"
)

// Reflection trigger kinds. Used as map keys in `lastTriggered`.
const (
	reflectKindMini     = "mini"
	reflectKindStrategy = "strategy"
	reflectKindStuck    = "stuck"
	reflectKindPeriodic = "periodic"
	reflectKindTodoNag  = "todo_nag"
)

// Cool-down windows (in tool-round units). Tuned conservatively: long
// enough to break out of a tight loop, short enough that real new
// problems still surface within a few rounds.
const (
	strategyChangeCooldown = 5
	stuckCooldown          = 5
	periodicCooldown       = 0 // governed by reflectInterval already
	todoNagCooldown        = 3
)

// reflect evaluates agent state and returns reflection prompts to
// inject. `lastTriggered` maps trigger-kind -> the toolRounds value at
// which that kind last fired; pass an empty map on the very first
// call. `triggered` is the set of kinds that fired *this* round, so
// the caller can update its own `lastTriggered` snapshot.
func reflect(
	consecutiveFailures int, lastFailedTool string, maxConsecutiveFailures int,
	toolRounds int, totalFailures int,
	roundsSinceLastComplete int, roundsWithoutTodo int,
	stuckThreshold int, reflectInterval int,
	hasOpenItems bool,
	lastTriggered map[string]int,
) (prompts []string, resetFailures, resetTodoNag, resetStuck bool, triggered []string) {

	// onCooldown returns true when `kind` was triggered within the
	// last `window` rounds (inclusive). A window of <=0 disables the
	// cool-down for that kind.
	onCooldown := func(kind string, window int) bool {
		if window <= 0 {
			return false
		}
		last, ok := lastTriggered[kind]
		if !ok {
			return false
		}
		return toolRounds-last < window
	}

	// 1) Immediate mini-reflect on first failure. No cool-down: this
	//    is already gated by the "first failure of a tool" condition,
	//    so it is naturally one-shot.
	if consecutiveFailures == 1 {
		prompts = append(prompts, fmt.Sprintf(
			"<mini-reflect>Tool '%s' failed. Before retrying, consider: "+
				"Is the approach correct? Are the arguments right? "+
				"Would a different tool or method work better?</mini-reflect>",
			lastFailedTool))
		triggered = append(triggered, reflectKindMini)
	}

	// 2) Force strategy change after repeated failures.
	if consecutiveFailures >= maxConsecutiveFailures && !onCooldown(reflectKindStrategy, strategyChangeCooldown) {
		// Fallback ensures reflection still works if the prompt file
		// is missing — silent empty injection here would defeat the
		// whole point of the strategy-change gate.
		tmpl := App.PromptLoader.LoadOr("strategy_change",
			"<strategy-change>Tool '{{tool}}' has failed {{count}} times in a row. "+
				"STOP retrying the same approach. Re-read the error, then either: "+
				"(a) try a different tool, (b) gather more context first, or "+
				"(c) ask the user for clarification.</strategy-change>")
		msg := strings.Replace(strings.Replace(tmpl,
			"{{tool}}", lastFailedTool, 1),
			"{{count}}", fmt.Sprintf("%d", consecutiveFailures), 1)
		prompts = append(prompts, msg)
		resetFailures = true
		triggered = append(triggered, reflectKindStrategy)
	} else if consecutiveFailures >= maxConsecutiveFailures {
		// Even when suppressed by cool-down, still reset the failure
		// counter so we don't re-trigger every round and so the next
		// failure starts a fresh streak. Without this reset the loop
		// would be stuck above the threshold and only the cooldown
		// would gate output, hiding real progress.
		resetFailures = true
	}

	// 3) Stuck detection: too many rounds without completing any DAG task.
	if roundsSinceLastComplete >= stuckThreshold && App.DagSched().TaskCount() > 0 &&
		!onCooldown(reflectKindStuck, stuckCooldown) {
		prompts = append(prompts, fmt.Sprintf(
			"<stuck>You have spent %d rounds without completing a task. "+
				"Step back and reconsider your approach. Use task_dag to review the plan. "+
				"Consider: Is the current task too large? Should you break it down further? "+
				"Is there a blocker you can work around?</stuck>",
			roundsSinceLastComplete))
		resetStuck = true
		triggered = append(triggered, reflectKindStuck)
	} else if roundsSinceLastComplete >= stuckThreshold && App.DagSched().TaskCount() > 0 {
		// Same idea as the strategy branch: clear the counter so the
		// next "stuck" episode starts from zero rather than firing
		// every round once we cross the threshold.
		resetStuck = true
	}

	// 4) Periodic reflection / 5) task nag.
	needReflect := reflectInterval > 0 && toolRounds > 0 && toolRounds%reflectInterval == 0 &&
		!onCooldown(reflectKindPeriodic, periodicCooldown)
	needNag := hasOpenItems && roundsWithoutTodo >= 3 &&
		!onCooldown(reflectKindTodoNag, todoNagCooldown)

	if needReflect {
		// Build context-aware reflection with actual data.
		var rb strings.Builder
		rb.WriteString("<reflect>Pause and evaluate (round ")
		rb.WriteString(fmt.Sprintf("%d", toolRounds))
		rb.WriteString("):\n")
		if totalFailures > 0 {
			rb.WriteString(fmt.Sprintf("- %d tool failures so far this session.\n", totalFailures))
		}
		if ps := App.DagSched().ProgressSummary(); ps != "" {
			rb.WriteString(fmt.Sprintf("- %s\n", ps))
		}
		rb.WriteString("1. Are your actions achieving the intended goal?\n")
		rb.WriteString("2. Did any tool call fail? If so, change strategy.\n")
		rb.WriteString("3. What should you do next? Is there a more efficient approach?\n")
		if needNag {
			rb.WriteString("4. You have open todos - update them now.\n")
			resetTodoNag = true
			triggered = append(triggered, reflectKindTodoNag)
		}
		rb.WriteString("</reflect>")
		prompts = append(prompts, rb.String())
		triggered = append(triggered, reflectKindPeriodic)
	} else if needNag {
		prompts = append(prompts, App.PromptLoader.LoadOr("todo_nag",
			"<task-nag>You have open task items. Update them via TodoWrite before continuing.</task-nag>"))
		resetTodoNag = true
		triggered = append(triggered, reflectKindTodoNag)
	}

	return prompts, resetFailures, resetTodoNag, resetStuck, triggered
}
