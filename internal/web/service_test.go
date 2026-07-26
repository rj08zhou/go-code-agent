package web

import (
	"context"
	"strings"
	"testing"
)

type serviceSearchProviderFake struct {
	limit int
}

func (f *serviceSearchProviderFake) Name() string { return "fake" }

func (f *serviceSearchProviderFake) Search(_ context.Context, _ string, limit int) ([]SearchResult, error) {
	f.limit = limit
	return []SearchResult{{Title: "Example", URL: "https://example.test", Snippet: "A result"}}, nil
}

func TestServiceSearchFormatsResults(t *testing.T) {
	fake := &serviceSearchProviderFake{}
	service := NewService(fake)

	output, err := service.Search(context.Background(), "query")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if fake.limit != defaultSearchLimit {
		t.Fatalf("Search() limit = %d, want %d", fake.limit, defaultSearchLimit)
	}
	if !strings.Contains(output, "[Example](https://example.test)") || !strings.Contains(output, "A result") {
		t.Fatalf("Search() output = %q", output)
	}
}

func TestServiceFetchReturnsValidationError(t *testing.T) {
	service := NewService(nil)
	if _, err := service.Fetch(context.Background(), "ftp://example.test"); err == nil {
		t.Fatal("Fetch() expected unsupported scheme error")
	}
}

func TestFormatFetchResultUntrustedWrapper(t *testing.T) {
	out := formatFetchResult(&FetchResult{
		URL:        "https://example.test/page",
		StatusCode: 200,
		Text:       "Hello article body",
	}, nil)
	if !strings.Contains(out, "URL: https://example.test/page") {
		t.Fatalf("missing URL: %q", out)
	}
	if !strings.Contains(out, "Status: 200") {
		t.Fatalf("missing Status: %q", out)
	}
	if !strings.Contains(out, "BEGIN UNTRUSTED PAGE CONTENT") || !strings.Contains(out, "END UNTRUSTED PAGE CONTENT") {
		t.Fatalf("missing untrusted markers: %q", out)
	}
	if !strings.Contains(out, "Hello article body") {
		t.Fatalf("missing body: %q", out)
	}
}
