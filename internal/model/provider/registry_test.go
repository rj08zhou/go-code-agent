package provider

import (
	"testing"

	"go-code-agent/internal/config"
	"go-code-agent/internal/model"
)

func TestJudgeProviderUsesDedicatedCredentials(t *testing.T) {
	reg := NewRegistry()
	// Main agent only has OpenAI; Judge asks for Anthropic with its own key.
	reg.Register(NewOpenAI("main-openai-key", "https://api.openai.com/v1"))

	cfg := &config.Config{
		JudgeProvider: "anthropic",
		JudgeModel:    "claude-haiku-4.5",
		JudgeAPIKey:   "judge-anthropic-key",
		JudgeBaseURL:  "https://judge.example.com",
	}
	jp := reg.JudgeProvider(cfg)
	if jp == nil {
		t.Fatal("JudgeProvider returned nil with dedicated credentials")
	}
	if jp.Name() != "anthropic" {
		t.Fatalf("provider name = %q, want anthropic", jp.Name())
	}
	wantID := model.StableProviderInstanceID("anthropic", "https://judge.example.com")
	if id := model.ProviderInstanceID(jp); id != wantID {
		t.Fatalf("instance ID = %q, want %q (isolated from main agent)", id, wantID)
	}
}

func TestJudgeProviderFallsBackToRegisteredProviderWithoutDedicatedCreds(t *testing.T) {
	reg := NewRegistry()
	main := NewAnthropic("main-key", "https://api.anthropic.com")
	reg.Register(main)

	cfg := &config.Config{
		JudgeModel: "claude-haiku-4.5",
	}
	jp := reg.JudgeProvider(cfg)
	if jp != main {
		t.Fatalf("expected registered anthropic provider reuse, got %#v", jp)
	}
}

func TestJudgeProviderRefusesUnknownBuilderWithDedicatedCreds(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewOpenAI("main-key", ""))

	cfg := &config.Config{
		JudgeProvider: "gemini",
		JudgeAPIKey:   "gemini-key",
	}
	if jp := reg.JudgeProvider(cfg); jp != nil {
		t.Fatalf("expected nil when dedicated creds request an unregistered builder, got %s", jp.Name())
	}
}
