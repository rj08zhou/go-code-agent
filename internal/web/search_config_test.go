package web

import "testing"

func TestSearchConfigFromEnv(t *testing.T) {
	t.Setenv("WEB_SEARCH_PROVIDER", "tavily")
	t.Setenv("WEB_SEARCH_API_KEY", "test-key")
	t.Setenv("SEARXNG_URL", "https://search.example")
	t.Setenv("SEARXNG_INSTANCES", "https://one.example, https://two.example")
	cfg := SearchConfigFromEnv()
	if cfg.Provider != "tavily" || cfg.APIKey != "test-key" || cfg.SearxngURL == "" || cfg.SearxngInstances == "" {
		t.Fatalf("environment search configuration not loaded: %#v", cfg)
	}
	if _, ok := NewSearchProvider(cfg).(*tavilySearchProvider); !ok {
		t.Fatalf("expected tavily search provider, got %T", NewSearchProvider(cfg))
	}
}
