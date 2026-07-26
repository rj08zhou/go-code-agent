package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/utils"
	"strings"
	"sync"
)

// JudgeVerdict is the structured output from the Judge LLM.
type JudgeVerdict struct {
	Approved    bool     `json:"approved"`
	Score       int      `json:"score"` // 1-10
	Issues      []string `json:"issues"`
	Suggestions []string `json:"suggestions"`
	ShouldRetry bool     `json:"should_retry"`
	Reason      string   `json:"reason"`
}

// JudgeToolResult captures a single tool execution for review.
type JudgeToolResult struct {
	ToolName string
	Args     string
	Status   tool.Status
	Output   string
	Reason   string
}

// Judge evaluates agent work using a secondary LLM call.
type Judge struct {
	enabled      bool
	model        string
	minScore     int
	maxHistory   int
	promptLoader *prompt.Loader
	gateway      *model.Gateway
	mu           sync.RWMutex
}

func NewJudge(enabled bool, model string, minScore int, pl *prompt.Loader, gw *model.Gateway) *Judge {
	if minScore <= 0 {
		minScore = 7
	}
	return &Judge{
		enabled:      enabled,
		model:        model,
		minScore:     minScore,
		maxHistory:   12,
		promptLoader: pl,
		gateway:      gw,
	}
}

func (j *Judge) IsEnabled() bool {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.enabled
}

func (j *Judge) SetEnabled(v bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.enabled = v
}

// Verify asks the judge LLM to evaluate agent actions.
func (j *Judge) Verify(ctx context.Context, originalTask string, conversation []llm.Message, toolResults []JudgeToolResult, fallbackModel string) (*JudgeVerdict, error) {
	if !j.IsEnabled() {
		return &JudgeVerdict{Approved: true, Score: 10, Reason: "judge disabled"}, nil
	}

	jPrompt := j.buildPrompt(originalTask, conversation, toolResults)

	callModel := j.model
	if callModel == "" {
		callModel = fallbackModel
	}

	comp, err := j.gateway.Call(ctx, "judge", llm.CallParams{
		Model:       callModel,
		Messages:    []llm.Message{llm.UserMessage(jPrompt)},
		Temperature: 0.0,
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

	if verdict.Score < j.minScore {
		verdict.ShouldRetry = true
		verdict.Approved = false
	}
	return verdict, nil
}

func (j *Judge) buildPrompt(originalTask string, conversation []llm.Message, toolResults []JudgeToolResult) string {
	var convo strings.Builder
	start := 0
	if len(conversation) > j.maxHistory {
		start = len(conversation) - j.maxHistory
	}
	for i := start; i < len(conversation); i++ {
		msg := conversation[i]
		if msg.Role == llm.RoleSystem {
			continue
		}
		if len(msg.ToolCalls) > 0 {
			fmt.Fprintf(&convo, "[%s calls:", msg.Role)
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&convo, " %s(%s)", tc.Name, utils.Truncate(tc.Arguments, 120))
			}
			convo.WriteString("]\n")
		}
		if strings.TrimSpace(msg.Content) != "" {
			fmt.Fprintf(&convo, "[%s]: %s\n", msg.Role, utils.Truncate(msg.Content, 600))
		}
	}

	toolResultsBlock := ""
	if len(toolResults) > 0 {
		var tr strings.Builder
		tr.WriteString("<tool_results>\n")
		for _, t := range toolResults {
			fmt.Fprintf(&tr, "- [%s] %s(%s) -> %s\n",
				t.Status, t.ToolName, utils.Truncate(t.Args, 120), utils.Truncate(t.Output, 400))
			if t.Reason != "" {
				fmt.Fprintf(&tr, "  reason: %s\n", utils.Truncate(t.Reason, 240))
			}
		}
		tr.WriteString("</tool_results>\n\n")
		toolResultsBlock = tr.String()
	}

	tmpl := j.promptLoader.Load("judge_system")
	return prompt.Render(tmpl, map[string]string{
		"min_score":           fmt.Sprintf("%d", j.minScore),
		"original_task":       utils.Truncate(originalTask, 2000),
		"recent_conversation": convo.String(),
		"tool_results":        toolResultsBlock,
	})
}

func parseJudgeResponse(content string) (*JudgeVerdict, error) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found: %s", utils.Truncate(content, 200))
	}
	raw := content[start : end+1]
	var v JudgeVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("unmarshal failed: %w", err)
	}
	if v.Score < 1 {
		v.Score = 1
	}
	if v.Score > 10 {
		v.Score = 10
	}
	return &v, nil
}

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
