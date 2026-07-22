package provider

import (
	"context"
	"encoding/json"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

const anthropicDefaultMaxTokens = 16384

type AnthropicProvider struct {
	client anthropic.Client
}

func NewAnthropic(apiKey, baseURL string) model.Provider {
	opts := []option.RequestOption{}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &AnthropicProvider{client: anthropic.NewClient(opts...)}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

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
	if params.Temperature != 0 {
		req.Temperature = param.NewOpt(params.Temperature)
	}

	resp, err := p.client.Messages.New(ctx, req)
	if err != nil {
		return nil, err
	}
	return mapAnthropicResponse(resp), nil
}

func (p *AnthropicProvider) Stream(ctx context.Context, params llm.CallParams, sink model.StreamSink) (*llm.StreamResult, error) {
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
	if params.Temperature != 0 {
		req.Temperature = param.NewOpt(params.Temperature)
	}

	result := &llm.StreamResult{}

	type toolAccum struct {
		ID       string
		Name     string
		ArgsJSON strings.Builder
	}
	blocks := map[int64]*toolAccum{}

	stream := p.client.Messages.NewStreaming(ctx, req)
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "message_start":
			ms := ev.AsMessageStart()
			result.Usage.PromptTokens = ms.Message.Usage.InputTokens
			result.Usage.CachedReadTokens = ms.Message.Usage.CacheReadInputTokens
			result.Usage.CacheCreateTokens = ms.Message.Usage.CacheCreationInputTokens
		case "content_block_start":
			cb := ev.ContentBlock
			if cb.Type == "tool_use" {
				blocks[ev.Index] = &toolAccum{ID: cb.ID, Name: cb.Name}
				if cb.Input != nil {
					if data, err := json.Marshal(cb.Input); err == nil {
						blocks[ev.Index].ArgsJSON.Write(data)
					}
				}
			}
		case "content_block_delta":
			d := ev.Delta
			switch d.Type {
			case "text_delta":
				if d.Text != "" {
					sink.OnTextDelta(d.Text)
					result.Content += d.Text
				}
			case "input_json_delta":
				if acc, ok := blocks[ev.Index]; ok {
					acc.ArgsJSON.WriteString(d.PartialJSON)
				}
			}
		case "message_delta":
			if sr := ev.Delta.StopReason; sr != "" {
				result.FinishReason = mapAnthropicStop(anthropic.StopReason(sr))
			}
			if ev.Usage.OutputTokens > 0 {
				result.Usage.CompletionTokens = ev.Usage.OutputTokens
				result.Usage.TotalTokens = result.Usage.PromptTokens + ev.Usage.OutputTokens
			}
		}
	}
	sink.OnDone()

	for _, acc := range blocks {
		args := acc.ArgsJSON.String()
		if args == "" {
			args = "{}"
		}
		result.ToolCalls = append(result.ToolCalls, llm.ToolCall{
			ID:        acc.ID,
			Name:      acc.Name,
			Arguments: args,
		})
	}
	return result, stream.Err()
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
			if strings.HasPrefix(strings.ToLower(content), "error") {
				tr.IsError = param.NewOpt(true)
			}
			appendUser([]anthropic.ContentBlockParamUnion{{OfToolResult: &tr}})
		}
	}
	return sys, out
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
