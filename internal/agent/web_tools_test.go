package agent

import (
	"go-code-agent/internal/web"
	"strings"
	"testing"
)

func TestFormatFetchResult_WrapsUntrustedContent(t *testing.T) {
	res := &web.FetchResult{URL: "https://example.com", StatusCode: 200, Text: "hello world"}
	out := formatFetchResult(res)
	if !strings.Contains(out, "BEGIN UNTRUSTED PAGE CONTENT") || !strings.Contains(out, "END UNTRUSTED PAGE CONTENT") {
		t.Errorf("expected untrusted-content markers, got: %q", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Errorf("expected page text present, got: %q", out)
	}
	if !strings.Contains(out, "https://example.com") || !strings.Contains(out, "200") {
		t.Errorf("expected URL/status present, got: %q", out)
	}
}

func TestFormatFetchResult_ReportsTruncation(t *testing.T) {
	res := &web.FetchResult{URL: "https://example.com", StatusCode: 200, Text: "x", Truncated: true}
	out := formatFetchResult(res)
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected a truncation notice, got: %q", out)
	}
}

func TestFormatFetchResult_RedactsSecrets(t *testing.T) {
	res := &web.FetchResult{
		URL: "https://example.com", StatusCode: 200,
		Text: "here is a key: sk-abcdefghijklmnopqrstuvwxyz1234567890 enjoy",
	}
	out := formatFetchResult(res)
	if strings.Contains(out, "abcdefghijklmnopqrstuvwxyz1234567890") {
		t.Errorf("expected secret-shaped text to be redacted, got: %q", out)
	}
}

func TestFormatSearchResults_ListsAllResults(t *testing.T) {
	results := []web.SearchResult{
		{Title: "First", URL: "https://a.example", Snippet: "snippet a"},
		{Title: "Second", URL: "https://b.example", Snippet: "snippet b"},
	}
	out := formatSearchResults("golang", results)
	for _, want := range []string{"First", "https://a.example", "snippet a", "Second", "https://b.example", "snippet b"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got: %q", want, out)
		}
	}
}

func TestFormatSearchResults_RedactsSecretsInTitleAndSnippet(t *testing.T) {
	results := []web.SearchResult{
		{Title: "leaked sk-abcdefghijklmnopqrstuvwxyz1234567890 key", URL: "https://a.example", Snippet: "clean"},
	}
	out := formatSearchResults("q", results)
	if strings.Contains(out, "abcdefghijklmnopqrstuvwxyz1234567890") {
		t.Errorf("expected secret in title to be redacted, got: %q", out)
	}
}

func TestClampSearchLimit(t *testing.T) {
	cases := map[int]int{
		0:   5,
		-1:  5,
		3:   3,
		20:  20,
		21:  20,
		999: 20,
	}
	for in, want := range cases {
		if got := clampSearchLimit(in); got != want {
			t.Errorf("clampSearchLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestSanitizeIfSecret(t *testing.T) {
	clean := "just a normal sentence"
	if sanitizeIfSecret(clean) != clean {
		t.Error("clean text should be returned unchanged")
	}
	dirty := "token: sk-abcdefghijklmnopqrstuvwxyz1234567890"
	if strings.Contains(sanitizeIfSecret(dirty), "abcdefghijklmnopqrstuvwxyz1234567890") {
		t.Error("secret-shaped text should be redacted")
	}
}

// TestDefaultSearcher_IsLazyAndCached verifies defaultSearcher() only
// builds the Searcher once (sync.Once) and returns the same instance
// on subsequent calls.
func TestDefaultSearcher_IsLazyAndCached(t *testing.T) {
	s1 := defaultSearcher()
	s2 := defaultSearcher()
	if s1 != s2 {
		t.Error("defaultSearcher() should return the same cached instance across calls")
	}
}
