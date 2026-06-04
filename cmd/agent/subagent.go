package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/hitl_audit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/log"
)

// Subagent - isolated read-only agent loop, returns summary only.
//
// Subagents never get write tools. They pass through the same security
// and HITL gates as the main agent. If a write is needed, the subagent
// returns a summary and the parent performs the write.

func runSubagent(ctx context.Context, prompt, agentType string) string {
	// Subagents NEVER receive write tools.
	subTools := coreToolDefs(false)
	subHandlers := coreToolHandlers()

	role := agentType
	if role == "" {
		role = "Explore"
	}
	sysPrompt := fmt.Sprintf(
		"You are a coding subagent (role=%s). You can read files and run safe shell "+
			"commands to investigate, but you have NO write/edit/delete tools. "+
			"If the task requires modifying files, do the investigation, then "+
			"return a concise summary describing exactly what change is needed "+
			"and why — the parent agent will perform the write.",
		role,
	)

	msgs := []llm.Message{
		llm.SystemMessage(sysPrompt),
		llm.UserMessage(prompt),
	}
	var lastContent string
	for range infra.SubagentMaxRounds {
		sr, err := llm.NewClient(nil).StreamWithRetrySink(ctx, "subagent", llm.CallParams{Model: model, Messages: msgs, Tools: subTools, MaxTokens: infra.DefaultMaxOutputTokens},
			&llm.PrefixedStreamSink{Prefix: "[sub]", Color: log.ColorCyan})
		if err != nil {
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
				log.PrintSubTool(tc.Name, out)
				msgs = append(msgs, llm.ToolMessage(out, tc.ID))
				continue
			}
			out := dispatchSubagentTool(ctx, tc, subHandlers)
			log.PrintSubTool(tc.Name, out)
			msgs = append(msgs, llm.ToolMessage(out, tc.ID))
		}
	}
	if lastContent != "" {
		return lastContent
	}
	return "(no summary)"
}

// dispatchSubagentTool runs security/HITL gates against the subagent's
// restricted handler map. On rejection, returns a tool-result describing it.
func dispatchSubagentTool(ctx context.Context, tc llm.ToolCall, handlers map[string]ToolHandler) string {
	// Security gate.
	if approved, reason := checkToolApproval(tc.Name); !approved {
		return fmt.Sprintf("[SECURITY] %s", reason)
	}

	// HITL gate.
	if needReview, risk, hitlReason := hitl_audit.HitlManager.NeedsReview(tc.Name, tc.Arguments); needReview {
		sid := ""
		if app != nil && app.SessionManager.Active() != nil {
			sid = app.SessionManager.Active().ID()
		}
		resp := hitl_audit.HitlManager.RequestApproval(hitl_audit.HITLRequest{
			ToolName:  tc.Name,
			Arguments: tc.Arguments,
			RiskLevel: risk,
			Reason:    hitlReason,
			SessionID: sid,
		})
		switch resp.Decision {
		case hitl_audit.HITLReject:
			return fmt.Sprintf("[HITL-REJECTED] %s", hitlReason)
		case hitl_audit.HITLModify:
			return fmt.Sprintf("[HITL-MODIFY] %s", resp.Feedback)
		case hitl_audit.HITLApprove:
		}
	}

	h, ok := handlers[tc.Name]
	if !ok {
		return fmt.Sprintf("[ERROR] tool %q is not available in subagent context", tc.Name)
	}
	result := h(ctx, json.RawMessage(tc.Arguments))
	return result.Output
}
