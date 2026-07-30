package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
)

type reasoningProviderWithoutIdentity struct {
	seen llm.CallParams
}

func (p *reasoningProviderWithoutIdentity) Name() string { return "legacy-reasoner" }
func (p *reasoningProviderWithoutIdentity) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{Reasoning: true}
}
func (p *reasoningProviderWithoutIdentity) Call(_ context.Context, params llm.CallParams) (*llm.Completion, error) {
	p.seen = params
	return &llm.Completion{Content: "ok"}, nil
}
func (p *reasoningProviderWithoutIdentity) Stream(_ context.Context, params llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
	p.seen = params
	sink.OnDone()
	return &llm.StreamResult{Content: "ok"}, nil
}

func reasoningMessages(provider, model string) []llm.Message {
	return []llm.Message{
		llm.UserMessage("task"),
		{
			Role:    llm.RoleAssistant,
			Content: "working",
			Reasoning: &llm.Reasoning{
				Summary: "visible summary",
				State: &llm.ReasoningState{
					Provider: provider,
					Model:    model,
					Kind:     "test.blob",
					Payload:  json.RawMessage(`"opaque"`),
				},
			},
		},
	}
}

func TestGatewayStripsReasoningOptInForUnsupportedProvider(t *testing.T) {
	var seen llm.CallParams
	p := &capsProvider{
		name: "plain",
		caps: ProviderCapabilities{}, // no Reasoning support
		callFn: func(_ context.Context, params llm.CallParams) (*llm.Completion, error) {
			seen = params
			return &llm.Completion{Content: "ok"}, nil
		},
	}
	g := NewGateway(p, NewRoleThrottle(10))

	_, err := g.Call(context.Background(), "lead", llm.CallParams{
		Model:     "m1",
		Messages:  reasoningMessages("plain", "m1"),
		Reasoning: &llm.ReasoningRequest{Enabled: true, Effort: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Reasoning != nil {
		t.Fatal("reasoning opt-in must be stripped for a provider without the capability")
	}
	if got := seen.Messages[1].Reasoning; got == nil || got.Summary != "visible summary" || got.State != nil {
		t.Fatalf("unsupported provider must keep summary but strip opaque state, got %#v", got)
	}
}

func TestGatewayFailsClosedWhenReasoningProviderLacksInstanceIdentity(t *testing.T) {
	p := &reasoningProviderWithoutIdentity{}
	g := NewGateway(p, NewRoleThrottle(10))
	_, err := g.Call(context.Background(), "lead", llm.CallParams{
		Model:     "m1",
		Messages:  reasoningMessages("legacy-reasoner", "m1"),
		Reasoning: &llm.ReasoningRequest{Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.seen.Reasoning != nil || p.seen.Messages[1].Reasoning.State != nil {
		t.Fatalf("provider without instance identity received reasoning state: %#v", p.seen)
	}
}

func TestGatewayKeepsReasoningOptInForSupportedProvider(t *testing.T) {
	var seen llm.CallParams
	p := &capsProvider{
		name: "reasoner",
		caps: ProviderCapabilities{Reasoning: true},
		callFn: func(_ context.Context, params llm.CallParams) (*llm.Completion, error) {
			seen = params
			return &llm.Completion{Content: "ok"}, nil
		},
	}
	g := NewGateway(p, NewRoleThrottle(10))

	_, err := g.Call(context.Background(), "lead", llm.CallParams{
		Model:     "m1",
		Messages:  []llm.Message{llm.UserMessage("task")},
		Reasoning: &llm.ReasoningRequest{Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Reasoning == nil || !seen.Reasoning.Enabled {
		t.Fatal("reasoning opt-in must reach a capable provider unchanged")
	}
}

func TestGatewayDropsForeignReasoningStateButKeepsSummary(t *testing.T) {
	var seen llm.CallParams
	p := &capsProvider{
		name: "other-provider",
		caps: ProviderCapabilities{Reasoning: true},
		callFn: func(_ context.Context, params llm.CallParams) (*llm.Completion, error) {
			seen = params
			return &llm.Completion{Content: "ok"}, nil
		},
	}
	g := NewGateway(p, NewRoleThrottle(10))

	original := reasoningMessages("origin-provider", "m1")
	_, err := g.Call(context.Background(), "lead", llm.CallParams{
		Model:    "m1",
		Messages: original,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := seen.Messages[1].Reasoning
	if got == nil || got.Summary != "visible summary" {
		t.Fatalf("summary must survive state stripping, got %#v", got)
	}
	if got.State != nil {
		t.Fatal("foreign opaque state must be dropped, not forwarded")
	}
	// Copy-on-write: the caller's message slice must stay intact for retries
	// against the original provider.
	if original[1].Reasoning.State == nil {
		t.Fatal("caller's original messages were mutated")
	}
}

func TestGatewayForwardsCompatibleReasoningState(t *testing.T) {
	var seen llm.CallParams
	p := &capsProvider{
		name:       "origin-provider",
		instanceID: "origin-instance",
		caps:       ProviderCapabilities{Reasoning: true},
		callFn: func(_ context.Context, params llm.CallParams) (*llm.Completion, error) {
			seen = params
			return &llm.Completion{Content: "ok"}, nil
		},
	}
	g := NewGateway(p, NewRoleThrottle(10))

	_, err := g.Call(context.Background(), "lead", llm.CallParams{
		Model:    "m1",
		Messages: reasoningMessages("origin-instance", "m1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	state := seen.Messages[1].Reasoning.State
	if state == nil || string(state.Payload) != `"opaque"` {
		t.Fatalf("compatible state must be forwarded verbatim, got %#v", state)
	}
}

func TestGatewayStripsStateAcrossSameProviderTypeDifferentInstances(t *testing.T) {
	var seen llm.CallParams
	p := &capsProvider{
		name:       "openai",
		instanceID: "openai:endpoint-b",
		caps:       ProviderCapabilities{Reasoning: true},
		callFn: func(_ context.Context, params llm.CallParams) (*llm.Completion, error) {
			seen = params
			return &llm.Completion{Content: "ok"}, nil
		},
	}
	g := NewGateway(p, NewRoleThrottle(10))
	_, err := g.Call(context.Background(), "lead", llm.CallParams{
		Model:    "same-model",
		Messages: reasoningMessages("openai:endpoint-a", "same-model"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Messages[1].Reasoning.State != nil {
		t.Fatal("same provider type/model but different endpoint instance must not receive opaque state")
	}
}

func TestGatewayFallbackStripsPrimaryInstanceState(t *testing.T) {
	primary := &capsProvider{
		name:       "openai",
		instanceID: "openai:endpoint-a",
		caps:       ProviderCapabilities{Reasoning: true},
		err:        errors.New("invalid_api_key"),
	}
	var seenFallback llm.CallParams
	fallback := &capsProvider{
		name:       "openai",
		instanceID: "openai:endpoint-b",
		caps:       ProviderCapabilities{Reasoning: true},
		callFn: func(_ context.Context, params llm.CallParams) (*llm.Completion, error) {
			seenFallback = params
			return &llm.Completion{Content: "ok"}, nil
		},
	}
	g := NewGateway(primary, NewRoleThrottle(10))
	g.SetFallbacks("lead", fallback)

	_, err := g.Call(context.Background(), "lead", llm.CallParams{
		Model:    "same-model",
		Messages: reasoningMessages("openai:endpoint-a", "same-model"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := seenFallback.Messages[1].Reasoning; got == nil || got.State != nil {
		t.Fatalf("fallback instance received primary opaque state: %#v", got)
	}
}

func TestGatewayStreamAppliesUnsupportedReasoningStripping(t *testing.T) {
	var seen llm.CallParams
	p := &capsProvider{
		name: "plain",
		caps: ProviderCapabilities{},
		streamFn: func(_ context.Context, params llm.CallParams, sink StreamSink) (*llm.StreamResult, error) {
			seen = params
			sink.OnDone()
			return &llm.StreamResult{Content: "ok"}, nil
		},
	}
	g := NewGateway(p, NewRoleThrottle(10))
	_, err := g.Stream(context.Background(), "lead", llm.CallParams{
		Model:     "m1",
		Messages:  reasoningMessages("plain", "m1"),
		Reasoning: &llm.ReasoningRequest{Enabled: true},
	}, &collectTestSink{})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Reasoning != nil || seen.Messages[1].Reasoning.State != nil {
		t.Fatalf("stream path did not strip unsupported reasoning: %#v", seen)
	}
}

func TestStableProviderInstanceIDIsStableAndDoesNotExposeEndpoint(t *testing.T) {
	const endpoint = "https://user:secret@example.test/v1/"
	a := StableProviderInstanceID("OpenAI", endpoint)
	b := StableProviderInstanceID("openai", strings.TrimSuffix(endpoint, "/"))
	if a != b {
		t.Fatalf("normalized endpoint IDs differ: %q != %q", a, b)
	}
	if strings.Contains(a, "example.test") || strings.Contains(a, "secret") {
		t.Fatalf("instance ID exposed endpoint material: %q", a)
	}
	if a == StableProviderInstanceID("openai", "https://other.test/v1") {
		t.Fatal("different endpoints produced the same instance ID")
	}
}
