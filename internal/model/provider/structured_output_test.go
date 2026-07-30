package provider

import (
	"errors"
	"net/http"
	"testing"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
)

func testStructuredOutput() *llm.StructuredOutput {
	return &llm.StructuredOutput{
		Name:        "test_result",
		Description: "A test result.",
		Schema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"ok": map[string]any{"type": "boolean"},
			},
			"required": []string{"ok"},
		},
	}
}

func TestToOpenAIResponseFormat(t *testing.T) {
	responseFormat, ok := toOpenAIResponseFormat(testStructuredOutput())
	if !ok {
		t.Fatal("expected OpenAI response format")
	}

	format := responseFormat.OfJSONSchema
	if format == nil {
		t.Fatal("OpenAI response_format.json_schema was not set")
	}
	if format.JSONSchema.Name != "test_result" {
		t.Fatalf("schema name = %q", format.JSONSchema.Name)
	}
	if strict := format.JSONSchema.Strict; !strict.Valid() || !strict.Value {
		t.Fatalf("strict = %#v, want true", strict)
	}
	if format.JSONSchema.Schema == nil {
		t.Fatal("schema body was not propagated")
	}
}

func TestToAnthropicOutputConfig(t *testing.T) {
	outputConfig, ok := toAnthropicOutputConfig(testStructuredOutput())
	if !ok {
		t.Fatal("expected Anthropic output config")
	}

	if outputConfig.Format.Schema == nil {
		t.Fatal("Anthropic output_config.format.schema was not set")
	}
	if got := outputConfig.Format.Schema["type"]; got != "object" {
		t.Fatalf("schema type = %#v", got)
	}
}

func TestToGeminiGenerationConfig(t *testing.T) {
	generationConfig, ok := toGeminiGenerationConfig(testStructuredOutput())
	if !ok {
		t.Fatal("expected Gemini generation config")
	}

	if got := generationConfig["responseMimeType"]; got != "application/json" {
		t.Fatalf("responseMimeType = %#v", got)
	}
	if generationConfig["responseSchema"] == nil {
		t.Fatal("Gemini responseSchema was not set")
	}
}

func TestProviderErrorAdapters(t *testing.T) {
	t.Run("OpenAI", func(t *testing.T) {
		source := &openai.Error{
			StatusCode: http.StatusTooManyRequests,
			Code:       "rate_limit_exceeded",
			Message:    "slow down",
		}
		got := requireProviderError(t, toOpenAIProviderError(source))
		assertProviderError(t, got, "openai", http.StatusTooManyRequests, "rate_limit_exceeded", true)
		if !errors.Is(got, source) {
			t.Fatal("OpenAI source error was not retained")
		}
	})

	t.Run("Anthropic", func(t *testing.T) {
		source := &anthropic.Error{}
		if err := source.UnmarshalJSON([]byte(
			`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)); err != nil {
			t.Fatalf("unmarshal Anthropic error: %v", err)
		}
		source.StatusCode = 529
		got := requireProviderError(t, toAnthropicProviderError(source))
		assertProviderError(t, got, "anthropic", 529, "overloaded_error", true)
		if !errors.Is(got, source) {
			t.Fatal("Anthropic source error was not retained")
		}
	})
}

func requireProviderError(t *testing.T, err error) *model.ProviderError {
	t.Helper()
	if err == nil {
		t.Fatal("expected provider error")
	}
	var providerErr *model.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("error type = %T, want *model.ProviderError", err)
	}
	return providerErr
}

func assertProviderError(
	t *testing.T,
	got *model.ProviderError,
	provider string,
	statusCode int,
	code string,
	retryable bool,
) {
	t.Helper()
	if got.Provider != provider || got.StatusCode != statusCode || got.Code != code || got.Retryable != retryable {
		t.Fatalf("ProviderError = %+v, want provider=%s status=%d code=%s retryable=%t",
			got, provider, statusCode, code, retryable)
	}
}
