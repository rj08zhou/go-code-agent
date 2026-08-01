package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
)

func decodeRequestBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	defer r.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// httptest handlers run in another goroutine, so do not use FailNow.
		t.Errorf("decode request: %v", err)
		return map[string]any{}
	}
	return body
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestOpenAIReasoningNonStreamingToolContinuation(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body := decodeRequestBody(t, r)
		mu.Lock()
		requests = append(requests, body)
		call := len(requests)
		mu.Unlock()

		if call == 1 {
			writeJSON(t, w, `{
				"id":"chatcmpl-reason-1","object":"chat.completion","created":1,"model":"reasoner-test",
				"choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"private analysis","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":10,"completion_tokens":6,"total_tokens":16,"completion_tokens_details":{"reasoning_tokens":4}}
			}`)
			return
		}
		writeJSON(t, w, `{
			"id":"chatcmpl-reason-2","object":"chat.completion","created":2,"model":"reasoner-test",
			"choices":[{"index":0,"message":{"role":"assistant","content":"final answer","reasoning_content":"discard after final answer"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":7,"total_tokens":27,"reasoning_tokens":5}
		}`)
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", srv.URL+"/v1")
	params := llm.CallParams{
		Model:     "reasoner-test",
		Messages:  []llm.Message{llm.UserMessage("inspect a.go")},
		Tools:     []llm.ToolDef{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
		Reasoning: &llm.ReasoningRequest{Enabled: true, Effort: "HIGH"},
	}
	first, err := p.Call(context.Background(), params)
	if err != nil {
		t.Fatalf("first Call: %v", err)
	}
	if first.Usage.ReasoningTokens != 4 {
		t.Fatalf("first reasoning tokens = %d", first.Usage.ReasoningTokens)
	}
	if first.Reasoning == nil || first.Reasoning.State == nil {
		t.Fatalf("tool response lost reasoning state: %#v", first.Reasoning)
	}
	state := first.Reasoning.State
	if state.Provider != model.ProviderInstanceID(p) || state.Model != params.Model || state.Kind != openAIReasoningContentKind {
		t.Fatalf("state binding = %#v", state)
	}
	var private string
	if err := json.Unmarshal(state.Payload, &private); err != nil || private != "private analysis" {
		t.Fatalf("state payload = %q err=%v", state.Payload, err)
	}

	params.Messages = append(params.Messages, first.ToAssistantMessage())
	params.Messages = append(params.Messages, llm.ToolMessage("package main", "call_1"))
	second, err := p.Call(context.Background(), params)
	if err != nil {
		t.Fatalf("second Call: %v", err)
	}
	if second.Content != "final answer" || second.Usage.ReasoningTokens != 5 {
		t.Fatalf("second completion = %#v", second)
	}
	if second.Reasoning != nil {
		t.Fatalf("final answer retained private reasoning without a tool continuation: %#v", second.Reasoning)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("request count = %d", len(requests))
	}
	for i, body := range requests {
		if body["reasoning_effort"] != "high" {
			t.Fatalf("request %d reasoning_effort = %#v", i+1, body["reasoning_effort"])
		}
	}
	messages, ok := requests[1]["messages"].([]any)
	if !ok || len(messages) < 2 {
		t.Fatalf("second request messages = %#v", requests[1]["messages"])
	}
	assistant, ok := messages[1].(map[string]any)
	if !ok {
		t.Fatalf("assistant message = %#v", messages[1])
	}
	if assistant["reasoning_content"] != "private analysis" {
		t.Fatalf("reasoning_content continuation = %#v", assistant["reasoning_content"])
	}
	if _, ok := assistant["tool_calls"]; !ok {
		t.Fatalf("assistant continuation lost tool_calls: %#v", assistant)
	}
}

func TestOpenAIReasoningStreamingIsOpaqueAndCounted(t *testing.T) {
	requestCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCh <- decodeRequestBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"private "},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"analysis"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"checking","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":13,"completion_tokens_details":{"reasoning_tokens":3}}}`,
		}
		for _, chunk := range chunks {
			_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", srv.URL+"/v1")
	sink := &collectSink{}
	result, err := p.Stream(context.Background(), llm.CallParams{
		Model:     "reasoner-test",
		Messages:  []llm.Message{llm.UserMessage("inspect")},
		MaxTokens: 4096,
		Reasoning: &llm.ReasoningRequest{Enabled: true, Effort: "medium"},
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(sink.deltas, "") != "checking" {
		t.Fatalf("user-visible deltas leaked reasoning: %#v", sink.deltas)
	}
	if result.Content != "checking" || result.Usage.ReasoningTokens != 3 {
		t.Fatalf("stream result = %#v", result)
	}
	// Chat Completions exposes no reasoning summary; only opaque state.
	if result.Reasoning == nil || result.Reasoning.Summary != "" || result.Reasoning.State == nil {
		t.Fatalf("stream reasoning = %#v", result.Reasoning)
	}
	request := <-requestCh
	var private string
	if err := json.Unmarshal(result.Reasoning.State.Payload, &private); err != nil || private != "private analysis" {
		t.Fatalf("stream state payload = %q err=%v", result.Reasoning.State.Payload, err)
	}
	if request["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort = %#v", request["reasoning_effort"])
	}
	// Reasoning models reject the legacy max_tokens field with HTTP 400.
	if request["max_completion_tokens"] != float64(4096) {
		t.Fatalf("max_completion_tokens = %#v", request["max_completion_tokens"])
	}
	if _, legacy := request["max_tokens"]; legacy {
		t.Fatal("reasoning request must not send legacy max_tokens")
	}
}

func TestOpenAIReasoningRejectsInvalidEffortBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		writeJSON(t, w, `{}`)
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", srv.URL+"/v1")
	// "xhigh" must be rejected too: it is not an official Chat Completions
	// value, and whitelisting it would forward a guaranteed HTTP 400.
	for _, effort := range []string{"untrusted-value", "xhigh", "max"} {
		_, err := p.Call(context.Background(), llm.CallParams{
			Model:     "reasoner-test",
			Messages:  []llm.Message{llm.UserMessage("task")},
			Reasoning: &llm.ReasoningRequest{Enabled: true, Effort: effort},
		})
		if err == nil || !strings.Contains(err.Error(), "unsupported OpenAI reasoning effort") {
			t.Fatalf("effort %q: error = %v", effort, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid request reached HTTP server %d times", requests.Load())
	}
}

func TestOpenAINonReasoningKeepsLegacyMaxTokens(t *testing.T) {
	requestCh := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCh <- decodeRequestBody(t, r)
		writeJSON(t, w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	p := NewOpenAI("test-key", srv.URL+"/v1")
	if _, err := p.Call(context.Background(), llm.CallParams{
		Model:     "gpt-4o",
		Messages:  []llm.Message{llm.UserMessage("hi")},
		MaxTokens: 512,
	}); err != nil {
		t.Fatal(err)
	}
	request := <-requestCh
	if request["max_tokens"] != float64(512) {
		t.Fatalf("non-reasoning request must keep max_tokens, got %#v", request["max_tokens"])
	}
	if _, newer := request["max_completion_tokens"]; newer {
		t.Fatal("non-reasoning request must not switch to max_completion_tokens")
	}
}

func TestOpenAIReasoningStateRequiresMatchingBindingAndToolCall(t *testing.T) {
	state := &llm.ReasoningState{
		Provider: "endpoint-a",
		Model:    "model-a",
		Kind:     openAIReasoningContentKind,
		Payload:  json.RawMessage(`"private"`),
	}
	base := llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_file", Arguments: `{}`}},
		Reasoning: &llm.Reasoning{State: state},
	}

	for _, tc := range []struct {
		name       string
		message    llm.Message
		providerID string
		modelID    string
		want       bool
	}{
		{name: "matching", message: base, providerID: "endpoint-a", modelID: "model-a", want: true},
		{name: "other endpoint", message: base, providerID: "endpoint-b", modelID: "model-a"},
		{name: "other model", message: base, providerID: "endpoint-a", modelID: "model-b"},
		{name: "no tool call", message: llm.Message{Role: llm.RoleAssistant, Reasoning: base.Reasoning}, providerID: "endpoint-a", modelID: "model-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := toOpenAIMessages([]llm.Message{tc.message}, tc.providerID, tc.modelID)
			data, err := json.Marshal(wire[0])
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(string(data), `"reasoning_content":"private"`)
			if got != tc.want {
				t.Fatalf("wire = %s, reasoning_content present=%v want=%v", data, got, tc.want)
			}
		})
	}
}
