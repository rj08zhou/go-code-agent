package provider

import (
	"context"
	"encoding/json"
	"errors"
	"go-code-agent/internal/gateway"
	"go-code-agent/internal/llm"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

const anthropicDefaultMaxTokens = 16384

type AnthropicProvider struct {
	client     anthropic.Client
	instanceID string
}

func NewAnthropic(apiKey, baseURL string) gateway.Provider {
	opts := []option.RequestOption{}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &AnthropicProvider{
		client:     anthropic.NewClient(opts...),
		instanceID: gateway.StableProviderInstanceID("anthropic", baseURL),
	}
}

func (p *AnthropicProvider) Name() string       { return "anthropic" }
func (p *AnthropicProvider) InstanceID() string { return p.instanceID }

func (p *AnthropicProvider) Capabilities() gateway.ProviderCapabilities {
	return gateway.ProviderCapabilities{
		StructuredOutput: true,
		ToolCalling:      true,
		Streaming:        true,
	}
}

func toAnthropicProviderError(err error) error {
	if err == nil {
		return nil
	}
	statusCode := 0
	code := ""
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		statusCode = apiErr.StatusCode
		code = string(apiErr.Type())
	}
	return gateway.NewProviderError("anthropic", statusCode, code, err)
}

func toAnthropicOutputConfig(output *llm.StructuredOutput) (anthropic.OutputConfigParam, bool) {
	if output == nil {
		return anthropic.OutputConfigParam{}, false
	}
	return anthropic.OutputConfigParam{
		Format: anthropic.JSONOutputFormatParam{Schema: output.Schema},
	}, true
}

func (p *AnthropicProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	sys, msgs := buildAnthropicMessages(params.Messages)
	maxTok := int64(params.MaxTokens)
	if maxTok <= 0 {
		maxTok = anthropicDefaultMaxTokens
	}
	req := anthropic.MessageNewParams{
		Model:     anthropic.Model(params.Model),
		MaxTokens: maxTok,
		Messages:  msgs,
	}
	if len(sys) > 0 {
		req.System = sys
	}
	if tools := toAnthropicTools(params.Tools); len(tools) > 0 {
		req.Tools = tools
	}
	if outputConfig, ok := toAnthropicOutputConfig(params.StructuredOutput); ok {
		req.OutputConfig = outputConfig
	}
	if params.Temperature != 0 {
		req.Temperature = param.NewOpt(params.Temperature)
	}

	resp, err := p.client.Messages.New(ctx, req)
	if err != nil {
		return nil, toAnthropicProviderError(err)
	}
	return mapAnthropicResponse(resp), nil
}

func (p *AnthropicProvider) Stream(ctx context.Context, params llm.CallParams, sink gateway.StreamSink) (*llm.StreamResult, error) {
	sys, msgs := buildAnthropicMessages(params.Messages)
	maxTok := int64(params.MaxTokens)
	if maxTok <= 0 {
		maxTok = anthropicDefaultMaxTokens
	}
	req := anthropic.MessageNewParams{
		Model:     anthropic.Model(params.Model),
		MaxTokens: maxTok,
		Messages:  msgs,
	}
	if len(sys) > 0 {
		req.System = sys
	}
	if tools := toAnthropicTools(params.Tools); len(tools) > 0 {
		req.Tools = tools
	}
	if outputConfig, ok := toAnthropicOutputConfig(params.StructuredOutput); ok {
		req.OutputConfig = outputConfig
	}
	if params.Temperature != 0 {
		req.Temperature = param.NewOpt(params.Temperature)
	}

	accum := newAnthropicStreamAccum()
	stream := p.client.Messages.NewStreaming(ctx, req)
	for stream.Next() {
		accum.apply(stream.Current(), sink)
	}
	return accum.finalize(sink), toAnthropicProviderError(stream.Err())
}

type anthropicToolAccum struct {
	ID       string
	Name     string
	ArgsJSON strings.Builder
}

// anthropicStreamAccum folds MessageStreamEventUnion events into a StreamResult.
// Extracted so golden tests can feed recorded SSE data payloads without HTTP.
type anthropicStreamAccum struct {
	result *llm.StreamResult
	blocks map[int64]*anthropicToolAccum
}

func newAnthropicStreamAccum() *anthropicStreamAccum {
	return &anthropicStreamAccum{
		result: &llm.StreamResult{},
		blocks: map[int64]*anthropicToolAccum{},
	}
}

func (a *anthropicStreamAccum) apply(ev anthropic.MessageStreamEventUnion, sink gateway.StreamSink) {
	switch ev.Type {
	case "message_start":
		ms := ev.AsMessageStart()
		a.result.Usage.PromptTokens = ms.Message.Usage.InputTokens
		a.result.Usage.CachedReadTokens = ms.Message.Usage.CacheReadInputTokens
		a.result.Usage.CacheCreateTokens = ms.Message.Usage.CacheCreationInputTokens
	case "content_block_start":
		cb := ev.ContentBlock
		if cb.Type == "tool_use" {
			a.blocks[ev.Index] = &anthropicToolAccum{ID: cb.ID, Name: cb.Name}
			// Streaming tool_use starts with input:{} ; the real JSON arrives
			// via input_json_delta. Do not marshal the empty placeholder or
			// arguments become "{}{\"path\":...}".
			if cb.Input != nil {
				if data, err := json.Marshal(cb.Input); err == nil {
					if s := string(data); s != "{}" && s != "null" {
						a.blocks[ev.Index].ArgsJSON.Write(data)
					}
				}
			}
		}
	case "content_block_delta":
		d := ev.Delta
		switch d.Type {
		case "text_delta":
			if d.Text != "" {
				if sink != nil {
					sink.OnTextDelta(d.Text)
				}
				a.result.Content += d.Text
			}
		case "input_json_delta":
			if acc, ok := a.blocks[ev.Index]; ok {
				acc.ArgsJSON.WriteString(d.PartialJSON)
			}
		}
	case "message_delta":
		if sr := ev.Delta.StopReason; sr != "" {
			a.result.FinishReason = mapAnthropicStop(anthropic.StopReason(sr))
		}
		if ev.Usage.OutputTokens > 0 {
			a.result.Usage.CompletionTokens = ev.Usage.OutputTokens
			a.result.Usage.TotalTokens = a.result.Usage.PromptTokens + ev.Usage.OutputTokens
		}
	}
}

func (a *anthropicStreamAccum) finalize(sink gateway.StreamSink) *llm.StreamResult {
	indices := make([]int64, 0, len(a.blocks))
	for idx := range a.blocks {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	for _, idx := range indices {
		acc := a.blocks[idx]
		args := acc.ArgsJSON.String()
		if args == "" {
			args = "{}"
		}
		a.result.ToolCalls = append(a.result.ToolCalls, llm.ToolCall{
			ID:        acc.ID,
			Name:      acc.Name,
			Arguments: args,
		})
	}
	if sink != nil {
		sink.OnDone()
	}
	return a.result
}

func buildAnthropicMessages(msgs []llm.Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	var sys []anthropic.TextBlockParam
	var out []anthropic.MessageParam

	// Prompt caching: place one cache_control breakpoint on the LAST
	// system block. Anthropic builds its cache prefix in the fixed
	// order tools -> system -> messages, so this single breakpoint
	// covers the entire static prefix (all tool defs + system prompt),
	// which is identical on every turn and is by far the largest
	// repeated chunk of input.
	//
	// Safe to always set: if the prefix is below the model's minimum
	// cacheable size, Anthropic silently ignores the breakpoint.
	defer func() {
		if n := len(sys); n > 0 {
			sys[n-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
	}()

	appendUser := func(blocks []anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == anthropic.MessageParamRoleUser {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropic.NewUserMessage(blocks...))
	}
	appendAsst := func(blocks []anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == anthropic.MessageParamRoleAssistant {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropic.NewAssistantMessage(blocks...))
	}

	for _, m := range msgs {
		switch m.Role {
		case llm.RoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				sys = append(sys, anthropic.TextBlockParam{Text: m.Content})
			}
		case llm.RoleUser:
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			appendUser([]anthropic.ContentBlockParamUnion{{
				OfText: &anthropic.TextBlockParam{Text: m.Content},
			}})
		case llm.RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfText: &anthropic.TextBlockParam{Text: m.Content},
				})
			}
			for _, tc := range m.ToolCalls {
				var parsed any
				if tc.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Arguments), &parsed); err != nil {
						parsed = tc.Arguments
					}
				} else {
					parsed = map[string]any{}
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID: tc.ID, Name: tc.Name, Input: parsed,
					},
				})
			}
			appendAsst(blocks)
		case llm.RoleTool:
			content := m.Content
			if content == "" {
				content = "(empty)"
			}
			tr := anthropic.ToolResultBlockParam{
				ToolUseID: m.ToolCallID,
				Content: []anthropic.ToolResultBlockParamContentUnion{
					{OfText: &anthropic.TextBlockParam{Text: content}},
				},
			}
			if isToolResultError(content) {
				tr.IsError = param.NewOpt(true)
			}
			appendUser([]anthropic.ContentBlockParamUnion{{OfToolResult: &tr}})
		}
	}
	return sys, out
}

// isToolResultError reports whether a tool result content represents a
// failed invocation. Tool outputs encode status as a leading tag, e.g.
// "[ERROR] ...", "[TIMEOUT] ...", "[SECURITY] ...", so the previous
// HasPrefix("error") check never matched and is_error was never set.
func isToolResultError(content string) bool {
	c := strings.ToLower(strings.TrimSpace(content))
	for _, p := range []string{"[error]", "[timeout]", "[cancelled]", "[security]", "[hitl-rejected]", "[hitl-modified]", "[skipped]"} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

func toAnthropicTools(defs []llm.ToolDef) []anthropic.ToolUnionParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]anthropic.ToolUnionParam, 0, len(defs))
	for _, d := range defs {
		schema := anthropic.ToolInputSchemaParam{}
		if props, ok := d.Parameters["properties"]; ok {
			schema.Properties = props
		}
		if req, ok := d.Parameters["required"].([]string); ok {
			schema.Required = req
		} else if reqAny, ok := d.Parameters["required"].([]any); ok {
			reqs := make([]string, 0, len(reqAny))
			for _, r := range reqAny {
				if s, ok := r.(string); ok {
					reqs = append(reqs, s)
				}
			}
			schema.Required = reqs
		}
		tp := &anthropic.ToolParam{
			Name:        d.Name,
			InputSchema: schema,
		}
		if d.Description != "" {
			tp.Description = param.NewOpt(d.Description)
		}
		out = append(out, anthropic.ToolUnionParam{OfTool: tp})
	}
	return out
}

func mapAnthropicStop(reason anthropic.StopReason) string {
	switch reason {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return "stop"
	case anthropic.StopReasonToolUse:
		return "tool_calls"
	case anthropic.StopReasonMaxTokens:
		return "length"
	case anthropic.StopReasonRefusal, anthropic.StopReasonPauseTurn:
		return "content_filter"
	}
	return "stop"
}

func mapAnthropicResponse(resp *anthropic.Message) *llm.Completion {
	c := &llm.Completion{
		FinishReason: mapAnthropicStop(resp.StopReason),
		Usage: llm.Usage{
			PromptTokens:      resp.Usage.InputTokens,
			CompletionTokens:  resp.Usage.OutputTokens,
			TotalTokens:       resp.Usage.InputTokens + resp.Usage.OutputTokens,
			CachedReadTokens:  resp.Usage.CacheReadInputTokens,
			CacheCreateTokens: resp.Usage.CacheCreationInputTokens,
		},
	}
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			c.Content += block.Text
		case "tool_use":
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			c.ToolCalls = append(c.ToolCalls, llm.ToolCall{
				ID:        block.ID,
				Name:      string(block.Name),
				Arguments: args,
			})
		}
	}
	return c
}
