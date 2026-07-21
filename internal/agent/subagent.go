package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent-refactor/internal/event"
	"go-code-agent-refactor/internal/llm"
	"go-code-agent-refactor/internal/model"
	"go-code-agent-refactor/internal/tool"
	"go-code-agent-refactor/internal/utils"
	"strings"
)

// SubagentRunner runs an isolated read-only agent loop using the unified Runner
// and returns a summary string.
type SubagentRunner struct {
	gateway   *model.Gateway
	catalog   *tool.ToolCatalog
	modelID   string
	eventSink event.Sink
	compress  *Compression
}

func NewSubagentRunner(gw *model.Gateway, catalog *tool.ToolCatalog, modelID string) *SubagentRunner {
	return &SubagentRunner{gateway: gw, catalog: catalog, modelID: modelID}
}

func (s *SubagentRunner) SetEventSink(sink event.Sink) {
	s.eventSink = sink
}

// SetCompression enables auto-compaction for subagent runners.
// Without it, subagent context grows unboundedly over many rounds
// (MicroCompact only clears old tool results, not the growing
// assistant / user message stack).
func (s *SubagentRunner) SetCompression(c *Compression) {
	s.compress = c
}

// Run executes a subagent investigation using the unified Runner and returns a summary.
func (s *SubagentRunner) Run(ctx context.Context, prompt, agentType, workdir string) string {
	role := agentType
	if role == "" {
		role = "explore"
		agentType = "explore"
	}

	sysPrompt := buildSubagentSystemPrompt(role, agentType)

	// Build scope and profile for the subagent
	scope := &tool.ToolScope{
		Role:       "explore",
		Workdir:    workdir,
		AgentID:    fmt.Sprintf("subagent-%s", agentType),
		CanRead:    true,
		CanWrite:   false,
		CanExecute: true,
		CanNetwork: (agentType == "web_fetch"),
		CanTeam:    false,
		CanMemory:  false,
	}

	profile := NewExploreProfile()
	profile.SystemPrompt = sysPrompt
	profile.CanNetwork = (agentType == "web_fetch")

	// Create executor and runner for this subagent invocation.
	// Subagent tool output is truncated so raw file contents don't
	// collapse DeepSeek's prefix cache (each read_file appends
	// thousands of chars to the message list, reshuffling the
	// entire prefix for subsequent requests).
	exec := tool.NewExecutor(s.catalog, nil, nil).
		WithSanitizer(&truncateSanitizer{maxLen: 3000})
	runner := NewRunner(profile, s.gateway, exec, scope)
	runner.SetEventSink(s.eventSink)
	if s.compress != nil {
		runner.SetCompression(s.compress)
	}

	messages := []llm.Message{
		llm.SystemMessage(sysPrompt),
		llm.UserMessage(prompt),
	}

	traceID := model.NewTraceID()
	outcome := runner.Run(ctx, messages, traceID)

	finalText := lastAssistantText(outcome.Messages)

	// Collect tool step descriptions before handling errors so partial work
	// is not lost when the parent context is cancelled or the budget expires.
	var steps []string
	for _, tr := range outcome.ToolResults {
		steps = append(steps, fmt.Sprintf("%s(%s)", tr.Name, subagentArgHint(tr.Args)))
	}

	if outcome.Error != nil {
		if ctx.Err() != nil {
			return formatSubagentTimeoutSummary(steps, finalText)
		}
		return fmt.Sprintf("Subagent error: %v", outcome.Error)
	}

	if outcome.StoppedReason == "prompt_budget" {
		return formatSubagentStoppedSummary("prompt budget exhausted", steps, finalText)
	}
	if outcome.Completed {
		return finalText
	}
	if finalText != "" {
		return finalText
	}
	return formatSubagentTimeoutSummary(steps, finalText)
}

func buildSubagentSystemPrompt(role, agentType string) string {
	switch agentType {
	case "web_fetch":
		return fmt.Sprintf(
			"You are a web research subagent (role=%s). You fetch and analyze web pages, then return a concise summary. "+
				"You have NO write/edit/delete tools. Use web_fetch to retrieve the target page. "+
				"Keep your response focused and relevant.",
			role)
	default:
		return fmt.Sprintf(
			"You are a coding subagent (role=%s). You can read files and run safe shell "+
				"commands to investigate, but you have NO write/edit/delete tools. "+
				"Investigate efficiently: when you need a specific fact, locate it first with grep/rg. "+
				"Do NOT read entire large files just to check one value. "+
				"If the task requires modifying files, return a concise summary of what change is needed. "+
				"Report only files, symbols, and command results you actually observed; never invent paths, tests, or implementation details. "+
				"If something was not verified, say so explicitly.",
			role)
	}
}

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

func formatSubagentTimeoutSummary(steps []string, lastContent string) string {
	return formatSubagentStoppedSummary("time budget exhausted", steps, lastContent)
}

func formatSubagentStoppedSummary(reason string, steps []string, lastContent string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[Subagent stopped early - %s after %d tool call(s)]\n", reason, len(steps)))
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
		b.WriteString("(none)")
	}
	b.WriteString("\n\nThe investigation above is INCOMPLETE. Use it as a starting point.")
	return b.String()
}

// lastAssistantText returns the content of the last assistant message.
func lastAssistantText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// prefixedSink wraps a model.StreamSink with a prefix and color consistent
// with the original project's terminal output conventions.
//
//	lead   → green body, no [lead] prefix
//	explore/teammate → cyan [sub] prefix, cyan body
type prefixedSink struct {
	Prefix string
	color  string
	isLead bool
}

func newPrefixedSink(role string) *prefixedSink {
	s := &prefixedSink{Prefix: role}
	if role == "lead" {
		s.isLead = true
		s.color = utils.Green
	} else {
		s.color = utils.Cyan
	}
	return s
}

func (s *prefixedSink) OnTextDelta(text string) {
	// Print immediately instead of buffering until OnDone.
	// This avoids the user-perceived "hang" when the model is
	// generating a long response: streaming content is visible in
	// real time, and Ctrl-C during generation doesn't lose
	// already-seen output.
	if s.isLead {
		fmt.Printf("%s%s%s", s.color, text, utils.Reset)
	} else {
		fmt.Printf("%s[sub] %s%s", s.color, text, utils.Reset)
	}
}

func (s *prefixedSink) OnDone() {
	if s.isLead {
		fmt.Println()
	} else {
		fmt.Println()
	}
}

// truncateSanitizer caps tool output length in subagents so that
// raw file contents don't inflate the message list and collapse the
// prompt prefix cache.
type truncateSanitizer struct{ maxLen int }

func (t *truncateSanitizer) Sanitize(s string) string {
	if len(s) <= t.maxLen {
		return s
	}
	if t.maxLen <= 256 {
		return s[:t.maxLen] + "\n... (truncated)"
	}
	// Keep the tail because bounded read/search tools put continuation
	// metadata (such as next_offset) there. This prevents the model from
	// repeatedly requesting the same file prefix after truncation.
	headLen := t.maxLen - 256
	return s[:headLen] + fmt.Sprintf("\n... (truncated at %d chars) ...\n", t.maxLen) + s[len(s)-256:]
}
