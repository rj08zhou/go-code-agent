package provider

import (
	"testing"

	"go-code-agent/internal/gateway"
)

func TestConfiguredProvidersHaveEndpointScopedInstanceIDs(t *testing.T) {
	constructors := []struct {
		name string
		new  func(string, string) gateway.Provider
	}{
		{name: "openai", new: NewOpenAI},
		{name: "anthropic", new: NewAnthropic},
		{name: "gemini", new: NewGemini},
		{name: "deepseek", new: NewDeepSeek},
	}

	for _, tc := range constructors {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.new("key-a", "https://endpoint-a.example/v1")
			aWithOtherKey := tc.new("key-b", "https://endpoint-a.example/v1")
			b := tc.new("key-a", "https://endpoint-b.example/v1")

			idA := gateway.ProviderInstanceID(a)
			if idA == "" || idA == a.Name() {
				t.Fatalf("provider did not expose an endpoint-scoped instance ID: %q", idA)
			}
			if idA != gateway.ProviderInstanceID(aWithOtherKey) {
				t.Fatal("API key must not influence or leak through provider instance identity")
			}
			if idA == gateway.ProviderInstanceID(b) {
				t.Fatal("different endpoints must have different provider instance IDs")
			}
		})
	}
}
