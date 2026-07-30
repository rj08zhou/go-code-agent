package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

const secretBlob = `{"encrypted":"THIS-MUST-NEVER-LEAK"}`

func TestReasoningStateJSONAlwaysRedactsPayload(t *testing.T) {
	state := &ReasoningState{
		Provider: "openai",
		Model:    "o4-mini",
		Kind:     "openai.reasoning_content",
		Payload:  json.RawMessage(secretBlob),
	}

	// Every JSON path that could touch the state must redact: direct,
	// embedded in Reasoning, embedded in a full Message (transcripts).
	for name, v := range map[string]any{
		"state":     state,
		"reasoning": &Reasoning{Summary: "visible", State: state},
		"message": Message{
			Role:      RoleAssistant,
			Content:   "final answer",
			Reasoning: &Reasoning{Summary: "visible", State: state},
		},
	} {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(data), "THIS-MUST-NEVER-LEAK") {
			t.Fatalf("%s serialization leaked opaque payload: %s", name, data)
		}
	}

	// The redacted form still records that a payload existed.
	data, _ := json.Marshal(state)
	if !strings.Contains(string(data), `"payload_redacted":true`) {
		t.Fatalf("redacted marker missing: %s", data)
	}
}

func TestReasoningStateCompatibility(t *testing.T) {
	state := &ReasoningState{Provider: "openai", Model: "o4-mini"}
	if !state.Compatible("openai", "o4-mini") {
		t.Fatal("same provider/model must be compatible")
	}
	for _, tc := range [][2]string{
		{"anthropic", "o4-mini"}, // cross provider
		{"openai", "gpt-4.1"},    // cross model
	} {
		if state.Compatible(tc[0], tc[1]) {
			t.Fatalf("state must not be compatible with %s/%s", tc[0], tc[1])
		}
	}
	var nilState *ReasoningState
	if nilState.Compatible("openai", "o4-mini") {
		t.Fatal("nil state must never be compatible")
	}
}

func TestCompletionAndStreamResultCarryReasoning(t *testing.T) {
	r := &Reasoning{Summary: "thought about it"}
	if got := (&Completion{Content: "a", Reasoning: r}).ToAssistantMessage(); got.Reasoning != r {
		t.Fatal("Completion.ToAssistantMessage dropped reasoning")
	}
	if got := (&StreamResult{Content: "a", Reasoning: r}).ToAssistantMessage(); got.Reasoning != r {
		t.Fatal("StreamResult.ToAssistantMessage dropped reasoning")
	}
	// Reasoning stays optional: absent input yields absent output.
	if got := (&Completion{Content: "a"}).ToAssistantMessage(); got.Reasoning != nil {
		t.Fatal("nil reasoning must stay nil")
	}
}

func TestUsageIsZeroIncludesReasoningTokens(t *testing.T) {
	if (Usage{ReasoningTokens: 1}).IsZero() {
		t.Fatal("usage with only reasoning tokens must not be zero")
	}
	if !(Usage{}).IsZero() {
		t.Fatal("empty usage must be zero")
	}
}

func TestUsageAddIncludesEveryDimension(t *testing.T) {
	u := Usage{
		PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3,
		CachedReadTokens: 4, CacheMissTokens: 5, CacheCreateTokens: 6,
		ReasoningTokens: 7,
	}
	u.Add(Usage{
		PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30,
		CachedReadTokens: 40, CacheMissTokens: 50, CacheCreateTokens: 60,
		ReasoningTokens: 70,
	})
	want := Usage{
		PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33,
		CachedReadTokens: 44, CacheMissTokens: 55, CacheCreateTokens: 66,
		ReasoningTokens: 77,
	}
	if u != want {
		t.Fatalf("Usage.Add result = %+v, want %+v", u, want)
	}

	var nilUsage *Usage
	nilUsage.Add(Usage{ReasoningTokens: 1}) // must be nil-safe
}
