package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/utils"
	"strings"
	"time"
)

// Subagent - isolated read-only agent loop, returns summary only.

func runSubagent(ctx context.Context, prompt, agentType string) string {
	// Subagents never receive write tools.
	subTools := coreToolDefs(false)
	subHandlers := coreToolHandlers(false)

	// Inject type-specific tools.
	switch agentType {
	case "web_fetch":
		raw := rawWebFetchSpec()
		subTools = append(subTools, raw.Def)
		subHandlers[raw.Def.Name] = raw.Handler
	}

	role := agentType
	if role == "" {
		role = "Explore"
	}

	// Build a role-tailored system prompt.
	var sysPrompt string
	switch agentType {
	case "web_fetch":
		sysPrompt = fmt.Sprintf(
			"You are a web research subagent (role=%s). You fetch and analyze web pages, then return a concise summary to the parent agent. "+
				"You can also read local files and run safe shell commands for reference. "+
				"You have NO write/edit/delete tools. "+
				"Use web_fetch to retrieve the target page. Read the content carefully, then answer the user's question directly from the page. "+
				"Keep your response focused and relevant — do NOT add unrelated commentary or repeat boilerplate.",
			role,
		)
	default:
		sysPrompt = fmt.Sprintf(
			"You are a coding subagent (role=%s). You can read files and run safe shell "+
				"commands to investigate, but you have NO write/edit/delete tools. "+
				"Investigate efficiently: when you need a specific fact, symbol, constant "+
				"or a few lines, locate it first with grep/rg via bash (e.g. rg -n \"Name\" .) "+
				"and read only the relevant part (pass read_file's `limit` for large files). "+
				"Do NOT read entire large files just to check one value - it burns context "+
				"and triggers compaction. "+
				"If the task requires modifying files, do the investigation, then "+
				"return a concise summary describing exactly what change is needed "+
				"and why — the parent agent will perform the write.",
			role,
		)
	}

	msgs := []llm.Message{
		llm.SystemMessage(sysPrompt),
		llm.UserMessage(prompt),
	}

	var softDeadline time.Time
	if dl, ok := ctx.Deadline(); ok {
		softDeadline = dl.Add(-infra.SubagentSoftDeadlineBuffer)
	}

	var steps []string
	var lastContent string
	timedOut := false

	for range infra.SubagentMaxRounds {
		if !softDeadline.IsZero() && !time.Now().Before(softDeadline) {
			timedOut = true
			break
		}
		if ctx.Err() != nil {
			timedOut = true
			break
		}

		sr, err := llm.NewClient(nil).StreamWithRetrySink(ctx, "subagent", llm.CallParams{Model: App.Model, Messages: msgs, Tools: subTools, MaxTokens: infra.DefaultMaxOutputTokens},
			&llm.PrefixedStreamSink{Prefix: "[sub]", Color: logging.ColorCyan})
		if err != nil {
			if ctx.Err() != nil {
				timedOut = true
				break
			}
			return fmt.Sprintf("Subagent error: %v", err)
		}
		if sr == nil {
			return "Subagent error: empty response"
		}
		lastContent = sr.Content
		msgs = append(msgs, sr.ToAssistantMessage())
		if sr.FinishReason != "tool_calls" {
			break
		}
		for _, tc := range sr.ToolCalls {
			// Skip tool calls with truncated JSON arguments.
			if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
				out := fmt.Sprintf("[SKIPPED] tool call '%s' has truncated arguments", tc.Name)
				logging.PrintSubTool(tc.Name, out)
				msgs = append(msgs, llm.ToolMessage(out, tc.ID))
				continue
			}
			out := dispatchSubagentTool(ctx, tc, subHandlers)
			logging.PrintSubTool(tc.Name, out)
			msgs = append(msgs, llm.ToolMessage(out, tc.ID))
			steps = append(steps, fmt.Sprintf("%s(%s)", tc.Name, subagentArgHint(tc.Arguments)))
		}
	}

	if timedOut {
		return formatSubagentTimeoutSummary(steps, lastContent)
	}
	if lastContent != "" {
		return lastContent
	}
	return "(no summary)"
}

// subagentArgHint extracts a short, human-readable hint from a tool
// call's raw JSON arguments for the progress trail (e.g. a file path),
// falling back to a truncated raw dump for tools with no recognized
// field. Best-effort only - never fails the caller.
func subagentArgHint(rawArgs string) string {
	if rawArgs == "" {
		return ""
	}
	var a struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Query   string `json:"query"`
		URL     string `json:"url"`
	}
	if json.Unmarshal([]byte(rawArgs), &a) == nil {
		switch {
		case a.Path != "":
			return a.Path
		case a.Command != "":
			return utils.Truncate(a.Command, 60)
		case a.Query != "":
			return utils.Truncate(a.Query, 60)
		case a.URL != "":
			return utils.Truncate(a.URL, 60)
		}
	}
	return utils.Truncate(rawArgs, 60)
}

// formatSubagentTimeoutSummary builds the partial-progress report
// returned when the subagent hits its soft deadline (or real
// cancellation) instead of finishing normally. This is the fix for
// the failure mode where a task call that ran out of time used to
// return a bare timeout error and silently discard every file it had
// already read - forcing the parent agent to redo the same
// exploration from scratch.
func formatSubagentTimeoutSummary(steps []string, lastContent string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[Subagent stopped early - time budget exhausted after %d tool call(s)]\n", len(steps)))
	if len(steps) > 0 {
		b.WriteString("Investigated so far:\n")
		for _, s := range steps {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
	} else {
		b.WriteString("No tool calls completed before the time budget ran out.\n")
	}
	b.WriteString("\nLast reasoning before stopping:\n")
	if lastContent != "" {
		b.WriteString(lastContent)
	} else {
		b.WriteString("(none yet)")
	}
	b.WriteString("\n\nThe investigation above is INCOMPLETE. Use it as a starting point " +
		"(e.g. avoid re-reading the files already listed) rather than re-exploring from scratch.")
	return b.String()
}

// dispatchSubagentTool runs security/HITL gates against the subagent's
// restricted handler map. On rejection, returns a tool-result describing it.
func dispatchSubagentTool(ctx context.Context, tc llm.ToolCall, handlers map[string]ToolHandler) string {
	// Security gate.
	if approved, reason := checkToolApproval(tc.Name, tc.Arguments); !approved {
		return fmt.Sprintf("[SECURITY] %s", reason)
	}

	// HITL gate.
	if g := runHITLGate(tc); g != nil {
		if g.Rejected {
			return fmt.Sprintf("[HITL-REJECTED] %s", g.Reason)
		}
		return fmt.Sprintf("[HITL-MODIFY] %s", g.Feedback)
	}

	h, ok := handlers[tc.Name]
	if !ok {
		return fmt.Sprintf("[ERROR] tool %q is not available in subagent context", tc.Name)
	}
	result := h(ctx, json.RawMessage(tc.Arguments))
	return result.Output
}
