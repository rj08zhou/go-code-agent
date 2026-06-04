package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/prompt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// LLM-as-Judge verifier.
//
// A secondary LLM call evaluates whether the agent's actions match the
// user's intent. Triggered after task completion. Uses a separate
// (usually cheaper) model. Disabled by default; opt-in via JUDGE_ENABLED.
//
// The judge is configured entirely through JUDGE_* environment variables
// (see judgeConfigFromEnv + llm.JudgeProvider), so its model, endpoint
// and credentials are set through one consistent mechanism rather than a
// mix of CLI flags and env vars.

// JudgeVerdict is the structured output produced by the Judge LLM.
type JudgeVerdict struct {
	Approved    bool     `json:"approved"`     // overall pass/fail
	Score       int      `json:"score"`        // 1-10
	Issues      []string `json:"issues"`       // concrete problems observed
	Suggestions []string `json:"suggestions"`  // how to fix / improve
	ShouldRetry bool     `json:"should_retry"` // if true, force another round
	Reason      string   `json:"reason"`       // brief explanation
}

// JudgeToolResult captures a single tool execution for the judge's review.
type JudgeToolResult struct {
	ToolName string
	Args     string
	Output   string
	Failed   bool
}

// Judge evaluates the agent's recent work using a secondary LLM call.
type Judge struct {
	enabled    bool
	model      string
	minScore   int
	maxHistory int
	mu         sync.RWMutex
}

// NewJudge constructs a Judge. enabled=false makes Verify a no-op pass.
func NewJudge(enabled bool, model string, minScore int) *Judge {
	if minScore <= 0 {
		minScore = 7
	}
	return &Judge{
		enabled:    enabled,
		model:      model,
		minScore:   minScore,
		maxHistory: 12, // last 12 messages is usually enough context
	}
}

// judgeConfigFromEnv reads the judge's entire configuration from the
// JUDGE_* environment variables, mirroring the JUDGE_PROVIDER/API_KEY/
// BASE_URL routing vars consumed by llm.JudgeProvider so the judge is
// configured through one consistent mechanism:
//
//	JUDGE_ENABLED    turn the judge on    (1 | true | yes | on)
//	JUDGE_MODEL      judge model id       (empty = reuse the main model)
//	JUDGE_MIN_SCORE  retry threshold 1-10 (default infra.JudgeMinScore)
func judgeConfigFromEnv() (enabled bool, model string, minScore int) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("JUDGE_ENABLED"))) {
	case "1", "true", "yes", "on":
		enabled = true
	}
	model = strings.TrimSpace(os.Getenv("JUDGE_MODEL"))
	minScore = infra.JudgeMinScore
	if v := strings.TrimSpace(os.Getenv("JUDGE_MIN_SCORE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			minScore = n
		}
	}
	return enabled, model, minScore
}

// IsEnabled reports whether the judge is active.
func (j *Judge) IsEnabled() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.enabled
}

// SetEnabled toggles the judge at runtime (e.g., via /judge REPL command).
func (j *Judge) SetEnabled(v bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.enabled = v
}

// Verify asks the judge LLM to evaluate the agent's recent actions.
// On internal error, returns a permissive default (never blocks progress).
func (j *Judge) Verify(ctx context.Context, originalTask string, conversation []llm.Message, toolResults []JudgeToolResult) (*JudgeVerdict, error) {
	if !j.IsEnabled() {
		return &JudgeVerdict{Approved: true, Score: 10, Reason: "judge disabled"}, nil
	}

	prompt := j.buildPrompt(originalTask, conversation, toolResults)

	// Pick model: explicit judge model > main model as fallback.
	callModel := j.model
	if callModel == "" {
		callModel = model
	}

	// Route to the backend that serves the judge. The judge is designed
	// to run a *different* (cheaper) model than the main agent;
	// JudgeProvider honours the JUDGE_PROVIDER / JUDGE_API_KEY /
	// JUDGE_BASE_URL env vars so it can even live behind a separate
	// endpoint, falling back to the main model's provider otherwise.
	comp, err := llm.NewClient(llm.JudgeProvider(callModel)).CallWithRetry(ctx, "judge", llm.CallParams{
		Model:       callModel,
		Messages:    []llm.Message{llm.SystemMessage(prompt)},
		Temperature: 0.0, // deterministic judgment
	})
	if err != nil {
		return permissiveVerdict("judge LLM error: " + err.Error()), err
	}
	if comp == nil || comp.Content == "" {
		return permissiveVerdict("judge returned empty response"), nil
	}

	verdict, perr := parseJudgeResponse(comp.Content)
	if perr != nil {
		return permissiveVerdict("judge parse error: " + perr.Error()), perr
	}

	// Enforce minScore: any sub-threshold verdict gets ShouldRetry=true.
	if verdict.Score < j.minScore {
		verdict.ShouldRetry = true
		if verdict.Approved {
			verdict.Approved = false
		}
	}

	return verdict, nil
}

// buildPrompt assembles the judge's evaluation prompt.
func (j *Judge) buildPrompt(originalTask string, conversation []llm.Message, toolResults []JudgeToolResult) string {
	// Conversation tail: focus on what the agent actually did recently.
	var convo strings.Builder
	start := 0
	if len(conversation) > j.maxHistory {
		start = len(conversation) - j.maxHistory
	}
	for i := start; i < len(conversation); i++ {
		msg := conversation[i]
		// Skip system messages; the judge doesn't need the agent's
		// (huge) own system prompt to evaluate outcomes.
		if msg.Role == llm.RoleSystem {
			continue
		}
		if len(msg.ToolCalls) > 0 {
			fmt.Fprintf(&convo, "[%s calls:", msg.Role)
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&convo, " %s(%s)", tc.Name, truncate(tc.Arguments, 120))
			}
			convo.WriteString("]\n")
		}
		if strings.TrimSpace(msg.Content) != "" {
			fmt.Fprintf(&convo, "[%s]: %s\n", msg.Role, truncate(msg.Content, 600))
		}
	}

	// Tool results: omitted entirely when the round had none.
	toolResultsBlock := ""
	if len(toolResults) > 0 {
		var tr strings.Builder
		tr.WriteString("<tool_results>\n")
		for _, t := range toolResults {
			status := "ok"
			if t.Failed {
				status = "FAILED"
			}
			fmt.Fprintf(&tr, "- [%s] %s(%s) -> %s\n",
				status, t.ToolName, truncate(t.Args, 120), truncate(t.Output, 400))
		}
		tr.WriteString("</tool_results>\n\n")
		toolResultsBlock = tr.String()
	}

	tmpl := app.PromptLoader.Load("judge_system")
	return prompt.Render(tmpl, map[string]string{
		"min_score":           fmt.Sprintf("%d", j.minScore),
		"original_task":       truncate(originalTask, 2000),
		"recent_conversation": convo.String(),
		"tool_results":        toolResultsBlock,
	})
}

// parseJudgeResponse extracts JSON verdict from the LLM's raw output.
func parseJudgeResponse(content string) (*JudgeVerdict, error) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in judge response: %s", truncate(content, 200))
	}
	raw := content[start : end+1]

	var v JudgeVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("unmarshal failed: %w, raw=%s", err, truncate(raw, 200))
	}

	// Normalize score to [1, 10]; defensive against bogus LLM output.
	if v.Score < 1 {
		v.Score = 1
	}
	if v.Score > 10 {
		v.Score = 10
	}
	return &v, nil
}

// permissiveVerdict produces a pass-through verdict for infrastructure failures.
func permissiveVerdict(reason string) *JudgeVerdict {
	return &JudgeVerdict{
		Approved:    true,
		Score:       5,
		Reason:      reason,
		ShouldRetry: false,
	}
}

// FormatFeedback renders a verdict into a <verification-failed> block.
func (v *JudgeVerdict) FormatFeedback() string {
	var sb strings.Builder
	sb.WriteString("<verification-failed>\n")
	fmt.Fprintf(&sb, "Judge score: %d/10\n", v.Score)
	if v.Reason != "" {
		fmt.Fprintf(&sb, "Reason: %s\n", v.Reason)
	}
	if len(v.Issues) > 0 {
		sb.WriteString("Issues:\n")
		for _, issue := range v.Issues {
			fmt.Fprintf(&sb, "  - %s\n", issue)
		}
	}
	if len(v.Suggestions) > 0 {
		sb.WriteString("Suggestions:\n")
		for _, sug := range v.Suggestions {
			fmt.Fprintf(&sb, "  - %s\n", sug)
		}
	}
	sb.WriteString("</verification-failed>\n")
	sb.WriteString("Address the issues above and continue. Do not declare the task done again until they are resolved.")
	return sb.String()
}

// Global singleton; constructed in main() after CLI flags are parsed.
var globalJudge = NewJudge(false, "", infra.JudgeMinScore)
