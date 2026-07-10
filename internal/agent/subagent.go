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

	role := agentType
	if role == "" {
		role = "Explore"
	}
	sysPrompt := fmt.Sprintf(
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

	msgs := []llm.Message{
		llm.SystemMessage(sysPrompt),
		llm.UserMessage(prompt),
	}

	// softDeadline lets us return an honest "here's what I found so far"
	// summary instead of being hard-killed by the caller's ctx deadline
	// (runToolWithTimeout's toolTimeoutOverrides["task"], see loop.go)
	// with the entire investigation discarded. Only armed when ctx
	// actually carries a deadline (it always does via the task tool
	// path, but callers/tests may pass a bare context.Background()).
	var softDeadline time.Time
	if dl, ok := ctx.Deadline(); ok {
		softDeadline = dl.Add(-infra.SubagentSoftDeadlineBuffer)
	}

	// steps is a lightweight, append-only trail of what the subagent
	// actually did (tool name + a short arg hint per call). Cheap to
	// maintain, and is exactly the information that used to be thrown
	// away on timeout - the parent had to re-read every file from
	// scratch to rediscover what the subagent had already found.
	var steps []string
	var lastContent string
	timedOut := false

roundLoop:
	for range infra.SubagentMaxRounds {
		if !softDeadline.IsZero() && !time.Now().Before(softDeadline) {
			timedOut = true
			break
		}
		if ctx.Err() != nil {
			// Real cancellation (Ctrl-C, parent already gone) - no
			// point drafting a summary nobody will read, but still
			// distinguish it in the fallback message below.
			timedOut = true
			break
		}

		sr, err := llm.NewClient(nil).StreamWithRetrySink(ctx, "subagent", llm.CallParams{Model: App.Model, Messages: msgs, Tools: subTools, MaxTokens: infra.DefaultMaxOutputTokens},
			&llm.PrefixedStreamSink{Prefix: "[sub]", Color: logging.ColorCyan})
		if err != nil {
			// A ctx-deadline/cancellation error surfacing from the LLM
			// call itself is just the hard ceiling catching up to us
			// slightly before our own soft check did - treat it the
			// same as a soft-deadline stop rather than a hard failure,
			// so the caller still gets the partial-progress summary.
			if ctx.Err() != nil {
				timedOut = true
				break roundLoop
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
	}
	if json.Unmarshal([]byte(rawArgs), &a) == nil {
		switch {
		case a.Path != "":
			return a.Path
		case a.Command != "":
			return utils.Truncate(a.Command, 60)
		case a.Query != "":
			return utils.Truncate(a.Query, 60)
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
