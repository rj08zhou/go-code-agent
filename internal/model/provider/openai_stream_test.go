package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-code-agent/internal/llm"

	"github.com/openai/openai-go"
)

type collectSink struct {
	deltas []string
	done   bool
}

func (c *collectSink) OnTextDelta(text string) { c.deltas = append(c.deltas, text) }
func (c *collectSink) OnDone()                 { c.done = true }

// Recorded OpenAI chat.completion.chunk payloads (SSE data= bodies).
var openaiToolCallChunks = []string{
	`{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\""}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"path\":\""}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"a.go\"}"}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	`{"id":"chatcmpl-test","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":50}}}`,
}

func TestOpenAIStreamAccum_ToolCallDeltasAndUsage(t *testing.T) {
	accum := newOpenAIStreamAccum("", "")
	sink := &collectSink{}
	for i, raw := range openaiToolCallChunks {
		var chunk openai.ChatCompletionChunk
		if err := chunk.UnmarshalJSON([]byte(raw)); err != nil {
			t.Fatalf("chunk %d unmarshal: %v", i, err)
		}
		accum.apply(chunk, sink)
	}
	got := accum.finalize(sink)

	if !sink.done {
		t.Fatal("expected OnDone")
	}
	if got.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q", got.FinishReason)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %#v", got.ToolCalls)
	}
	tc := got.ToolCalls[0]
	if tc.ID != "call_abc" || tc.Name != "read_file" || tc.Arguments != `{"path":"a.go"}` {
		t.Fatalf("tool call = %#v", tc)
	}
	if got.Usage.PromptTokens != 100 || got.Usage.CompletionTokens != 20 || got.Usage.TotalTokens != 120 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if got.Usage.CachedReadTokens != 50 {
		t.Fatalf("CachedReadTokens = %d", got.Usage.CachedReadTokens)
	}
}

func TestOpenAIStreamAccum_TwoToolCallsOrdered(t *testing.T) {
	chunks := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"list_dir","arguments":"{"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	accum := newOpenAIStreamAccum("", "")
	for i, raw := range chunks {
		var chunk openai.ChatCompletionChunk
		if err := chunk.UnmarshalJSON([]byte(raw)); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		accum.apply(chunk, nil)
	}
	got := accum.finalize(nil)
	if len(got.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %#v", got.ToolCalls)
	}
	if got.ToolCalls[0].ID != "call_a" || got.ToolCalls[1].ID != "call_b" {
		t.Fatalf("order by index broken: %#v", got.ToolCalls)
	}
}

func TestOpenAIProvider_Stream_HTTPGolden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, chunk := range openaiToolCallChunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", srv.URL+"/v1")
	sink := &collectSink{}
	got, err := p.Stream(context.Background(), llm.CallParams{
		Model:    "gpt-test",
		Messages: []llm.Message{llm.UserMessage("read a.go")},
		Tools:    []llm.ToolDef{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
	}, sink)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !sink.done {
		t.Fatal("expected OnDone")
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("got %#v", got)
	}
	if got.Usage.CachedReadTokens != 50 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}
