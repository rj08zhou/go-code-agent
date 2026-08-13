package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/gateway"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/utils"
	"io"
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

func judgeStructuredOutput() *llm.StructuredOutput {
	return &llm.StructuredOutput{
		Name:        "judge_verdict",
		Description: "Quality review verdict for the coding agent's completed work.",
		Schema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"approved":     map[string]any{"type": "boolean"},
				"score":        map[string]any{"type": "integer", "minimum": 1, "maximum": 10},
				"issues":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"suggestions":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"should_retry": map[string]any{"type": "boolean"},
				"reason":       map[string]any{"type": "string"},
			},
			"required": []string{"approved", "score", "issues", "suggestions", "should_retry", "reason"},
		},
	}
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
	gateway      *gateway.Gateway
	mu           sync.RWMutex
}

func NewJudge(enabled bool, model string, minScore int, pl *prompt.Loader, gw *gateway.Gateway) *Judge {
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
		Model:            callModel,
		Messages:         []llm.Message{llm.UserMessage(jPrompt)},
		Temperature:      0.0,
		StructuredOutput: judgeStructuredOutput(),
	})
	if err != nil {
		return permissiveVerdict("judge LLM error: " + err.Error()), err
	}
	if comp == nil || strings.TrimSpace(comp.Content) == "" {
		err := fmt.Errorf("judge returned empty structured response")
		return permissiveVerdict(err.Error()), err
	}

	verdict, perr := parseJudgeResponse(comp.Content)
	if perr != nil {
		return permissiveVerdict("judge schema violation: " + perr.Error()), perr
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

	tmpl := j.promptLoader.MustLoad("judge_system")
	return prompt.Render(tmpl, map[string]string{
		"min_score":           fmt.Sprintf("%d", j.minScore),
		"original_task":       utils.Truncate(originalTask, 2000),
		"recent_conversation": convo.String(),
		"tool_results":        toolResultsBlock,
	})
}

func parseJudgeResponse(content string) (*JudgeVerdict, error) {
	// Pointer fields distinguish a missing required property from its valid zero
	// value (notably false for approved/should_retry).
	var wire struct {
		Approved    *bool     `json:"approved"`
		Score       *int      `json:"score"`
		Issues      *[]string `json:"issues"`
		Suggestions *[]string `json:"suggestions"`
		ShouldRetry *bool     `json:"should_retry"`
		Reason      *string   `json:"reason"`
	}
	dec := json.NewDecoder(strings.NewReader(content))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode structured verdict: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	if wire.Approved == nil || wire.Score == nil || wire.Issues == nil ||
		wire.Suggestions == nil || wire.ShouldRetry == nil || wire.Reason == nil {
		return nil, fmt.Errorf("structured verdict omitted one or more required fields")
	}
	if *wire.Score < 1 || *wire.Score > 10 {
		return nil, fmt.Errorf("structured verdict score %d is outside 1..10", *wire.Score)
	}
	return &JudgeVerdict{
		Approved:    *wire.Approved,
		Score:       *wire.Score,
		Issues:      *wire.Issues,
		Suggestions: *wire.Suggestions,
		ShouldRetry: *wire.ShouldRetry,
		Reason:      *wire.Reason,
	}, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("structured verdict contains trailing JSON value")
		}
		return fmt.Errorf("structured verdict contains trailing content: %w", err)
	}
	return nil
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
