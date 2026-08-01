package provider

import (
	"testing"

	"go-code-agent/internal/model"
)

func TestConfiguredProvidersHaveEndpointScopedInstanceIDs(t *testing.T) {
	constructors := []struct {
		name string
		new  func(string, string) model.Provider
	}{
		{name: "openai", new: NewOpenAI},
		{name: "anthropic", new: NewAnthropic},
		{name: "gemini", new: NewGemini},
	}

	for _, tc := range constructors {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.new("key-a", "https://endpoint-a.example/v1")
			aWithOtherKey := tc.new("key-b", "https://endpoint-a.example/v1")
			b := tc.new("key-a", "https://endpoint-b.example/v1")

			idA := model.ProviderInstanceID(a)
			if idA == "" || idA == a.Name() {
				t.Fatalf("provider did not expose an endpoint-scoped instance ID: %q", idA)
			}
			if idA != model.ProviderInstanceID(aWithOtherKey) {
				t.Fatal("API key must not influence or leak through provider instance identity")
			}
			if idA == model.ProviderInstanceID(b) {
				t.Fatal("different endpoints must have different provider instance IDs")
			}
		})
	}
}
