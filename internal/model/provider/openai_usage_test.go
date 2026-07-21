package provider

import (
	"testing"

	"github.com/openai/openai-go"
)

func TestMapOpenAIUsage_DeepSeekCacheFields(t *testing.T) {
	var usage openai.CompletionUsage
	if err := usage.UnmarshalJSON([]byte(`{
		"prompt_tokens": 100,
		"completion_tokens": 12,
		"total_tokens": 112,
		"prompt_cache_hit_tokens": 80,
		"prompt_cache_miss_tokens": 20
	}`)); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}

	got := mapOpenAIUsage(usage)
	if got.PromptTokens != 100 || got.CompletionTokens != 12 || got.TotalTokens != 112 {
		t.Fatalf("unexpected basic usage: %+v", got)
	}
	if got.CachedReadTokens != 80 || got.CacheMissTokens != 20 {
		t.Fatalf("unexpected DeepSeek cache usage: %+v", got)
	}
}

func TestMapOpenAIUsage_StandardCachedTokens(t *testing.T) {
	var usage openai.CompletionUsage
	if err := usage.UnmarshalJSON([]byte(`{
		"prompt_tokens": 100,
		"completion_tokens": 12,
		"total_tokens": 112,
		"prompt_tokens_details": {"cached_tokens": 75}
	}`)); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}

	got := mapOpenAIUsage(usage)
	if got.CachedReadTokens != 75 {
		t.Fatalf("expected standard cached tokens=75, got %+v", got)
	}
}
