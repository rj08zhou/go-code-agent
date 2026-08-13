package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go-code-agent/internal/gateway"
	"go-code-agent/internal/llm"

	"github.com/openai/openai-go/responses"
)

func TestNewDeepSeekIgnoresOpenAIEnvDefaults(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("OPENAI_API_KEY", "openai-env-key")

	empty := NewDeepSeek("ds-key", "")
	explicit := NewDeepSeek("ds-key", deepSeekDefaultBaseURL)
	if gateway.ProviderInstanceID(empty) != gateway.ProviderInstanceID(explicit) {
		t.Fatal("empty base URL must resolve to the official DeepSeek endpoint")
	}
	if empty.Name() != "deepseek" {
		t.Fatalf("Name = %q", empty.Name())
	}
}

func TestToDeepSeekRequest_SystemBecomesInstructions(t *testing.T) {
	req, err := toDeepSeekRequest(llm.CallParams{
		Model: "deepseek-v4-flash",
		Messages: []llm.Message{
			llm.SystemMessage("you are a coder"),
			llm.SystemMessage("prefer tests"),
			llm.UserMessage("hi"),
		},
		MaxTokens: 128,
		Tools: []llm.ToolDef{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  map[string]any{"type": "object"},
		}},
		Reasoning: &llm.ReasoningRequest{Enabled: true, Effort: "HIGH"},
	}, "deepseek:test")
	if err != nil {
		t.Fatalf("toDeepSeekRequest: %v", err)
	}

	body := marshalJSONMap(t, req)
	if got := body["instructions"]; got != "you are a coder\n\nprefer tests" {
		t.Fatalf("instructions = %#v", got)
	}
	if _, ok := body["store"]; ok {
		t.Fatal("store must not be sent; DeepSeek Responses is always stateless")
	}
	if _, ok := body["previous_response_id"]; ok {
		t.Fatal("previous_response_id is unsupported")
	}
	if _, ok := body["include"]; ok {
		t.Fatal("include is unsupported")
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatal("stream_options is unsupported")
	}
	if body["max_output_tokens"] != float64(128) {
		t.Fatalf("max_output_tokens = %#v", body["max_output_tokens"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v", body["reasoning"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "read_file" {
		t.Fatalf("tool = %#v", tool)
	}
	if _, ok := tool["strict"]; ok {
		t.Fatal("DeepSeek function tools should omit strict")
	}
}

func TestToDeepSeekRequest_ReplaysReasoningAndFunctionCall(t *testing.T) {
	instanceID := gateway.StableProviderInstanceID("deepseek", deepSeekDefaultBaseURL)
	payload, err := json.Marshal([]json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"need the file"}]}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := toDeepSeekRequest(llm.CallParams{
		Model: "deepseek-v4-flash",
		Messages: []llm.Message{
			llm.UserMessage("inspect a.go"),
			{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file", Arguments: `{"path":"a.go"}`}},
				Reasoning: &llm.Reasoning{State: &llm.ReasoningState{
					Provider: instanceID,
					Model:    "deepseek-v4-flash",
					Kind:     deepSeekReasoningItemsKind,
					Payload:  payload,
				}},
			},
			llm.ToolMessage(`package main`, "call_1"),
		},
	}, instanceID)
	if err != nil {
		t.Fatalf("toDeepSeekRequest: %v", err)
	}

	body := marshalJSONMap(t, req)
	input, _ := body["input"].([]any)
	if len(input) != 4 {
		t.Fatalf("input len = %d, body=%s", len(input), mustJSON(t, body))
	}
	user, _ := input[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("input[0] = %#v", input[0])
	}
	assertItemType(t, input[1], "reasoning")
	reasoning, _ := input[1].(map[string]any)
	content, _ := reasoning["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("reasoning content missing: %#v", reasoning)
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "reasoning_text" || part["text"] != "need the file" {
		t.Fatalf("reasoning part = %#v", part)
	}
	assertItemType(t, input[2], "function_call")
	assertItemType(t, input[3], "function_call_output")
}

func TestMapDeepSeekResponse_ToolCallAndUsage(t *testing.T) {
	var resp responses.Response
	if err := resp.UnmarshalJSON([]byte(`{
		"id":"resp_1","object":"response","status":"completed","model":"deepseek-v4-flash",
		"output":[
			{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"private"}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"}
		],
		"usage":{
			"input_tokens":10,"output_tokens":6,"total_tokens":16,
			"input_tokens_details":{"cached_tokens":4},
			"output_tokens_details":{"reasoning_tokens":3}
		}
	}`)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got, err := mapDeepSeekResponse(&resp, "deepseek:test", "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if got.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q", got.FinishReason)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != "call_1" || got.ToolCalls[0].Name != "read_file" {
		t.Fatalf("ToolCalls = %#v", got.ToolCalls)
	}
	if got.Usage.PromptTokens != 10 || got.Usage.CompletionTokens != 6 || got.Usage.CachedReadTokens != 4 || got.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if got.Usage.CacheMissTokens != 6 {
		t.Fatalf("CacheMissTokens = %d", got.Usage.CacheMissTokens)
	}
	if got.Reasoning == nil || got.Reasoning.State == nil || got.Reasoning.State.Kind != deepSeekReasoningItemsKind {
		t.Fatal("expected opaque reasoning continuation state")
	}
}

func TestMapDeepSeekResponse_IncompleteIsLength(t *testing.T) {
	var resp responses.Response
	if err := resp.UnmarshalJSON([]byte(`{
		"id":"resp_2","object":"response","status":"incomplete",
		"incomplete_details":{"reason":"max_output_tokens"},
		"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"partial"}]}],
		"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}
	}`)); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := mapDeepSeekResponse(&resp, "deepseek:test", "deepseek-v4-flash")
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if got.FinishReason != "length" {
		t.Fatalf("FinishReason = %q", got.FinishReason)
	}
	if got.Content != "partial" {
		t.Fatalf("Content = %q", got.Content)
	}
	if got.Reasoning != nil {
		t.Fatal("final answer must not keep reasoning continuation state")
	}
}

func TestDeepSeekStreamAccum_DeltasAndFinalResponse(t *testing.T) {
	events := []string{
		`{"type":"response.reasoning_text.delta","delta":"think ","sequence_number":1}`,
		`{"type":"response.output_text.delta","delta":"hello","sequence_number":2}`,
		`{"type":"response.output_text.delta","delta":" world","sequence_number":3}`,
		`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_s","object":"response","status":"completed","output":[{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":1}}}}`,
	}
	accum := newDeepSeekStreamAccum("deepseek:test", "deepseek-v4-flash")
	sink := &collectSink{}
	for i, raw := range events {
		var evt responses.ResponseStreamEventUnion
		if err := evt.UnmarshalJSON([]byte(raw)); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if err := accum.apply(evt, sink); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	got := accum.finalize(sink)
	if !sink.done {
		t.Fatal("expected OnDone")
	}
	if strings.Join(sink.deltas, "") != "hello world" {
		t.Fatalf("text deltas = %#v", sink.deltas)
	}
	if strings.Join(sink.reasoning, "") != "think " {
		t.Fatalf("reasoning deltas = %#v", sink.reasoning)
	}
	if got.Content != "hello world" || got.FinishReason != "stop" {
		t.Fatalf("result = %#v", got)
	}
	if got.Usage.ReasoningTokens != 1 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestDeepSeekProvider_Stream_HTTPGolden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "responses") {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ds-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeSSE := func(event, data string) {
			_, _ = w.Write([]byte("event: " + event + "\n"))
			_, _ = w.Write([]byte("data: " + data + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeSSE("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file","arguments":""}}`)
		writeSSE("response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":\"a.go\"}"}`)
		writeSSE("response.completed", `{"type":"response.completed","response":{"id":"resp_tool","object":"response","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"read_file","arguments":"{\"path\":\"a.go\"}"}],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":50},"output_tokens_details":{"reasoning_tokens":0}}}}`)
	}))
	defer srv.Close()

	p := NewDeepSeek("ds-key", srv.URL)
	sink := &collectSink{}
	got, err := p.Stream(context.Background(), llm.CallParams{
		Model:    "deepseek-v4-flash",
		Messages: []llm.Message{llm.UserMessage("read a.go")},
		Tools:    []llm.ToolDef{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
	}, sink)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !sink.done {
		t.Fatal("expected OnDone")
	}
	if got.FinishReason != "tool_calls" {
		t.Fatalf("FinishReason = %q", got.FinishReason)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("got %#v", got)
	}
	if got.Usage.CachedReadTokens != 50 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestDeepSeekReasoningNonStreamingToolContinuation(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "responses") {
			http.NotFound(w, r)
			return
		}
		body := decodeRequestBody(t, r)
		mu.Lock()
		requests = append(requests, body)
		call := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{
				"id":"resp_reason_1","object":"response","status":"completed","model":"deepseek-v4-flash",
				"output":[
					{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"private analysis"}]},
					{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"}
				],
				"usage":{"input_tokens":10,"output_tokens":6,"total_tokens":16,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":4}}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"resp_reason_2","object":"response","status":"completed","model":"deepseek-v4-flash",
			"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"final answer"}]}],
			"usage":{"input_tokens":20,"output_tokens":7,"total_tokens":27,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":5}}
		}`))
	}))
	defer srv.Close()

	p := NewDeepSeek("ds-key", srv.URL)
	params := llm.CallParams{
		Model:     "deepseek-v4-flash",
		Messages:  []llm.Message{llm.UserMessage("inspect a.go")},
		Tools:     []llm.ToolDef{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
		Reasoning: &llm.ReasoningRequest{Enabled: true, Effort: "medium"},
	}
	first, err := p.Call(context.Background(), params)
	if err != nil {
		t.Fatalf("first Call: %v", err)
	}
	if first.FinishReason != "tool_calls" || first.Reasoning == nil || first.Reasoning.State == nil {
		t.Fatalf("first = %#v", first)
	}

	second, err := p.Call(context.Background(), llm.CallParams{
		Model: "deepseek-v4-flash",
		Messages: []llm.Message{
			llm.UserMessage("inspect a.go"),
			first.ToAssistantMessage(),
			llm.ToolMessage("ok", "call_1"),
		},
	})
	if err != nil {
		t.Fatalf("second Call: %v", err)
	}
	if second.Content != "final answer" || second.Reasoning != nil {
		t.Fatalf("second = %#v", second)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("calls = %d", len(requests))
	}
	input, _ := requests[1]["input"].([]any)
	foundReasoning := false
	for _, item := range input {
		m, _ := item.(map[string]any)
		if m["type"] == "reasoning" {
			foundReasoning = true
			content, _ := m["content"].([]any)
			if len(content) == 0 {
				t.Fatalf("continuation dropped reasoning content: %#v", m)
			}
		}
	}
	if !foundReasoning {
		t.Fatalf("second request missing reasoning item: %s", mustJSON(t, requests[1]))
	}
}

func marshalJSONMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v\nraw=%s", err, raw)
	}
	return body
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertItemType(t *testing.T, item any, want string) {
	t.Helper()
	m, _ := item.(map[string]any)
	if m["type"] != want {
		t.Fatalf("item type = %#v, want %s (%#v)", m["type"], want, item)
	}
}
