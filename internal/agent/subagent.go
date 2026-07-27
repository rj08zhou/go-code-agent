package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/utils"
	"strings"
)

// exploreBaseTools is the read-oriented whitelist for explore subagents.
// Write/team/memory/spawn tools are intentionally omitted so the LLM never
// sees them; bash is kept (aligned with master) but gated by HITL.
var exploreBaseTools = []string{
	"bash", "read_file", "list_dir", "search_file", "search_content",
}

// webFetchTools is the whitelist for web_fetch subagents. They only need to
// pull pages (and optionally search for alternates); giving them bash/read/
// grep just invites unproductive local-shell attempts on remote content
// (observed: repeated `grep -P` failures burning the whole time budget).
var webFetchTools = []string{"web_fetch", "web_search"}

func exploreToolNames(agentType string) []string {
	if agentType == "web_fetch" {
		return append([]string(nil), webFetchTools...)
	}
	return append([]string(nil), exploreBaseTools...)
}

// SubagentRunner runs an isolated read-only agent loop using the unified Runner
// and returns a summary string.
type SubagentRunner struct {
	gateway      *model.Gateway
	catalog      *tool.ToolCatalog
	cfg          *config.Config
	modelID      string
	promptLoader *prompt.Loader
	approval     tool.ApprovalChecker
	eventSink    event.Sink
	compress     *Compression
}

func NewSubagentRunner(gw *model.Gateway, catalog *tool.ToolCatalog, cfg *config.Config, pl *prompt.Loader) *SubagentRunner {
	modelID := "default"
	if cfg != nil && cfg.ModelID != "" {
		modelID = cfg.ModelID
	}
	return &SubagentRunner{gateway: gw, catalog: catalog, cfg: cfg, modelID: modelID, promptLoader: pl}
}

func (s *SubagentRunner) SetEventSink(sink event.Sink) {
	s.eventSink = sink
}

// SetApproval wires the session HITL adapter so explore tools are gated
// the same way as lead tools.
func (s *SubagentRunner) SetApproval(a tool.ApprovalChecker) {
	s.approval = a
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

	sysPrompt := s.buildSubagentSystemPrompt(role, agentType)

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
	// web_fetch only has ~60s of wall time; the explore default of 30 rounds
	// lets it thrash fetch↔search until the clock runs out. Cap rounds so
	// it is forced to report what it found and stop.
	if agentType == "web_fetch" {
		profile.MaxRounds = config.WebFetchSubagentMaxRounds
	}

	// Whitelist catalog: explore must not see write/team/memory tools.
	exploreCatalog := s.catalog.Subset(exploreToolNames(agentType)...)

	// Create executor and runner for this subagent invocation.
	// Subagent tool output is truncated so raw file contents don't
	// collapse DeepSeek's prefix cache (each read_file appends
	// thousands of chars to the message list, reshuffling the
	// entire prefix for subsequent requests).
	exec := tool.NewExecutor(exploreCatalog, s.approval, nil).
		WithSanitizer(&truncateSanitizer{maxLen: config.SubagentToolOutputMaxChars})
	runner := NewRunner(profile, s.gateway, exec, scope, s.cfg)
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

	// prompt_budget / soft_deadline / max_rounds now attempt a no-tools
	// wrap-up inside Runner. Prefer that summary over the INCOMPLETE stub.
	if finalText != "" && (outcome.Completed ||
		outcome.StoppedReason == "prompt_budget" ||
		outcome.StoppedReason == "soft_deadline" ||
		outcome.StoppedReason == "max_rounds") {
		return finalText
	}
	if outcome.StoppedReason == "prompt_budget" {
		return formatSubagentStoppedSummary("prompt budget exhausted", steps, finalText)
	}
	if outcome.StoppedReason == "soft_deadline" {
		return formatSubagentTimeoutSummary(steps, finalText)
	}
	if outcome.Completed {
		return finalText
	}
	if finalText != "" {
		return finalText
	}
	return formatSubagentTimeoutSummary(steps, finalText)
}

func (s *SubagentRunner) buildSubagentSystemPrompt(role, agentType string) string {
	name := "explore"
	if agentType == "web_fetch" {
		name = "web_fetch"
	}
	return prompt.Render(s.promptLoader.MustLoad(name), map[string]string{
		"role": role,
	})
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
//	explore/teammate → cyan [sub] prefix once, then cyan body
//
// The [sub] label is printed only on the first delta of a stream. Printing
// it on every OnTextDelta (one token/chunk) produced output like
// `[sub] Code[sub] x[sub]  CLI` and made subagent replies unreadable.
type prefixedSink struct {
	Prefix  string
	color   string
	isLead  bool
	started bool
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
	if !s.started {
		if s.isLead {
			fmt.Print(s.color)
		} else {
			fmt.Printf("%s[sub] ", s.color)
		}
		s.started = true
	}
	fmt.Print(text)
}

func (s *prefixedSink) OnDone() {
	if s.started {
		fmt.Print(utils.Reset)
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
	//
	// The guidance is explicit and actionable so the model does NOT blindly
	// re-read the same file (which wastes rounds): to see the rest, page with
	// read_file offset/limit, or grep for the specific symbol with
	// search_content instead of reading the whole file.
	headLen := t.maxLen - 256
	notice := fmt.Sprintf(
		"\n... (output capped at %d chars; content omitted here) ...\n"+
			"To continue, re-run read_file with an offset past the lines shown above, "+
			"or use search_content to jump directly to the symbol you need. "+
			"Do NOT re-read this file from the start.\n",
		t.maxLen)
	return s[:headLen] + notice + s[len(s)-256:]
}
