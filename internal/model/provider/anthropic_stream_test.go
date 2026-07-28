package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-code-agent/internal/llm"

	"github.com/anthropics/anthropic-sdk-go"
)

// Recorded Anthropic Messages SSE data payloads (paired with event: type).
var anthropicToolUseEvents = []struct {
	event string
	data  string
}{
	{
		event: "message_start",
		data:  `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":10,"cache_read_input_tokens":40,"output_tokens":0}}}`,
	},
	{
		event: "content_block_start",
		data:  `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"read_file","input":{}}}`,
	},
	{
		event: "content_block_delta",
		data:  `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\""}}`,
	},
	{
		event: "content_block_delta",
		data:  `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":":\"a.go\"}"}}`,
	},
	{
		event: "content_block_stop",
		data:  `{"type":"content_block_stop","index":0}`,
	},
	{
		event: "message_delta",
		data:  `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":25}}`,
	},
	{
		event: "message_stop",
		data:  `{"type":"message_stop"}`,
	},
}

func TestAnthropicStreamAccum_ToolUseDeltasAndUsage(t *testing.T) {
	accum := newAnthropicStreamAccum()
	sink := &collectSink{}
	for i, ev := range anthropicToolUseEvents {
		var union anthropic.MessageStreamEventUnion
		if err := union.UnmarshalJSON([]byte(ev.data)); err != nil {
			t.Fatalf("event %d unmarshal: %v\ndata=%s", i, err, ev.data)
		}
		accum.apply(union, sink)
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
	if tc.ID != "toolu_01" || tc.Name != "read_file" || tc.Arguments != `{"path":"a.go"}` {
		t.Fatalf("tool call = %#v", tc)
	}
	if got.Usage.PromptTokens != 100 || got.Usage.CompletionTokens != 25 || got.Usage.TotalTokens != 125 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if got.Usage.CachedReadTokens != 40 || got.Usage.CacheCreateTokens != 10 {
		t.Fatalf("cache usage = %+v", got.Usage)
	}
}

func TestAnthropicStreamAccum_EmptyInputPlaceholderNotPrefixed(t *testing.T) {
	// Regression: marshalling content_block_start input:{} must not prefix deltas.
	events := []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_x","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}`,
	}
	accum := newAnthropicStreamAccum()
	for _, raw := range events {
		var union anthropic.MessageStreamEventUnion
		if err := union.UnmarshalJSON([]byte(raw)); err != nil {
			t.Fatal(err)
		}
		accum.apply(union, nil)
	}
	got := accum.finalize(nil)
	if got.ToolCalls[0].Arguments != `{"command":"ls"}` {
		t.Fatalf("arguments polluted by empty input: %q", got.ToolCalls[0].Arguments)
	}
}

func TestAnthropicProvider_Stream_HTTPGolden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") && r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, ev := range anthropicToolUseEvents {
			_, _ = w.Write([]byte("event: " + ev.event + "\ndata: " + ev.data + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	p := NewAnthropic("test-key", srv.URL+"/")
	sink := &collectSink{}
	got, err := p.Stream(context.Background(), llm.CallParams{
		Model:    "claude-test",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "read a.go"}},
		Tools:    []llm.ToolDef{{Name: "read_file", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}},
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
	if got.Usage.PromptTokens != 100 || got.Usage.CachedReadTokens != 40 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}
