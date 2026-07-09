package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"go-code-agent/infra"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// OpenAI Provider - wraps github.com/openai/openai-go
//
// This file owns *all* interaction with the OpenAI SDK. The rest of
// the codebase speaks neutral types (Message / ToolDef / Completion /
// StreamResult). Anything that used to live in the old llm_client.go
// that was specifically about OpenAI's SDK lives here.

type openAIProvider struct {
	client openai.Client
}

func (p *openAIProvider) Name() string { return "openai" }

// toOpenAIMessages converts []Message into the SDK's
// ChatCompletionMessageParamUnion slice. System / user / assistant
// (with tool_calls) / tool roles are all first-class on OpenAI so
// translation is near-direct.
func toOpenAIMessages(msgs []Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			out = append(out, openai.SystemMessage(m.Content))
		case RoleUser:
			out = append(out, openai.UserMessage(m.Content))
		case RoleAssistant:
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
		case RoleTool:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		}
	}
	return out
}

func toOpenAITools(defs []ToolDef) []openai.ChatCompletionToolParam {
	if len(defs) == 0 {
		return nil
	}
	out := make([]openai.ChatCompletionToolParam, 0, len(defs))
	for _, d := range defs {
		params := openai.FunctionParameters{}
		for k, v := range d.Parameters {
			params[k] = v
		}
		// Ensure "type":"object" is present (JSON schema requirement).
		if _, ok := params["type"]; !ok {
			params["type"] = "object"
		}
		out = append(out, openai.ChatCompletionToolParam{
			Function: openai.FunctionDefinitionParam{
				Name:        d.Name,
				Description: openai.String(d.Description),
				Parameters:  params,
			},
		})
	}
	return out
}

func (p *openAIProvider) Call(ctx context.Context, cp CallParams) (*Completion, error) {
	params := openai.ChatCompletionNewParams{
		Model:    cp.Model,
		Messages: toOpenAIMessages(cp.Messages),
	}
	if tools := toOpenAITools(cp.Tools); len(tools) > 0 {
		params.Tools = tools
	}
	if cp.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(cp.MaxTokens))
	}
	if cp.Temperature != 0 {
		params.Temperature = openai.Float(cp.Temperature)
	}

	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return &Completion{}, nil
	}
	choice := resp.Choices[0]
	tcs := make([]ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		tcs = append(tcs, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return &Completion{
		Content:      choice.Message.Content,
		ToolCalls:    tcs,
		FinishReason: string(choice.FinishReason),
		Usage: Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			// OpenAI exposes cached prompt tokens under
			// PromptTokensDetails.CachedTokens. Surface it so the
			// recorder can attribute prompt-cache savings.
			CachedReadTokens: resp.Usage.PromptTokensDetails.CachedTokens,
		},
	}, nil
}

func (p *openAIProvider) Stream(ctx context.Context, cp CallParams, sink StreamSink) (*StreamResult, error) {
	if sink == nil {
		sink = SilentStreamSink{}
	}
	params := openai.ChatCompletionNewParams{
		Model:    cp.Model,
		Messages: toOpenAIMessages(cp.Messages),
	}
	if tools := toOpenAITools(cp.Tools); len(tools) > 0 {
		params.Tools = tools
	}
	if cp.MaxTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(cp.MaxTokens))
	}
	if cp.Temperature != 0 {
		params.Temperature = openai.Float(cp.Temperature)
	}

	// Ask the API to emit a final chunk carrying token usage. Without
	// this flag the streaming endpoint never reports usage and the
	// UsageRecorder gets all-zero rows. The extra chunk has empty
	// choices so the existing accumulation logic ignores it cleanly.
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
		IncludeUsage: openai.Bool(true),
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)

	result := &StreamResult{}

	// Tool-call delta accumulator keyed by index. OpenAI's streaming
	// protocol emits tool calls piecewise: the id/name arrive in the
	// first delta for an index, then arguments arrive as a stream of
	// JSON fragments. We concatenate them into Arguments.
	type tcAccum struct {
		ID       string
		Name     string
		ArgsJSON strings.Builder
	}
	accum := map[int64]*tcAccum{}

	for stream.Next() {
		chunk := stream.Current()

		// Final usage chunk: choices is empty, usage is populated.
		// Capture it before bailing out on len(choices)==0. Earlier
		// chunks also carry a Usage field but it's null/zero, so
		// overwriting on every chunk is safe.
		if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 {
			result.Usage = Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
				CachedReadTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
			}
		}

		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		result.FinishReason = string(chunk.Choices[0].FinishReason)

		if delta.Content != "" {
			sink.OnTextDelta(delta.Content)
			result.Content += delta.Content
		}

		for _, tc := range delta.ToolCalls {
			idx := tc.Index
			acc, ok := accum[idx]
			if !ok {
				acc = &tcAccum{}
				accum[idx] = acc
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function.Name != "" {
				acc.Name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				acc.ArgsJSON.WriteString(tc.Function.Arguments)
			}
		}
	}
	sink.OnDone()

	if err := stream.Err(); err != nil {
		return result, fmt.Errorf("streaming failed: %w", err)
	}

	// Flatten the accumulator in index order.
	for i := int64(0); ; i++ {
		acc, ok := accum[i]
		if !ok {
			break
		}
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:        acc.ID,
			Name:      acc.Name,
			Arguments: acc.ArgsJSON.String(),
		})
	}

	return result, nil
}

// openAIIsRetriable returns true if err looks transient for the
// OpenAI SDK. Kept here so the classifier stays next to the SDK types
// it inspects.
func openAIIsRetriable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Per-attempt ctx timeout: the parent ctx isn't done (the retry
	// wrapper checks that separately and short-circuits), so this is
	// the LlmCallTimeout firing on a single hung attempt — retry.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *openai.Error
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

// newOpenAIProvider builds an OpenAI provider with explicit credentials.
// Empty apiKey/baseURL fall back to the OPENAI_API_KEY / OPENAI_BASE_URL
// env vars, so the same constructor serves both the default startup
// registration and the judge's dedicated-endpoint path (JUDGE_API_KEY /
// JUDGE_BASE_URL).
func newOpenAIProvider(apiKey, baseURL string) Provider {
	if apiKey == "" {
		apiKey = infra.Cfg.OpenAIAPIKey
	}
	if baseURL == "" {
		baseURL = infra.Cfg.OpenAIBaseURL
	}
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	// Install an explicit http.Client with timeouts. The SDK otherwise
	// falls back to http.DefaultClient which has no read deadline, so a
	// stuck SSE stream from an OpenAI-compatible gateway (seen with
	// some GLM endpoints under long contexts) freezes the agent loop
	// indefinitely - no error, no chunks, no EOF.
	//
	// We deliberately keep Client.Timeout unset for streaming: it would
	// abort even a healthy 5-minute generation. Instead the per-call
	// ctx deadline (LlmCallTimeout, set in llm_client.go) bounds the
	// whole call, and the transport-level timeouts below kill truly
	// dead connections fast.
	opts = append(opts, option.WithHTTPClient(&http.Client{
		Timeout: infra.LlmHTTPTimeout, // hard backstop for non-streaming calls
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
		},
	}))
	return &openAIProvider{client: openai.NewClient(opts...)}
}

func init() {
	// Configure SDK from environment. Keys are the same ones the old
	// main.go used; we preserve them so users don't have to change
	// anything.
	RegisterProvider(newOpenAIProvider("", ""))
	RegisterProviderBuilder("openai", newOpenAIProvider)
	SetRetriableClassifier("openai", openAIIsRetriable)
}
