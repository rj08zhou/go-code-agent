package agent

import (
	"fmt"
	"go-code-agent/internal/prompt"
	"strings"
)

// Reflection trigger kinds.
const (
	reflectKindMini          = "mini"
	reflectKindStrategy      = "strategy"
	reflectKindStuck         = "stuck"
	reflectKindInvestigation = "investigation_stuck"
	reflectKindPeriodic      = "periodic"
	reflectKindTodoNag       = "todo_nag"
)

// Cool-down windows (in tool-round units).
const (
	strategyChangeCooldown = 5
	stuckCooldown          = 5
	investigationCooldown  = 3
	todoNagCooldown        = 3
)

// Reflection evaluates agent state and returns reflection prompts to inject.
type Reflection struct {
	promptLoader *prompt.Loader
}

func NewReflection(pl *prompt.Loader) *Reflection {
	return &Reflection{promptLoader: pl}
}

// Eval generates reflection prompts based on agent state.
func (r *Reflection) Eval(
	consecutiveFailures int, lastFailedTool string,
	maxConsecutiveFailures int,
	toolRounds int, totalFailures int,
	roundsSinceLastComplete int, roundsWithoutTodo int,
	stuckThreshold int, reflectInterval int,
	hasOpenItems bool,
	lastTriggered map[string]int,
	taskCount int, progressSummary string,
) (prompts []string, resetFailures, resetTodoNag, resetStuck bool, triggered []string) {

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

	// 1) Mini-reflect on first failure.
	if consecutiveFailures == 1 {
		prompts = append(prompts, fmt.Sprintf(
			"<mini-reflect>Tool '%s' failed. Before retrying, consider: "+
				"Is the approach correct? Are the arguments right? "+
				"Would a different tool or method work better?</mini-reflect>",
			lastFailedTool))
		triggered = append(triggered, reflectKindMini)
	}

	// 2) Strategy change after repeated failures.
	if consecutiveFailures >= maxConsecutiveFailures && !onCooldown(reflectKindStrategy, strategyChangeCooldown) {
		tmpl := r.promptLoader.Load("strategy_change")
		if tmpl == "" {
			tmpl = "<strategy-change>Tool '{{tool}}' has failed {{count}} times in a row. " +
				"STOP retrying the same approach. Re-read the error, then either: " +
				"(a) try a different tool, (b) gather more context first, or " +
				"(c) ask the user for clarification.</strategy-change>"
		}
		msg := strings.Replace(strings.Replace(tmpl,
			"{{tool}}", lastFailedTool, 1),
			"{{count}}", fmt.Sprintf("%d", consecutiveFailures), 1)
		prompts = append(prompts, msg)
		resetFailures = true
		triggered = append(triggered, reflectKindStrategy)
	} else if consecutiveFailures >= maxConsecutiveFailures {
		resetFailures = true
	}

	// 3) Investigation stuck — tools are succeeding but no progress is made.
	// Common in explore subagents that read the same files repeatedly
	// without converging on a summary. Fires earlier than the generic
	// stuck detector to save tokens.
	if consecutiveFailures == 0 && totalFailures == 0 &&
		roundsSinceLastComplete >= 10 && taskCount > 0 &&
		!onCooldown(reflectKindInvestigation, investigationCooldown) {
		prompts = append(prompts,
			"<investigation-stuck>You have been running tools successfully for "+
				fmt.Sprintf("%d", roundsSinceLastComplete)+
				" rounds but haven't completed the task. Your tools are working — the problem is your approach. "+
				"STOP reading more files. Instead:\n"+
				"1. Summarize what you already know in 2-3 sentences.\n"+
				"2. Identify the single missing piece of information you still need.\n"+
				"3. Use the most direct tool to get ONLY that piece.\n"+
				"4. Then respond with your final answer immediately.\n"+
				"Do NOT read another large file or list another directory.</investigation-stuck>")
		resetStuck = true
		triggered = append(triggered, reflectKindInvestigation)
	}

	// 4) Stuck detection.
	if roundsSinceLastComplete >= stuckThreshold && taskCount > 0 &&
		!onCooldown(reflectKindStuck, stuckCooldown) {
		prompts = append(prompts, fmt.Sprintf(
			"<stuck>You have spent %d rounds without completing a task. "+
				"Step back and reconsider your approach. Use task_dag to review the plan. "+
				"Consider: Is the current task too large? Should you break it down further? "+
				"Is there a blocker you can work around?</stuck>",
			roundsSinceLastComplete))
		resetStuck = true
		triggered = append(triggered, reflectKindStuck)
	} else if roundsSinceLastComplete >= stuckThreshold && taskCount > 0 {
		resetStuck = true
	}

	// 4) Periodic reflection / 5) todo nag.
	needReflect := reflectInterval > 0 && toolRounds > 0 && toolRounds%reflectInterval == 0 &&
		!onCooldown(reflectKindPeriodic, 0)
	needNag := hasOpenItems && roundsWithoutTodo >= 3 &&
		!onCooldown(reflectKindTodoNag, todoNagCooldown)

	if needReflect {
		var rb strings.Builder
		rb.WriteString(fmt.Sprintf("<reflect>Pause and evaluate (round %d):\n", toolRounds))
		if totalFailures > 0 {
			rb.WriteString(fmt.Sprintf("- %d tool failures so far this session.\n", totalFailures))
		}
		if progressSummary != "" {
			rb.WriteString(fmt.Sprintf("- %s\n", progressSummary))
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
		tmpl := r.promptLoader.Load("todo_nag")
		if tmpl == "" {
			tmpl = "<task-nag>You have open task items. Update them via TodoWrite before continuing.</task-nag>"
		}
		prompts = append(prompts, tmpl)
		resetTodoNag = true
		triggered = append(triggered, reflectKindTodoNag)
	}

	return
}
