package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go-code-agent/internal/config"
)

func TestExtractTextHTMLReturnsVisibleText(t *testing.T) {
	body := []byte(`<html><body><p>Hello world</p><script>var x=1;</script></body></html>`)
	text, noStatic := extractText(body, "text/html; charset=utf-8")
	if noStatic {
		t.Fatalf("expected static text, got noStaticText=true")
	}
	if !strings.Contains(text, "Hello world") {
		t.Fatalf("text = %q, want it to contain visible content", text)
	}
	if strings.Contains(text, "var x=1") {
		t.Fatalf("script content leaked into text: %q", text)
	}
}

func TestExtractTextSPAFallsBackToMeta(t *testing.T) {
	body := []byte(`<html><head><title>Docs</title>` +
		`<meta name="description" content="API reference">` +
		`</head><body><div id="root"></div><script>render()</script></body></html>`)
	text, noStatic := extractText(body, "text/html")
	if !noStatic {
		t.Fatalf("expected noStaticText=true for JS-rendered page")
	}
	if !strings.Contains(text, "Docs") || !strings.Contains(text, "API reference") {
		t.Fatalf("fallback text = %q, want title + description", text)
	}
}

func TestExtractTextSPAWithoutMeta(t *testing.T) {
	body := []byte(`<html><body><div id="root"></div><script>render()</script></body></html>`)
	text, noStatic := extractText(body, "text/html")
	if !noStatic {
		t.Fatalf("expected noStaticText=true")
	}
	if !strings.Contains(text, "JavaScript") && !strings.Contains(text, "chrome") && !strings.Contains(text, "article") {
		t.Fatalf("expected diagnostic note, got %q", text)
	}
}

func TestHTMLToTextPrefersMainAndSkipsChrome(t *testing.T) {
	raw := `<html><body>
		<div class="navbar">Join our Discord</div>
		<main><article><p>This is the real article body with enough substance to keep.</p></article></main>
		<footer>Copyright 2026</footer>
	</body></html>`
	text := HTMLToText(raw)
	if !strings.Contains(text, "real article body") {
		t.Fatalf("missing article body: %q", text)
	}
	if strings.Contains(text, "Discord") || strings.Contains(text, "Copyright") {
		t.Fatalf("chrome leaked into text: %q", text)
	}
}

func TestExtractTextChromeOnlyFallsBackToMeta(t *testing.T) {
	body := []byte(`<html><head><title>Cursor Deep Dive</title>
		<meta name="description" content="Architecture overview">
		</head><body>
		<div class="nav">Join Discord</div>
		<div class="promo">New Book</div>
		<div class="sidebar">Subscribe</div>
		</body></html>`)
	text, noStatic := extractText(body, "text/html")
	if !noStatic {
		t.Fatalf("expected noStaticText for chrome-only page, got text=%q", text)
	}
	if !strings.Contains(text, "Cursor Deep Dive") || !strings.Contains(text, "Architecture overview") {
		t.Fatalf("want title+meta fallback, got %q", text)
	}
}

// hangingProvider blocks until its context is cancelled, simulating a stuck
// backend that must not consume the whole chain budget.
type hangingProvider struct{ name string }

func (h *hangingProvider) Name() string { return h.name }
func (h *hangingProvider) Search(ctx context.Context, _ string, _ int) ([]SearchResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type okProvider struct{ name string }

func (o *okProvider) Name() string { return o.name }
func (o *okProvider) Search(_ context.Context, _ string, _ int) ([]SearchResult, error) {
	return []SearchResult{{Title: "ok", URL: "https://ok.test"}}, nil
}

func TestChainFallsBackPastHangingBackend(t *testing.T) {
	chain := &chainSearchProvider{backends: []SearchProvider{
		&hangingProvider{name: "slow"},
		&okProvider{name: "fast"},
	}}
	// Overall budget comfortably larger than one per-backend timeout so the
	// fallback has room to run.
	ctx, cancel := context.WithTimeout(context.Background(), config.WebSearchPerBackendTimeout+5*time.Second)
	defer cancel()

	start := time.Now()
	results, err := chain.Search(ctx, "q", 3)
	if err != nil {
		t.Fatalf("chain.Search error = %v", err)
	}
	if len(results) != 1 || results[0].Title != "ok" {
		t.Fatalf("results = %+v, want fallback result from fast backend", results)
	}
	if elapsed := time.Since(start); elapsed < config.WebSearchPerBackendTimeout {
		t.Fatalf("elapsed %v: expected to wait out the per-backend timeout on the hung backend", elapsed)
	}
}

func TestChainStopsWhenBudgetExhausted(t *testing.T) {
	chain := &chainSearchProvider{backends: []SearchProvider{&okProvider{name: "a"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already exhausted
	_, err := chain.Search(ctx, "q", 3)
	if err == nil {
		t.Fatal("expected error when budget already exhausted")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx err = %v", ctx.Err())
	}
}
