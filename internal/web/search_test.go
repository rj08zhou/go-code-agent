package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ddgSampleHTML is a real DuckDuckGo Lite response captured for a
// "golang" query (see testdata/ddg_lite_sample.html) - parsing against
// a real fixture rather than a hand-written stub catches drift in the
// actual markup DDG serves (e.g. the exact class names, sponsored-row
// structure) that a synthetic sample could miss or accidentally match
// too easily.
func loadDDGSample(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/ddg_lite_sample.html")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return string(data)
}

func TestParseDDGLiteHTML_RealFixture(t *testing.T) {
	html := loadDDGSample(t)
	results, err := parseDDGLiteHTML(html, 10)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result from the real fixture")
	}

	// The fixture's first organic (non-sponsored) result is go.dev.
	var first *SearchResult
	for i := range results {
		if strings.Contains(results[i].URL, "go.dev") {
			first = &results[i]
			break
		}
	}
	if first == nil {
		t.Fatalf("expected a go.dev result among: %+v", results)
	}
	if !strings.Contains(first.Title, "Go Programming Language") {
		t.Errorf("Title = %q, want it to mention 'Go Programming Language'", first.Title)
	}
	if !strings.Contains(first.Snippet, "open source programming language") {
		t.Errorf("Snippet = %q, want the go.dev abstract", first.Snippet)
	}

	// Sponsored rows (udemy.com ad) must NOT leak into results.
	for _, r := range results {
		if strings.Contains(r.URL, "duckduckgo.com/y.js") {
			t.Errorf("sponsored/ad result leaked into results: %+v", r)
		}
	}
}

func TestParseDDGLiteHTML_RespectsLimit(t *testing.T) {
	html := loadDDGSample(t)
	results, err := parseDDGLiteHTML(html, 2)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(results) > 2 {
		t.Errorf("got %d results, limit was 2", len(results))
	}
}

func TestParseDDGLiteHTML_EmptyInput(t *testing.T) {
	results, err := parseDDGLiteHTML("<html><body></body></html>", 10)
	if err != nil {
		t.Fatalf("unexpected error on empty page: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results, got %d", len(results))
	}
}

// TestChainSearcher_FallsThroughOnFailure verifies the core downgrade
// behavior: a failing/empty first backend does not abort the whole
// search - the chain moves on to the next, and only reports failure
// once every backend has failed.
func TestChainSearcher_FallsThroughOnFailure(t *testing.T) {
	failing := &stubSearcher{name: "failing", err: errFake}
	empty := &stubSearcher{name: "empty", results: nil}
	working := &stubSearcher{name: "working", results: []SearchResult{{Title: "T", URL: "https://x.com"}}}

	c := &chainSearcher{backends: []Searcher{failing, empty, working}}
	results, err := c.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("expected chain to succeed via the working backend, got: %v", err)
	}
	if len(results) != 1 || results[0].Title != "T" {
		t.Errorf("expected the working backend's result, got: %+v", results)
	}
}

func TestChainSearcher_AllFailReturnsAggregateError(t *testing.T) {
	c := &chainSearcher{backends: []Searcher{
		&stubSearcher{name: "a", err: errFake},
		&stubSearcher{name: "b", results: nil},
	}}
	_, err := c.Search(context.Background(), "q", 5)
	if err == nil {
		t.Fatal("expected an error when all backends fail")
	}
	if !strings.Contains(err.Error(), "a:") || !strings.Contains(err.Error(), "b:") {
		t.Errorf("expected aggregate error to mention both backends, got: %v", err)
	}
}

type stubSearcher struct {
	name    string
	results []SearchResult
	err     error
}

func (s *stubSearcher) Name() string { return s.name }
func (s *stubSearcher) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	return s.results, s.err
}

var errFake = fakeErr("boom")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

// TestSearxngSearcher_ParsesJSONAPI exercises the SearXNG backend
// against a local httptest server implementing the documented JSON
// response shape, since we cannot rely on any specific public
// instance being reachable/stable in tests.
func TestSearxngSearcher_ParsesJSONAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("expected format=json query param, got %q", r.URL.RawQuery)
		}
		resp := searxngResponse{Results: []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		}{
			{Title: "Result A", URL: "https://a.example", Content: "snippet a"},
			{Title: "Result B", URL: "https://b.example", Content: "snippet b"},
		}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	os.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	defer os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")

	s := newSearxngSearcher(srv.URL)
	results, err := s.Search(context.Background(), "golang", 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) != 2 || results[0].Title != "Result A" || results[0].Snippet != "snippet a" {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestSearxngSearcher_NonJSONInstanceErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden) // many public instances disable ?format=json
	}))
	defer srv.Close()

	os.Setenv("WEB_ALLOW_PRIVATE_IPS", "1")
	defer os.Unsetenv("WEB_ALLOW_PRIVATE_IPS")

	s := newSearxngSearcher(srv.URL)
	if _, err := s.Search(context.Background(), "golang", 10); err == nil {
		t.Error("expected an error for a non-200 response")
	}
}

// TestNewDefaultSearcher_ProviderSelection is a lightweight assertion
// on WHICH concrete type gets built for each env combination - it
// does not perform any network I/O.
func TestNewDefaultSearcher_ProviderSelection(t *testing.T) {
	clearSearchEnv := func() {
		os.Unsetenv("WEB_SEARCH_PROVIDER")
		os.Unsetenv("WEB_SEARCH_API_KEY")
		os.Unsetenv("SEARXNG_URL")
		os.Unsetenv("SEARXNG_INSTANCES")
	}
	defer clearSearchEnv()

	t.Run("explicit tavily with key", func(t *testing.T) {
		clearSearchEnv()
		os.Setenv("WEB_SEARCH_PROVIDER", "tavily")
		os.Setenv("WEB_SEARCH_API_KEY", "tvly-xxx")
		s := NewDefaultSearcher()
		if _, ok := s.(*tavilySearcher); !ok {
			t.Errorf("expected *tavilySearcher, got %T", s)
		}
	})

	t.Run("explicit tavily without key falls back to chain", func(t *testing.T) {
		clearSearchEnv()
		os.Setenv("WEB_SEARCH_PROVIDER", "tavily")
		s := NewDefaultSearcher()
		if _, ok := s.(*chainSearcher); !ok {
			t.Errorf("expected *chainSearcher when tavily has no key, got %T", s)
		}
	})

	t.Run("default with explicit SEARXNG_URL", func(t *testing.T) {
		clearSearchEnv()
		os.Setenv("SEARXNG_URL", "https://my-trusted-instance.example")
		s := NewDefaultSearcher()
		c, ok := s.(*chainSearcher)
		if !ok || len(c.backends) != 2 {
			t.Fatalf("expected chain of [searxng(trusted), ddg], got %T %+v", s, s)
		}
		if _, ok := c.backends[0].(*searxngSearcher); !ok {
			t.Errorf("expected first backend to be searxngSearcher, got %T", c.backends[0])
		}
		if _, ok := c.backends[len(c.backends)-1].(*ddgSearcher); !ok {
			t.Errorf("expected last backend to be ddgSearcher, got %T", c.backends[len(c.backends)-1])
		}
	})

	t.Run("zero-config default uses public instances then ddg", func(t *testing.T) {
		clearSearchEnv()
		s := NewDefaultSearcher()
		c, ok := s.(*chainSearcher)
		if !ok {
			t.Fatalf("expected *chainSearcher, got %T", s)
		}
		if len(c.backends) != len(searxngPublicInstances)+1 {
			t.Errorf("expected %d backends (public instances + ddg), got %d", len(searxngPublicInstances)+1, len(c.backends))
		}
		if _, ok := c.backends[len(c.backends)-1].(*ddgSearcher); !ok {
			t.Errorf("expected ddg to be the last/final fallback")
		}
	})
}
