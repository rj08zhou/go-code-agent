package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"go-code-agent/infra"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// Anthropic Provider - wraps github.com/anthropics/anthropic-sdk-go
//
// Translation cheatsheet (neutral -> Anthropic):
//
//   Role=system     -> hoisted to request.System ([]TextBlockParam).
//   Role=user       -> MessageParam{Role:"user",  Content: [text_block]}
//   Role=assistant  -> MessageParam{Role:"assistant", Content:
//                      [text_block?, tool_use_block*]}
//   Role=tool       -> folded into the preceding/next user message as
//                      a tool_result content block (Anthropic has no
//                      dedicated "tool" role).
//
// Consecutive same-role messages (e.g. user+user after a gate injection)
// are merged into one MessageParam - Anthropic rejects alternating-role
// violations otherwise.
//
// Finish reason mapping:
//   end_turn / stop_sequence -> "stop"
//   tool_use                 -> "tool_calls"
//   max_tokens               -> "length"
//   refusal / pause_turn     -> "content_filter"

const anthropicDefaultMaxTokens = 16384

type anthropicProvider struct {
	client anthropic.Client
}

func (p *anthropicProvider) Name() string { return "anthropic" }

// buildAnthropicMessages walks the neutral message list and produces
// (system-blocks, message-params). It is robust against common shapes
// the agent loop produces: leading system, interleaved user/tool,
// assistant-with-tool_calls, etc.
func buildAnthropicMessages(msgs []Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam) {
	var systemBlocks []anthropic.TextBlockParam
	var out []anthropic.MessageParam

	// Helper: append a user MessageParam, merging into the last one if
	// it was also user (Anthropic requires strictly alternating roles
	// between non-tool turns; merging is the safe fix).
	appendUserBlocks := func(blocks []anthropic.ContentBlockParamUnion) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == anthropic.MessageParamRoleUser {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropic.NewUserMessage(blocks...))
	}
	appendAssistantBlocks := func(blocks []anthropic.ContentBlockParamUnion) {
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
		case RoleSystem:
			if strings.TrimSpace(m.Content) != "" {
				systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: m.Content})
			}

		case RoleUser:
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			appendUserBlocks([]anthropic.ContentBlockParamUnion{{
				OfText: &anthropic.TextBlockParam{Text: m.Content},
			}})

		case RoleAssistant:
			var blocks []anthropic.ContentBlockParamUnion
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfText: &anthropic.TextBlockParam{Text: m.Content},
				})
			}
			for _, tc := range m.ToolCalls {
				// Arguments is a JSON string in the neutral model;
				// Anthropic expects the parsed value in Input.
				var parsed any
				if tc.Arguments != "" {
					if err := json.Unmarshal([]byte(tc.Arguments), &parsed); err != nil {
						// Keep raw string so the request still works -
						// Anthropic will usually echo it back and the
						// loop can recover.
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
			appendAssistantBlocks(blocks)

		case RoleTool:
			// tool_result must live inside a user message.
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
			// Heuristic: if the tool output starts with "Error" we mark
			// it is_error so the model knows. Keeps parity with the
			// isToolFailure logic in agent_loop.go without duplicating
			// its full list.
			if strings.HasPrefix(strings.ToLower(content), "error") {
				tr.IsError = param.NewOpt(true)
			}
			appendUserBlocks([]anthropic.ContentBlockParamUnion{{OfToolResult: &tr}})
		}
	}

	// Prompt caching: place one cache_control breakpoint on the LAST
	// system block. Anthropic builds its cache prefix in the fixed
	// order tools -> system -> messages, and a breakpoint caches
	// everything up to and including that block - so this single
	// breakpoint covers the entire static prefix (all tool defs + the
	// whole system prompt: system.md + evergreen memory + resume
	// context), which is identical on every turn of a session and is
	// by far the largest repeated chunk of input. On a cache hit those
	// input tokens are billed at a large discount (surfaced via
	// Usage.CachedReadTokens, which we already record).
	//
	// Safe to always set: if the prefix is below the model's minimum
	// cacheable size (~1024 tokens; 2048 for Haiku) Anthropic silently
	// ignores the breakpoint rather than erroring, so short-prompt
	// callers (judge, memory-save, subagent) are unaffected.
	if n := len(systemBlocks); n > 0 {
		systemBlocks[n-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	return systemBlocks, out
}

func toAnthropicTools(defs []ToolDef) []anthropic.ToolUnionParam {
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
			var reqs []string
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

func (p *anthropicProvider) Call(ctx context.Context, cp CallParams) (*Completion, error) {
	sys, msgs := buildAnthropicMessages(cp.Messages)
	maxTok := int64(cp.MaxTokens)
	if maxTok <= 0 {
		maxTok = anthropicDefaultMaxTokens
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(cp.Model),
		MaxTokens: maxTok,
		Messages:  msgs,
	}
	if len(sys) > 0 {
		params.System = sys
	}
	if tools := toAnthropicTools(cp.Tools); len(tools) > 0 {
		params.Tools = tools
	}
	if cp.Temperature != 0 {
		params.Temperature = param.NewOpt(cp.Temperature)
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, err
	}
	comp := &Completion{FinishReason: mapAnthropicStop(resp.StopReason)}
	comp.Usage = Usage{
		PromptTokens:      resp.Usage.InputTokens,
		CompletionTokens:  resp.Usage.OutputTokens,
		TotalTokens:       resp.Usage.InputTokens + resp.Usage.OutputTokens,
		CachedReadTokens:  resp.Usage.CacheReadInputTokens,
		CacheCreateTokens: resp.Usage.CacheCreationInputTokens,
	}
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			comp.Content += block.Text
		case "tool_use":
			// Input is already json.RawMessage; convert to string for
			// parity with OpenAI's Arguments string convention.
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			comp.ToolCalls = append(comp.ToolCalls, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			})
		}
	}
	return comp, nil
}

func (p *anthropicProvider) Stream(ctx context.Context, cp CallParams, sink StreamSink) (*StreamResult, error) {
	if sink == nil {
		sink = SilentStreamSink{}
	}
	sys, msgs := buildAnthropicMessages(cp.Messages)
	maxTok := int64(cp.MaxTokens)
	if maxTok <= 0 {
		maxTok = anthropicDefaultMaxTokens
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(cp.Model),
		MaxTokens: maxTok,
		Messages:  msgs,
	}
	if len(sys) > 0 {
		params.System = sys
	}
	if tools := toAnthropicTools(cp.Tools); len(tools) > 0 {
		params.Tools = tools
	}
	if cp.Temperature != 0 {
		params.Temperature = param.NewOpt(cp.Temperature)
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	result := &StreamResult{}

	// Per-index tool-use accumulator. Anthropic streams each tool_use
	// block with content_block_start (carrying id/name) followed by
	// input_json_delta fragments, then content_block_stop.
	type toolAccum struct {
		ID       string
		Name     string
		ArgsJSON strings.Builder
	}
	blocks := map[int64]*toolAccum{} // only tool_use blocks go here

	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "message_start":
			// message_start carries the initial Message object whose
			// Usage holds prompt-side accounting (input_tokens,
			// cache_read_input_tokens, cache_creation_input_tokens).
			// Output_tokens here is usually 0 and grows via
			// message_delta events.
			ms := ev.AsMessageStart()
			result.Usage.PromptTokens = ms.Message.Usage.InputTokens
			result.Usage.CachedReadTokens = ms.Message.Usage.CacheReadInputTokens
			result.Usage.CacheCreateTokens = ms.Message.Usage.CacheCreationInputTokens

		case "content_block_start":
			cb := ev.ContentBlock
			if cb.Type == "tool_use" {
				blocks[ev.Index] = &toolAccum{ID: cb.ID, Name: cb.Name}
				// If the server provided a pre-filled input (rare but
				// possible), serialize it now so Arguments is valid JSON.
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
			// message_delta.Usage is cumulative across the whole
			// stream - safe to overwrite on every event.
			if ev.Usage.OutputTokens > 0 {
				result.Usage.CompletionTokens = ev.Usage.OutputTokens
			}
			// Some Anthropic responses re-emit input-side numbers in
			// the final delta; only overwrite if they're non-zero so
			// we don't lose the message_start values.
			if ev.Usage.InputTokens > 0 {
				result.Usage.PromptTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				result.Usage.CachedReadTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				result.Usage.CacheCreateTokens = ev.Usage.CacheCreationInputTokens
			}

		case "message_stop":
			// handled via message_delta's stop_reason; nothing to do
		}
	}
	sink.OnDone()

	if err := stream.Err(); err != nil {
		return result, fmt.Errorf("anthropic streaming failed: %w", err)
	}

	// Flatten tool_use blocks in index order so callers see a stable
	// sequence (matches OpenAI provider behaviour).
	// Note: indices may not be contiguous if the model emitted text
	// blocks interleaved with tool_use, so walk up to max seen index.
	maxIdx := int64(-1)
	for i := range blocks {
		if i > maxIdx {
			maxIdx = i
		}
	}
	for i := int64(0); i <= maxIdx; i++ {
		acc, ok := blocks[i]
		if !ok {
			continue
		}
		args := acc.ArgsJSON.String()
		if args == "" {
			args = "{}"
		}
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:        acc.ID,
			Name:      acc.Name,
			Arguments: args,
		})
	}
	if result.FinishReason == "" && len(result.ToolCalls) > 0 {
		result.FinishReason = "tool_calls"
	}
	// Derive TotalTokens once at the end. Anthropic doesn't emit a
	// total directly; PromptTokens + CompletionTokens is the canonical
	// definition for billing.
	if result.Usage.TotalTokens == 0 {
		result.Usage.TotalTokens = result.Usage.PromptTokens + result.Usage.CompletionTokens
	}
	return result, nil
}

func anthropicIsRetriable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 408, 409, 425, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, h := range TransientMsgHints {
		if strings.Contains(msg, h) {
			return true
		}
	}
	return false
}

// newAnthropicProvider builds an Anthropic provider with explicit
// credentials. Empty apiKey/baseURL fall back to the ANTHROPIC_API_KEY /
// ANTHROPIC_BASE_URL env vars, so the same constructor serves both the
// default startup registration and the judge's dedicated-endpoint path.
func newAnthropicProvider(apiKey, baseURL string) Provider {
	if apiKey == "" {
		apiKey = infra.Cfg.AnthropicAPIKey
	}
	if baseURL == "" {
		baseURL = infra.Cfg.AnthropicBaseURL
	}
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &anthropicProvider{client: anthropic.NewClient(opts...)}
}

func init() {
	RegisterProvider(newAnthropicProvider("", ""))
	RegisterProviderBuilder("anthropic", newAnthropicProvider)
	SetRetriableClassifier("anthropic", anthropicIsRetriable)
}
