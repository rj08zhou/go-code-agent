package llm

import (
	"context"
	"errors"
)

// Gemini Provider - placeholder, returns "not implemented"
//
// This is intentionally a stub. The interface is wired up and the
// provider is registered so that MODEL_ID=gemini-* or
// LLM_PROVIDER=gemini route here, where they get a clear error
// instead of silently falling back to OpenAI.
//
// To finish this implementation:
//   - depend on a Gemini SDK (e.g. google.golang.org/genai)
//   - mirror the shape of provider_openai.go / provider_anthropic.go
//   - fill Call + Stream using the neutral CallParams / StreamSink
//
// The rest of the codebase requires zero changes once this file is
// completed - llm_client.go and every call site already talk through
// the Provider interface.

type geminiProvider struct{}

func (p *geminiProvider) Name() string { return "gemini" }

var errGeminiNotImplemented = errors.New(
	"gemini provider is not implemented yet. " +
		"Unset LLM_PROVIDER or set it to openai/anthropic, " +
		"or finish provider_gemini.go")

func (p *geminiProvider) Call(ctx context.Context, _ CallParams) (*Completion, error) {
	return nil, errGeminiNotImplemented
}

func (p *geminiProvider) Stream(ctx context.Context, _ CallParams, _ StreamSink) (*StreamResult, error) {
	return nil, errGeminiNotImplemented
}

func init() {
	RegisterProvider(&geminiProvider{})
}
