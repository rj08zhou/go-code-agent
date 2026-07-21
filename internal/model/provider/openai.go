package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent-refactor/internal/llm"
	"go-code-agent-refactor/internal/model"
	"sort"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
)

type OpenAIProvider struct {
	client openai.Client
}

func NewOpenAI(apiKey, baseURL string) model.Provider {
	opts := []option.RequestOption{}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &OpenAIProvider{client: openai.NewClient(opts...)}
}

func (p *OpenAIProvider) Name() string { return "openai" }

func (p *OpenAIProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	msgs := toOpenAIMessages(params.Messages)
	tools := toOpenAITools(params.Tools)

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(params.Model),
		Messages: msgs,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	if params.MaxTokens > 0 {
		req.MaxTokens = param.NewOpt(int64(params.MaxTokens))
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return nil, err
	}
	return mapOpenAIResponse(resp), nil
}

func (p *OpenAIProvider) Stream(ctx context.Context, params llm.CallParams, sink model.StreamSink) (*llm.StreamResult, error) {
	msgs := toOpenAIMessages(params.Messages)
	tools := toOpenAITools(params.Tools)

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(params.Model),
		Messages: msgs,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: param.NewOpt(true),
		},
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	if params.MaxTokens > 0 {
		req.MaxTokens = param.NewOpt(int64(params.MaxTokens))
	}

	result := &llm.StreamResult{}
	toolCalls := map[int64]*llm.ToolCall{}

	stream := p.client.Chat.Completions.NewStreaming(ctx, req)
	for stream.Next() {
		evt := stream.Current()
		for _, choice := range evt.Choices {
			result.Content += choice.Delta.Content
			// Forward text immediately for real-time UX. DSML markup may
			// appear in output for legacy DeepSeek models that lack native
			// function calling — an acceptable tradeoff vs. complete
			// silence during long tool-call generation.
			if choice.Delta.Content != "" {
				sink.OnTextDelta(choice.Delta.Content)
			}
			for _, tc := range choice.Delta.ToolCalls {
				idx := tc.Index
				if _, ok := toolCalls[idx]; !ok {
					toolCalls[idx] = &llm.ToolCall{}
				}
				if tc.ID != "" {
					toolCalls[idx].ID = tc.ID
				}
				if tc.Function.Name != "" {
					toolCalls[idx].Name = tc.Function.Name
				}
				toolCalls[idx].Arguments += tc.Function.Arguments
			}
			if choice.FinishReason != "" {
				result.FinishReason = string(choice.FinishReason)
			}
		}
		if evt.JSON.Usage.Valid() || evt.Usage.TotalTokens > 0 {
			result.Usage = mapOpenAIUsage(evt.Usage)
		}
	}

	streamErr := stream.Err()
	if clean, dsmlCalls, parsed := parseDSMLToolCalls(result.Content); parsed {
		result.Content = clean
		for _, tc := range dsmlCalls {
			result.ToolCalls = append(result.ToolCalls, tc)
		}
	}
	indices := make([]int64, 0, len(toolCalls))
	for idx := range toolCalls {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	for _, idx := range indices {
		tc := toolCalls[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", idx)
		}
		result.ToolCalls = append(result.ToolCalls, *tc)
	}
	sink.OnDone()
	return result, streamErr
}

func toOpenAIMessages(msgs []llm.Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			out = append(out, openai.SystemMessage(m.Content))
		case llm.RoleUser:
			out = append(out, openai.UserMessage(m.Content))
		case llm.RoleAssistant:
			am := openai.AssistantMessage(m.Content)
			if len(m.ToolCalls) > 0 && am.OfAssistant != nil {
				tcs := make([]openai.ChatCompletionMessageToolCallParam, 0, len(m.ToolCalls))
				for _, tc := range m.ToolCalls {
					tcs = append(tcs, openai.ChatCompletionMessageToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: tc.Arguments,
						},
					})
				}
				am.OfAssistant.ToolCalls = tcs
			}
			out = append(out, am)
		case llm.RoleTool:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		}
	}
	return out
}

func toOpenAITools(defs []llm.ToolDef) []openai.ChatCompletionToolParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolParam, 0, len(defs))
	for _, d := range defs {
		params := openai.FunctionParameters{}
		for k, v := range d.Parameters {
			params[k] = v
		}
		if _, ok := params["type"]; !ok {
			params["type"] = "object"
		}
		tp := openai.ChatCompletionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:       d.Name,
				Parameters: params,
			},
		}
		if d.Description != "" {
			tp.Function.Description = param.NewOpt(d.Description)
		}
		out = append(out, tp)
	}
	return out
}

// mapOpenAIUsage maps standard OpenAI usage and DeepSeek's custom
// prompt_cache_* usage fields into the provider-neutral Usage model.
// DeepSeek is OpenAI-compatible but its cache fields are not part of the
// standard OpenAI CompletionUsage struct, so RawJSON is also inspected.
func mapOpenAIUsage(src openai.CompletionUsage) llm.Usage {
	usage := llm.Usage{
		PromptTokens:     src.PromptTokens,
		CompletionTokens: src.CompletionTokens,
		TotalTokens:      src.TotalTokens,
		CachedReadTokens: src.PromptTokensDetails.CachedTokens,
	}

	var deepSeek struct {
		PromptCacheHitTokens  *int64 `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens *int64 `json:"prompt_cache_miss_tokens"`
	}
	if raw := src.RawJSON(); raw != "" {
		if err := json.Unmarshal([]byte(raw), &deepSeek); err == nil {
			if deepSeek.PromptCacheHitTokens != nil {
				usage.CachedReadTokens = *deepSeek.PromptCacheHitTokens
			}
			if deepSeek.PromptCacheMissTokens != nil {
				usage.CacheMissTokens = *deepSeek.PromptCacheMissTokens
			}
		}
	}
	return usage
}

func mapOpenAIResponse(resp *openai.ChatCompletion) *llm.Completion {
	if len(resp.Choices) == 0 {
		return &llm.Completion{FinishReason: "stop"}
	}
	choice := resp.Choices[0]
	content, dsmlCalls, _ := parseDSMLToolCalls(choice.Message.Content)
	c := &llm.Completion{
		Content:      content,
		FinishReason: string(choice.FinishReason),
		Usage:        mapOpenAIUsage(resp.Usage),
	}
	for _, tc := range choice.Message.ToolCalls {
		c.ToolCalls = append(c.ToolCalls, llm.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	c.ToolCalls = append(c.ToolCalls, dsmlCalls...)
	return c
}

// ParseArgs unmarshals raw JSON args into a struct, returning an error message on failure.
func ParseArgs(raw json.RawMessage, v any) string {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Sprintf("invalid arguments: %v", err)
	}
	return ""
}

func OpenAIRetriable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, h := range []string{"rate limit", "too many requests", "429", "500", "502", "503", "504", "timeout"} {
		if strings.Contains(msg, h) {
			return true
		}
	}
	return false
}
