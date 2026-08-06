package event

import (
	"fmt"
	"strings"
	"unicode"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/utils"
)

func decisionLabel(et EventType) string {
	switch et {
	case PlanningDecision:
		return "policy"
	case ContextDecision:
		return "context"
	case TurnComplete:
		return "turn_complete"
	case MemoryDecision:
		return "memory"
	case JudgeDecision:
		return "judge"
	case ReflectionTriggered:
		return "reflection"
	default:
		return string(et)
	}
}

func decisionColor(et EventType) string {
	switch et {
	case AgentStarted:
		return utils.BoldGreen
	case ModelCalled:
		return utils.BoldYellow
	case ToolStarted, ToolFinished, TurnDecision:
		return utils.Cyan
	case ReflectionTriggered:
		return utils.BoldMagenta
	case PlanningDecision:
		return utils.BoldMagenta
	case ContextDecision:
		return utils.Dim
	case TurnComplete:
		return utils.BoldGreen
	case MemoryDecision:
		return utils.BoldGreen
	case JudgeDecision:
		return utils.BoldYellow
	default:
		return utils.Dim
	}
}

// renderConsoleEvent formats one event without performing I/O. ConsoleSink
// owns serialization and writes the resulting text to stderr.
func renderConsoleEvent(e Event) string {
	var out strings.Builder

	// Per-turn summary — prominent standalone line.
	if e.Type == TurnDecision {
		mp, _ := e.Payload.(map[string]string)
		fmt.Fprintf(&out, "%s--- turn %s ---%s tools=%s failures=%s\n",
			utils.BoldCyan, mp["round"], utils.Reset, mp["tool_calls"], mp["failures"])
		return out.String()
	}

	// Model retry wait / provider fallback — keep the user informed during
	// backoff instead of showing a silent terminal.
	if e.Type == ModelRetry {
		mp, _ := e.Payload.(map[string]string)
		fmt.Fprintf(&out, "%s[retry]%s %s\n", utils.BoldYellow, utils.Reset, mp["summary"])
		return out.String()
	}

	// PlanningDecision is a local policy gate (nudge / block), not model
	// "thinking". Always label it [policy] and surface a short action summary.
	if e.Type == PlanningDecision {
		mp, _ := e.Payload.(map[string]string)
		fmt.Fprintf(&out, "%s[policy]%s %s", utils.BoldMagenta, utils.Reset, policyPlanningSummary(mp, e))
		if e.AgentID != "" && e.AgentID != "lead" {
			fmt.Fprintf(&out, " agent=%s", e.AgentID)
		}
		if e.Status != "" {
			fmt.Fprintf(&out, " status=%s", e.Status)
		}
		if e.Error != "" {
			fmt.Fprintf(&out, " %serror=%s%s", utils.Red, consoleLine(e.Error), utils.Reset)
		}
		out.WriteByte('\n')
		return out.String()
	}

	label := decisionLabel(e.Type)
	color := decisionColor(e.Type)
	payload, _ := e.Payload.(map[string]string)
	var preview []string
	previewOmitted := 0

	if e.ToolName != "" {
		action := string(e.Type)
		switch e.Type {
		case ToolStarted:
			action = "started"
		case ToolFinished:
			action = e.Status
			if action == "" {
				action = "finished"
			}
		}
		fmt.Fprintf(&out, "%s[%s]%s %s%s%s",
			utils.BoldYellow, e.ToolName, utils.Reset, color, action, utils.Reset)
	} else {
		fmt.Fprintf(&out, "%s[%s]%s", color, label, utils.Reset)
	}
	// The active terminal already identifies the session. Keep agent identity
	// only for concurrent subagents and teammates, where it disambiguates output.
	if e.AgentID != "" && e.AgentID != "lead" {
		fmt.Fprintf(&out, " agent=%s", e.AgentID)
	}
	if path := payload["path"]; path != "" {
		fmt.Fprintf(&out, " path=%s", consoleLine(path))
	}
	if pattern := payload["pattern"]; pattern != "" {
		fmt.Fprintf(&out, " pattern=%q", consoleLine(pattern))
	}
	if offset := payload["offset"]; offset != "" {
		fmt.Fprintf(&out, " offset=%s", offset)
	}
	if limit := payload["limit"]; limit != "" {
		fmt.Fprintf(&out, " limit=%s", limit)
	}
	if e.Status != "" && e.Type != ToolFinished {
		fmt.Fprintf(&out, " status=%s", e.Status)
	}
	if e.Error != "" {
		fmt.Fprintf(&out, " %serror=%s%s", utils.Red, consoleLine(e.Error), utils.Reset)
	}
	if e.Duration > 0 {
		fmt.Fprintf(&out, " dur=%.2fs", e.Duration.Seconds())
	}
	if e.Type == ToolFinished {
		switch {
		case e.Status != "succeeded" && e.Output != "":
			if e.Error == "" {
				fmt.Fprintf(&out, " %serror=%s%s", utils.Red, consoleLine(e.Output), utils.Reset)
			}
		case e.Status == "succeeded" && consoleResultLabel(e.ToolName) != "":
			label := consoleResultLabel(e.ToolName)
			var total int
			if isTaskStateTool(e.ToolName) {
				preview, total = consoleFullToolOutput(e.Output)
			} else {
				preview, total = consoleMutationPlanner(e.ToolName, e.Output, 8)
				previewOmitted = total - len(preview)
			}
			fmt.Fprintf(&out, " %s=%d", label, total)
		case e.Status == "succeeded" && e.Output != "" && len(e.Output) <= 240:
			fmt.Fprintf(&out, " %s", consoleLine(e.Output))
		case e.Status == "succeeded" && e.Output != "":
			fmt.Fprintf(&out, " output=%dB", len(e.Output))
		}
	}
	// Print payload fields for decision events (matches original verbose output).
	if mp, ok := e.Payload.(map[string]string); ok && len(mp) > 0 {
		if k := mp["kind"]; k != "" && e.Type == ReflectionTriggered {
			fmt.Fprintf(&out, " %skind=%s%s", utils.BoldMagenta, k, utils.Reset)
		}
		if s := mp["action"]; s != "" {
			fmt.Fprintf(&out, " %s%s%s", utils.Dim, s, utils.Reset)
		}
		if s := mp["summary"]; s != "" {
			fmt.Fprintf(&out, " %s", s)
		}
		if r := mp["rounds"]; r != "" {
			fmt.Fprintf(&out, " %srounds=%s%s", utils.Dim, r, utils.Reset)
		}
		if cf := mp["consecutive_fails"]; cf != "" {
			fmt.Fprintf(&out, " %sconsecutive_fails=%s%s", utils.Dim, cf, utils.Reset)
		}
		if p := mp["prompt_count"]; p != "" {
			fmt.Fprintf(&out, " %sprompts=%s%s", utils.Dim, p, utils.Reset)
		}
		if sc := mp["score"]; sc != "" {
			fmt.Fprintf(&out, " %sscore=%s%s", utils.Dim, sc, utils.Reset)
		}
		if ap := mp["approved"]; ap != "" {
			fmt.Fprintf(&out, " %sapproved=%s%s", utils.Dim, ap, utils.Reset)
		}
		if cl := mp["cleared"]; cl != "" {
			fmt.Fprintf(&out, " %scleared=%s%s", utils.Dim, cl, utils.Reset)
		}
	}
	if e.Type == TurnComplete && e.Usage != nil && !e.Usage.IsZero() {
		fmt.Fprintf(&out, " %sin=%d out=%d reasoning=%d hit=%d miss=%d hit_rate=%.1f%%%s",
			utils.Dim, e.Usage.PromptTokens, e.Usage.CompletionTokens,
			e.Usage.ReasoningTokens, e.Usage.CachedReadTokens, e.Usage.CacheMissTokens,
			cacheHitRate(*e.Usage), utils.Reset)
	}
	out.WriteByte('\n')
	for _, line := range preview {
		fmt.Fprintf(&out, "%s  %s%s\n", utils.Dim, line, utils.Reset)
	}
	if previewOmitted > 0 {
		fmt.Fprintf(&out, "%s  ... %d more lines%s\n", utils.Dim, previewOmitted, utils.Reset)
	}
	return out.String()
}

func policyPlanningSummary(mp map[string]string, e Event) string {
	action := ""
	if mp != nil {
		action = mp["action"]
	}
	switch action {
	case "require_plan":
		return "plan required"
	case "require_dag_edges":
		return "dag edges required"
	case "block_unplanned_side_effect":
		parts := []string{"blocked unplanned side effect"}
		if e.ToolName != "" {
			parts = append(parts, e.ToolName)
		}
		if c := mp["classification"]; c != "" {
			parts = append(parts, c)
		}
		return strings.Join(parts, " ")
	default:
		if action != "" {
			return action
		}
		return "planning"
	}
}

func consoleLine(text string) string {
	return utils.Truncate(consoleText(strings.Join(strings.Fields(text), " ")), 240)
}

func consoleResultLabel(toolName string) string {
	switch toolName {
	case "read_file":
		return "lines"
	case "list_dir":
		return "entries"
	case "search_file", "search_content":
		return "matches"
	case "TodoWrite":
		return "items"
	case "task_dag":
		return "nodes"
	case "task_create", "task_list", "task_update", "task_get",
		"task_ready", "claim_task", "task_add_dep", "task_remove_dep":
		return "result"
	default:
		return ""
	}
}

func isTaskStateTool(toolName string) bool {
	switch toolName {
	case "TodoWrite", "task_dag", "task_create", "task_list", "task_update",
		"task_get", "task_ready", "claim_task", "task_add_dep", "task_remove_dep":
		return true
	default:
		return false
	}
}

func consoleFullToolOutput(text string) ([]string, int) {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return nil, 0
	}
	lines := strings.Split(text, "\n")
	output := make([]string, 0, len(lines))
	for _, line := range lines {
		output = append(output, consoleText(line))
	}
	return output, len(lines)
}

func consoleMutationPlanner(toolName, text string, maxLines int) ([]string, int) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" ||
		(toolName == "search_file" && trimmed == "No files matched.") ||
		(toolName == "search_content" && trimmed == "No matches found.") {
		return nil, 0
	}
	return consolePreview(text, maxLines)
}

func consolePreview(text string, maxLines int) ([]string, int) {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if text == "" {
		return nil, 0
	}
	lines := strings.Split(text, "\n")
	limit := min(len(lines), maxLines)
	preview := make([]string, 0, limit)
	for _, line := range lines[:limit] {
		preview = append(preview, utils.Truncate(consoleText(line), 180))
	}
	return preview, len(lines)
}

func consoleText(text string) string {
	text = strings.ReplaceAll(text, "	", "    ")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

func cacheHitRate(u llm.Usage) float64 {
	input := u.PromptTokens
	if input <= 0 {
		input = u.CachedReadTokens + u.CacheMissTokens
	}
	if input <= 0 {
		return 0
	}
	return float64(u.CachedReadTokens) / float64(input) * 100
}
