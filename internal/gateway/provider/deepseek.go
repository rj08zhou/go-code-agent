package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go-code-agent/internal/gateway"
	"go-code-agent/internal/llm"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
)

const (
	deepSeekDefaultBaseURL     = "https://api.deepseek.com"
	deepSeekReasoningItemsKind = "deepseek.responses.output_items"
)

// DeepSeek reasoning.effort values accepted by the Responses API.
// "minimal" is accepted as an alias of "low" so the shared REASONING_EFFORT
// knob stays usable without a DeepSeek-specific setting.
var deepSeekReasoningEfforts = map[string]shared.ReasoningEffort{
	"low":     shared.ReasoningEffortLow,
	"medium":  shared.ReasoningEffortMedium,
	"high":    shared.ReasoningEffortHigh,
	"minimal": shared.ReasoningEffortLow,
}

type DeepSeekProvider struct {
	client     openai.Client
	instanceID string
}

func NewDeepSeek(apiKey, baseURL string) gateway.Provider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = deepSeekDefaultBaseURL
	}
	return &DeepSeekProvider{
		// Always set key and base URL so OPENAI_* env defaults from openai-go
		// cannot silently redirect a DeepSeek client at OpenAI.
		client: openai.NewClient(
			option.WithAPIKey(apiKey),
			option.WithBaseURL(baseURL),
		),
		instanceID: gateway.StableProviderInstanceID("deepseek", baseURL),
	}
}

func (p *DeepSeekProvider) Name() string       { return "deepseek" }
func (p *DeepSeekProvider) InstanceID() string { return p.instanceID }

func (p *DeepSeekProvider) Capabilities() gateway.ProviderCapabilities {
	return gateway.ProviderCapabilities{
		StructuredOutput: true,
		ToolCalling:      true,
		Streaming:        true,
		Reasoning:        true,
	}
}

func toDeepSeekProviderError(err error) error {
	if err == nil {
		return nil
	}
	var providerErr *gateway.ProviderError
	if errors.As(err, &providerErr) {
		return err
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
	return gateway.NewProviderError("deepseek", statusCode, code, err)
}

func (p *DeepSeekProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	req, err := toDeepSeekRequest(params, p.instanceID)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Responses.New(ctx, req)
	if err != nil {
		return nil, toDeepSeekProviderError(err)
	}
	return mapDeepSeekResponse(resp, p.instanceID, params.Model)
}

func (p *DeepSeekProvider) Stream(ctx context.Context, params llm.CallParams, sink gateway.StreamSink) (*llm.StreamResult, error) {
	req, err := toDeepSeekRequest(params, p.instanceID)
	if err != nil {
		return nil, err
	}

	accum := newDeepSeekStreamAccum(p.instanceID, params.Model)
	stream := p.client.Responses.NewStreaming(ctx, req)
	for stream.Next() {
		if applyErr := accum.apply(stream.Current(), sink); applyErr != nil {
			_ = stream.Close()
			return accum.finalize(sink), toDeepSeekProviderError(applyErr)
		}
	}
	streamErr := toDeepSeekProviderError(stream.Err())
	return accum.finalize(sink), streamErr
}

func toDeepSeekRequest(params llm.CallParams, providerInstanceID string) (responses.ResponseNewParams, error) {
	instructions, input := toDeepSeekInput(params.Messages, providerInstanceID, params.Model)
	req := responses.ResponseNewParams{
		Model: shared.ResponsesModel(params.Model),
	}
	if instructions != "" {
		req.Instructions = param.NewOpt(instructions)
	}
	if len(input) > 0 {
		req.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: input}
	}
	if tools := toDeepSeekTools(params.Tools); len(tools) > 0 {
		req.Tools = tools
	}
	if text, ok := toDeepSeekTextFormat(params.StructuredOutput); ok {
		req.Text = text
	}
	if params.MaxTokens > 0 {
		req.MaxOutputTokens = param.NewOpt(int64(params.MaxTokens))
	}
	if params.Temperature != 0 {
		req.Temperature = param.NewOpt(params.Temperature)
	}
	if err := setDeepSeekReasoning(&req, params.Reasoning); err != nil {
		return responses.ResponseNewParams{}, err
	}
	return req, nil
}

func setDeepSeekReasoning(req *responses.ResponseNewParams, reasoning *llm.ReasoningRequest) error {
	if reasoning == nil || !reasoning.Enabled {
		return nil
	}
	effort := strings.ToLower(strings.TrimSpace(reasoning.Effort))
	if effort == "" {
		return nil
	}
	mapped, ok := deepSeekReasoningEfforts[effort]
	if !ok {
		return fmt.Errorf("unsupported DeepSeek reasoning effort %q", reasoning.Effort)
	}
	req.Reasoning = shared.ReasoningParam{Effort: mapped}
	return nil
}

func toDeepSeekTextFormat(output *llm.StructuredOutput) (responses.ResponseTextConfigParam, bool) {
	if output == nil {
		return responses.ResponseTextConfigParam{}, false
	}
	schema := responses.ResponseFormatTextJSONSchemaConfigParam{
		Name:   output.Name,
		Schema: output.Schema,
		Strict: param.NewOpt(true),
	}
	if output.Description != "" {
		schema.Description = param.NewOpt(output.Description)
	}
	return responses.ResponseTextConfigParam{
		Format: responses.ResponseFormatTextConfigUnionParam{
			OfJSONSchema: &schema,
		},
	}, true
}

func toDeepSeekTools(defs []llm.ToolDef) []responses.ToolUnionParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]responses.ToolUnionParam, 0, len(defs))
	for _, d := range defs {
		parameters := map[string]any{}
		for k, v := range d.Parameters {
			parameters[k] = v
		}
		if _, ok := parameters["type"]; !ok {
			parameters["type"] = "object"
		}
		fn := responses.FunctionToolParam{
			Name:       d.Name,
			Parameters: parameters,
		}
		if d.Description != "" {
			fn.Description = param.NewOpt(d.Description)
		}
		out = append(out, responses.ToolUnionParam{OfFunction: &fn})
	}
	return out
}

func toDeepSeekInput(msgs []llm.Message, providerInstanceID, modelID string) (instructions string, input responses.ResponseInputParam) {
	systemParts := make([]string, 0, 1)
	i := 0
	for i < len(msgs) && msgs[i].Role == llm.RoleSystem {
		if strings.TrimSpace(msgs[i].Content) != "" {
			systemParts = append(systemParts, msgs[i].Content)
		}
		i++
	}
	instructions = strings.Join(systemParts, "\n\n")

	out := make(responses.ResponseInputParam, 0, len(msgs)-i)
	for _, m := range msgs[i:] {
		switch m.Role {
		case llm.RoleSystem:
			if m.Content != "" {
				out = append(out, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleSystem))
			}
		case llm.RoleUser:
			out = append(out, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleUser))
		case llm.RoleAssistant:
			out = append(out, deepSeekAssistantInput(m, providerInstanceID, modelID)...)
		case llm.RoleTool:
			out = append(out, responses.ResponseInputItemParamOfFunctionCallOutput(m.ToolCallID, m.Content))
		}
	}
	return instructions, out
}

func deepSeekAssistantInput(m llm.Message, providerInstanceID, modelID string) []responses.ResponseInputItemUnionParam {
	out := make([]responses.ResponseInputItemUnionParam, 0, 1+len(m.ToolCalls))
	if m.Content != "" {
		out = append(out, responses.ResponseInputItemParamOfMessage(m.Content, responses.EasyInputMessageRoleAssistant))
	}
	if replay := deepSeekReplayItems(m.Reasoning, providerInstanceID, modelID); len(replay) > 0 {
		out = append(out, replay...)
		return out
	}
	for _, tc := range m.ToolCalls {
		out = append(out, responses.ResponseInputItemParamOfFunctionCall(tc.Arguments, tc.ID, tc.Name))
	}
	return out
}

func deepSeekReplayItems(reasoning *llm.Reasoning, providerInstanceID, modelID string) []responses.ResponseInputItemUnionParam {
	if reasoning == nil || reasoning.State == nil ||
		!reasoning.State.Compatible(providerInstanceID, modelID) ||
		reasoning.State.Kind != deepSeekReasoningItemsKind {
		return nil
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(reasoning.State.Payload, &rawItems); err != nil || len(rawItems) == 0 {
		return nil
	}
	out := make([]responses.ResponseInputItemUnionParam, 0, len(rawItems))
	for _, raw := range rawItems {
		item, ok := deepSeekInputItemFromRaw(raw)
		if !ok {
			return nil
		}
		out = append(out, item)
	}
	return out
}

func deepSeekInputItemFromRaw(raw json.RawMessage) (responses.ResponseInputItemUnionParam, bool) {
	var meta struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Content   json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return responses.ResponseInputItemUnionParam{}, false
	}
	switch meta.Type {
	case "reasoning":
		item := responses.ResponseReasoningItemParam{ID: meta.ID}
		if len(meta.Content) > 0 {
			item.SetExtraFields(map[string]any{"content": json.RawMessage(meta.Content)})
		}
		return responses.ResponseInputItemUnionParam{OfReasoning: &item}, true
	case "function_call":
		return responses.ResponseInputItemParamOfFunctionCall(meta.Arguments, meta.CallID, meta.Name), true
	case "web_search_call":
		return param.Override[responses.ResponseInputItemUnionParam](json.RawMessage(raw)), true
	default:
		return responses.ResponseInputItemUnionParam{}, false
	}
}

func mapDeepSeekResponse(resp *responses.Response, providerInstanceID, modelID string) (*llm.Completion, error) {
	if resp == nil {
		return &llm.Completion{FinishReason: "stop"}, nil
	}
	if resp.Status == responses.ResponseStatusFailed {
		msg := strings.TrimSpace(resp.Error.Message)
		if msg == "" {
			msg = "deepseek response failed"
		}
		code := string(resp.Error.Code)
		return nil, gateway.NewProviderError("deepseek", 0, code, errors.New(msg))
	}

	c := &llm.Completion{
		Content:      resp.OutputText(),
		FinishReason: deepSeekFinishReason(resp),
		Usage:        mapDeepSeekUsage(resp.Usage),
	}
	replay := make([]json.RawMessage, 0, len(resp.Output))
	for _, item := range resp.Output {
		switch item.Type {
		case "function_call":
			c.ToolCalls = append(c.ToolCalls, llm.ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: item.Arguments,
			})
			if raw := item.RawJSON(); raw != "" {
				replay = append(replay, json.RawMessage(raw))
			}
		case "reasoning", "web_search_call":
			if raw := item.RawJSON(); raw != "" {
				replay = append(replay, json.RawMessage(raw))
			}
		}
	}
	c.Reasoning = newDeepSeekReasoning(replay, providerInstanceID, modelID, len(c.ToolCalls) > 0)
	return c, nil
}

func deepSeekFinishReason(resp *responses.Response) string {
	for _, item := range resp.Output {
		if item.Type == "function_call" {
			return "tool_calls"
		}
	}
	if resp.Status == responses.ResponseStatusIncomplete {
		if resp.IncompleteDetails.Reason == "max_output_tokens" {
			return "length"
		}
		if resp.IncompleteDetails.Reason != "" {
			return resp.IncompleteDetails.Reason
		}
		return "length"
	}
	return "stop"
}

func mapDeepSeekUsage(src responses.ResponseUsage) llm.Usage {
	usage := llm.Usage{
		PromptTokens:     src.InputTokens,
		CompletionTokens: src.OutputTokens,
		TotalTokens:      src.TotalTokens,
		CachedReadTokens: src.InputTokensDetails.CachedTokens,
		ReasoningTokens:  src.OutputTokensDetails.ReasoningTokens,
	}
	if usage.CachedReadTokens > 0 && usage.PromptTokens >= usage.CachedReadTokens {
		usage.CacheMissTokens = usage.PromptTokens - usage.CachedReadTokens
	}
	return usage
}

func newDeepSeekReasoning(items []json.RawMessage, providerInstanceID, modelID string, keep bool) *llm.Reasoning {
	if !keep || len(items) == 0 || providerInstanceID == "" || modelID == "" {
		return nil
	}
	payload, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return &llm.Reasoning{
		State: &llm.ReasoningState{
			Provider: providerInstanceID,
			Model:    modelID,
			Kind:     deepSeekReasoningItemsKind,
			Payload:  payload,
		},
	}
}

type deepSeekStreamAccum struct {
	result             *llm.StreamResult
	final              *responses.Response
	failErr            error
	toolCalls          map[int64]*llm.ToolCall
	providerInstanceID string
	model              string
}

func newDeepSeekStreamAccum(providerInstanceID, modelID string) *deepSeekStreamAccum {
	return &deepSeekStreamAccum{
		result:             &llm.StreamResult{},
		toolCalls:          map[int64]*llm.ToolCall{},
		providerInstanceID: providerInstanceID,
		model:              modelID,
	}
}

func (a *deepSeekStreamAccum) apply(evt responses.ResponseStreamEventUnion, sink gateway.StreamSink) error {
	if a.failErr != nil {
		return a.failErr
	}
	switch evt.Type {
	case "response.output_text.delta":
		delta := deepSeekStreamDelta(evt)
		a.result.Content += delta
		if delta != "" && sink != nil {
			sink.OnTextDelta(delta)
		}
	case "response.reasoning_text.delta":
		delta := deepSeekStreamDelta(evt)
		if delta != "" && sink != nil {
			sink.OnReasoningDelta(delta)
		}
	case "response.output_item.added":
		if evt.Item.Type == "function_call" {
			tc := a.toolCallAt(evt.OutputIndex)
			if evt.Item.CallID != "" {
				tc.ID = evt.Item.CallID
			}
			if evt.Item.Name != "" {
				tc.Name = evt.Item.Name
			}
			if evt.Item.Arguments != "" {
				tc.Arguments = evt.Item.Arguments
			}
		}
	case "response.function_call_arguments.delta":
		a.toolCallAt(evt.OutputIndex).Arguments += deepSeekStreamDelta(evt)
	case "response.function_call_arguments.done":
		if evt.Arguments != "" {
			a.toolCallAt(evt.OutputIndex).Arguments = evt.Arguments
		}
	case "response.completed", "response.incomplete":
		resp := evt.Response
		a.final = &resp
	case "response.failed":
		a.failErr = deepSeekFailedError(evt.Response)
		return a.failErr
	case "error":
		msg := strings.TrimSpace(evt.Message)
		if msg == "" {
			msg = "deepseek stream error"
		}
		a.failErr = gateway.NewProviderError("deepseek", 0, evt.Code, errors.New(msg))
		return a.failErr
	}
	return nil
}

func (a *deepSeekStreamAccum) toolCallAt(index int64) *llm.ToolCall {
	if tc, ok := a.toolCalls[index]; ok {
		return tc
	}
	tc := &llm.ToolCall{}
	a.toolCalls[index] = tc
	return tc
}

func (a *deepSeekStreamAccum) finalize(sink gateway.StreamSink) *llm.StreamResult {
	if a.final != nil {
		mapped, err := mapDeepSeekResponse(a.final, a.providerInstanceID, a.model)
		if err != nil {
			a.failErr = err
		} else if mapped != nil {
			a.result.Content = mapped.Content
			a.result.ToolCalls = mapped.ToolCalls
			a.result.FinishReason = mapped.FinishReason
			a.result.Usage = mapped.Usage
			a.result.Reasoning = mapped.Reasoning
		}
	} else {
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
		if a.result.FinishReason == "" {
			if len(a.result.ToolCalls) > 0 {
				a.result.FinishReason = "tool_calls"
			} else {
				a.result.FinishReason = "stop"
			}
		}
	}
	if sink != nil {
		sink.OnDone()
	}
	return a.result
}

func deepSeekStreamDelta(evt responses.ResponseStreamEventUnion) string {
	if evt.Delta.OfString != "" {
		return evt.Delta.OfString
	}
	var wire struct {
		Delta string `json:"delta"`
	}
	if raw := evt.RawJSON(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &wire)
	}
	return wire.Delta
}

func deepSeekFailedError(resp responses.Response) error {
	msg := strings.TrimSpace(resp.Error.Message)
	if msg == "" {
		msg = "deepseek response failed"
	}
	return gateway.NewProviderError("deepseek", 0, string(resp.Error.Code), errors.New(msg))
}
