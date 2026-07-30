package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"sort"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
)

type OpenAIProvider struct {
	client     openai.Client
	instanceID string
}

func NewOpenAI(apiKey, baseURL string) model.Provider {
	opts := []option.RequestOption{}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &OpenAIProvider{
		client:     openai.NewClient(opts...),
		instanceID: model.StableProviderInstanceID("openai", baseURL),
	}
}

func (p *OpenAIProvider) Name() string       { return "openai" }
func (p *OpenAIProvider) InstanceID() string { return p.instanceID }

func (p *OpenAIProvider) Capabilities() model.ProviderCapabilities {
	return model.ProviderCapabilities{
		StructuredOutput: true,
		ToolCalling:      true,
		Streaming:        true,
		Reasoning:        true,
	}
}

const openAIReasoningContentKind = "openai.chat.reasoning_content"

var openAIReasoningEfforts = map[string]struct{}{
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
	"max":     {},
}

// setOpenAIReasoningEffort validates and maps the provider-neutral effort
// hint to the Chat Completions reasoning_effort extension used by OpenAI and
// compatible APIs. An empty effort leaves the provider/model default intact.
func setOpenAIReasoningEffort(req *openai.ChatCompletionNewParams, reasoning *llm.ReasoningRequest) error {
	if reasoning == nil || !reasoning.Enabled {
		return nil
	}
	effort := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	if effort == "" {
		return nil
	}
	if _, ok := openAIReasoningEfforts[effort]; !ok {
		return fmt.Errorf("unsupported OpenAI reasoning effort %q", reasoning.Effort)
	}
	req.SetExtraFields(map[string]any{"reasoning_effort": effort})
	return nil
}

func toOpenAIProviderError(err error) error {
	if err == nil {
		return nil
	}
	statusCode := 0
	code := ""
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		statusCode = apiErr.StatusCode
		code = apiErr.Code
		if code == "" {
			code = apiErr.Type
		}
	}
	return model.NewProviderError("openai", statusCode, code, err)
}

func toOpenAIResponseFormat(output *llm.StructuredOutput) (openai.ChatCompletionNewParamsResponseFormatUnion, bool) {
	if output == nil {
		return openai.ChatCompletionNewParamsResponseFormatUnion{}, false
	}
	schema := openai.ResponseFormatJSONSchemaJSONSchemaParam{
		Name:        output.Name,
		Description: param.NewOpt(output.Description),
		Schema:      output.Schema,
		Strict:      param.NewOpt(true),
	}
	return openai.ChatCompletionNewParamsResponseFormatUnion{
		OfJSONSchema: &openai.ResponseFormatJSONSchemaParam{JSONSchema: schema},
	}, true
}

func (p *OpenAIProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	msgs := toOpenAIMessages(params.Messages, p.instanceID, params.Model)
	tools := toOpenAITools(params.Tools)

	req := openai.ChatCompletionNewParams{
		Model:    openai.ChatModel(params.Model),
		Messages: msgs,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	if responseFormat, ok := toOpenAIResponseFormat(params.StructuredOutput); ok {
		req.ResponseFormat = responseFormat
	}
	if params.MaxTokens > 0 {
		req.MaxTokens = param.NewOpt(int64(params.MaxTokens))
	}
	if err := setOpenAIReasoningEffort(&req, params.Reasoning); err != nil {
		return nil, err
	}

	resp, err := p.client.Chat.Completions.New(ctx, req)
	if err != nil {
		return nil, toOpenAIProviderError(err)
	}
	return mapOpenAIResponse(resp, p.instanceID, params.Model), nil
}

func (p *OpenAIProvider) Stream(ctx context.Context, params llm.CallParams, sink model.StreamSink) (*llm.StreamResult, error) {
	msgs := toOpenAIMessages(params.Messages, p.instanceID, params.Model)
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
	if responseFormat, ok := toOpenAIResponseFormat(params.StructuredOutput); ok {
		req.ResponseFormat = responseFormat
	}
	if params.MaxTokens > 0 {
		req.MaxTokens = param.NewOpt(int64(params.MaxTokens))
	}
	if err := setOpenAIReasoningEffort(&req, params.Reasoning); err != nil {
		return nil, err
	}

	accum := newOpenAIStreamAccum(p.instanceID, params.Model)
	stream := p.client.Chat.Completions.NewStreaming(ctx, req)
	for stream.Next() {
		accum.apply(stream.Current(), sink)
	}
	streamErr := toOpenAIProviderError(stream.Err())
	return accum.finalize(sink), streamErr
}

// openAIStreamAccum folds ChatCompletionChunk deltas into a StreamResult.
// Extracted so golden tests can feed recorded chunk JSON without HTTP.
type openAIStreamAccum struct {
	result             *llm.StreamResult
	toolCalls          map[int64]*llm.ToolCall
	reasoningContent   strings.Builder
	reasoningSummary   strings.Builder
	providerInstanceID string
	model              string
}

func newOpenAIStreamAccum(providerInstanceID, modelID string) *openAIStreamAccum {
	return &openAIStreamAccum{
		result:             &llm.StreamResult{},
		toolCalls:          map[int64]*llm.ToolCall{},
		providerInstanceID: providerInstanceID,
		model:              modelID,
	}
}

// parseOpenAIReasoningFields reads compatible response extensions without
// depending on a specific SDK release. reasoning_content is private model
// rationale and therefore becomes opaque state, never a visible Summary.
func parseOpenAIReasoningFields(raw string) (content, summary string) {
	if strings.TrimSpace(raw) == "" {
		return "", ""
	}
	var wire struct {
		ReasoningContent json.RawMessage `json:"reasoning_content"`
		ReasoningSummary json.RawMessage `json:"reasoning_summary"`
	}
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return "", ""
	}
	_ = json.Unmarshal(wire.ReasoningContent, &content)
	_ = json.Unmarshal(wire.ReasoningSummary, &summary)
	return content, summary
}

func newOpenAIReasoning(content, summary, providerInstanceID, modelID string, keepContent bool) *llm.Reasoning {
	if summary == "" && (!keepContent || content == "") {
		return nil
	}
	reasoning := &llm.Reasoning{Summary: summary}
	if keepContent && content != "" && providerInstanceID != "" && modelID != "" {
		payload, err := json.Marshal(content)
		if err == nil {
			reasoning.State = &llm.ReasoningState{
				Provider: providerInstanceID,
				Model:    modelID,
				Kind:     openAIReasoningContentKind,
				Payload:  payload,
			}
		}
	}
	if reasoning.Summary == "" && reasoning.State == nil {
		return nil
	}
	return reasoning
}

func (a *openAIStreamAccum) apply(evt openai.ChatCompletionChunk, sink model.StreamSink) {
	for _, choice := range evt.Choices {
		// Compatible APIs stream private rationale separately from answer text.
		// Accumulate it for tool-loop continuation, but never forward it to the
		// text sink (which is user-visible).
		reasoningContent, reasoningSummary := parseOpenAIReasoningFields(choice.Delta.RawJSON())
		a.reasoningContent.WriteString(reasoningContent)
		a.reasoningSummary.WriteString(reasoningSummary)
		a.result.Content += choice.Delta.Content
		// Forward text immediately for real-time UX. DSML markup may
		// appear in output for legacy DeepSeek models that lack native
		// function calling — an acceptable tradeoff vs. complete
		// silence during long tool-call generation.
		if choice.Delta.Content != "" && sink != nil {
			sink.OnTextDelta(choice.Delta.Content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			idx := tc.Index
			if _, ok := a.toolCalls[idx]; !ok {
				a.toolCalls[idx] = &llm.ToolCall{}
			}
			if tc.ID != "" {
				a.toolCalls[idx].ID = tc.ID
			}
			if tc.Function.Name != "" {
				a.toolCalls[idx].Name = tc.Function.Name
			}
			a.toolCalls[idx].Arguments += tc.Function.Arguments
		}
		if choice.FinishReason != "" {
			a.result.FinishReason = string(choice.FinishReason)
		}
	}
	if evt.JSON.Usage.Valid() || evt.Usage.TotalTokens > 0 {
		a.result.Usage = mapOpenAIUsage(evt.Usage)
	}
}

func (a *openAIStreamAccum) finalize(sink model.StreamSink) *llm.StreamResult {
	if clean, dsmlCalls, parsed := parseDSMLToolCalls(a.result.Content); parsed {
		a.result.Content = clean
		a.result.ToolCalls = append(a.result.ToolCalls, dsmlCalls...)
	}
	indices := make([]int64, 0, len(a.toolCalls))
	for idx := range a.toolCalls {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	for _, idx := range indices {
		tc := a.toolCalls[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", idx)
		}
		a.result.ToolCalls = append(a.result.ToolCalls, *tc)
	}
	// Raw reasoning_content is retained only when needed to continue a tool
	// loop. A final answer with no tool call discards it immediately.
	a.result.Reasoning = newOpenAIReasoning(
		a.reasoningContent.String(), a.reasoningSummary.String(),
		a.providerInstanceID, a.model, len(a.result.ToolCalls) > 0,
	)
	if sink != nil {
		sink.OnDone()
	}
	return a.result
}

func openAIReasoningContentForRequest(
	reasoning *llm.Reasoning,
	providerInstanceID, modelID string,
) (string, bool) {
	if reasoning == nil || reasoning.State == nil ||
		!reasoning.State.Compatible(providerInstanceID, modelID) ||
		reasoning.State.Kind != openAIReasoningContentKind {
		return "", false
	}
	var content string
	if err := json.Unmarshal(reasoning.State.Payload, &content); err != nil || content == "" {
		return "", false
	}
	return content, true
}

func toOpenAIMessages(msgs []llm.Message, providerInstanceID, modelID string) []openai.ChatCompletionMessageParamUnion {
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
				if reasoningContent, ok := openAIReasoningContentForRequest(
					m.Reasoning, providerInstanceID, modelID,
				); ok {
					// Some compatible reasoning APIs require the complete assistant
					// reasoning_content beside tool_calls on the next request.
					am.OfAssistant.SetExtraFields(map[string]any{
						"reasoning_content": reasoningContent,
					})
				}
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

	var extensions struct {
		PromptCacheHitTokens  *int64 `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens *int64 `json:"prompt_cache_miss_tokens"`
		ReasoningTokens       *int64 `json:"reasoning_tokens"`
		CompletionDetails     struct {
			ReasoningTokens *int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if raw := src.RawJSON(); raw != "" {
		if err := json.Unmarshal([]byte(raw), &extensions); err == nil {
			if extensions.PromptCacheHitTokens != nil {
				usage.CachedReadTokens = *extensions.PromptCacheHitTokens
			}
			if extensions.PromptCacheMissTokens != nil {
				usage.CacheMissTokens = *extensions.PromptCacheMissTokens
			}
			if extensions.CompletionDetails.ReasoningTokens != nil {
				usage.ReasoningTokens = *extensions.CompletionDetails.ReasoningTokens
			} else if extensions.ReasoningTokens != nil {
				// A few compatible APIs report this at usage top level.
				usage.ReasoningTokens = *extensions.ReasoningTokens
			}
		}
	}
	return usage
}

func mapOpenAIResponse(resp *openai.ChatCompletion, providerInstanceID, modelID string) *llm.Completion {
	if len(resp.Choices) == 0 {
		return &llm.Completion{FinishReason: "stop", Usage: mapOpenAIUsage(resp.Usage)}
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
	reasoningContent, reasoningSummary := parseOpenAIReasoningFields(choice.Message.RawJSON())
	c.Reasoning = newOpenAIReasoning(
		reasoningContent, reasoningSummary, providerInstanceID, modelID, len(c.ToolCalls) > 0,
	)
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
