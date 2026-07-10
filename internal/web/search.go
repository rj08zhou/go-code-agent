package web

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// SearchTimeout is a re-exported alias of infra.WebSearchTimeout (see
// infra/consts.go) - bounds a single backend's request within the
// downgrade chain (see chainSearcher) so that trying a down/slow
// public instance doesn't stall the whole search for long before
// falling through to the next backend.
const SearchTimeout = infra.WebSearchTimeout

// SearchResult is one hit returned by any Searcher implementation.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// Searcher is the pluggable backend interface every search provider
// implements (DuckDuckGo, SearXNG, Tavily, Brave, ...). Kept minimal
// deliberately: query in, ranked results out, or an error - anything
// backend-specific (auth headers, endpoint shape, result parsing)
// stays inside that backend's own implementation.
type Searcher interface {
	// Search returns up to limit results for query, or an error if the
	// backend could not be reached / returned no usable data. An empty
	// (nil, nil) result is treated the same as an error by chainSearcher
	// (nothing useful came back), so implementations may return either.
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
	// Name identifies the backend for logging/diagnostics and for the
	// "all backends failed" aggregate error message.
	Name() string
}

// ---------------------------------------------------------------------
// chainSearcher: try backends in order, return the first that yields
// results. See NewDefaultSearcher for how the chain is assembled.
// ---------------------------------------------------------------------

type chainSearcher struct {
	backends []Searcher
}

func (c *chainSearcher) Name() string { return "chain" }

func (c *chainSearcher) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	var errs []string
	for _, b := range c.backends {
		results, err := b.Search(ctx, query, limit)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", b.Name(), err))
			continue
		}
		if len(results) == 0 {
			errs = append(errs, fmt.Sprintf("%s: no results", b.Name()))
			continue
		}
		return results, nil
	}
	return nil, fmt.Errorf("no search backend available - all failed: %s", strings.Join(errs, "; "))
}

// ---------------------------------------------------------------------
// DuckDuckGo Lite - zero-key fallback. Scrapes the non-JS HTML result
// page (there is no official DDG API); this is a widely-used fallback
// pattern precisely because it requires no signup/key at all, at the
// cost of being unofficial and occasionally broken by markup changes
// or bot-challenge pages - errors from it are just one more link in
// the chain, not fatal to web_search as a whole.
// ---------------------------------------------------------------------

type ddgSearcher struct{ client *http.Client }

func newDDGSearcher() *ddgSearcher {
	return &ddgSearcher{client: NewSafeHTTPClient(SearchTimeout)}
}

func (d *ddgSearcher) Name() string { return "duckduckgo" }

func (d *ddgSearcher) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	form := url.Values{"q": {query}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://lite.duckduckgo.com/lite/", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; go-code-agent-websearch/1.0)")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, _, err := ReadLimited(resp.Body, FetchMaxBytes)
	if err != nil {
		return nil, err
	}
	results, err := parseDDGLiteHTML(string(body), limit)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// parseDDGLiteHTML parses DuckDuckGo Lite's result table. Verified
// structure (2026, may drift - DDG Lite is explicitly documented as
// unofficial/scraped, hence this being wrapped in error handling and
// positioned as one link in a downgrade chain rather than the sole
// backend): each organic result is a run of adjacent <tr> rows:
//
//	<tr><td>N.</td><td><a class='result-link' href="URL">TITLE</a></td></tr>
//	<tr><td/><td class='result-snippet'>SNIPPET</td></tr>
//
// Sponsored rows carry class="result-sponsored" on the <tr> and are
// skipped entirely (ads are not useful search results for the agent,
// and their href is a DDG ad-redirect, not the actual target).
func parseDDGLiteHTML(rawHTML string, limit int) ([]SearchResult, error) {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil, fmt.Errorf("parsing DDG response: %w", err)
	}

	var results []SearchResult
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if len(results) >= limit {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Tr && !hasClass(n, "result-sponsored") {
			if a := findResultLinkAnchor(n); a != nil {
				href := attr(a, "href")
				title := textContent(a)
				if href != "" && title != "" {
					results = append(results, SearchResult{Title: strings.TrimSpace(title), URL: href})
				}
			}
		}
		// A snippet row is a separate sibling <tr>; attach it to the
		// most recently added result that doesn't have one yet.
		if n.Type == html.ElementNode && n.DataAtom == atom.Td && hasClass(n, "result-snippet") {
			if len(results) > 0 && results[len(results)-1].Snippet == "" {
				results[len(results)-1].Snippet = strings.TrimSpace(textContent(n))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func findResultLinkAnchor(tr *html.Node) *html.Node {
	var found *html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if found != nil {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.A && hasClass(n, "result-link") {
			found = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(tr)
	return found
}

func hasClass(n *html.Node, class string) bool {
	return slices.Contains(strings.Fields(attr(n, "class")), class)
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// ---------------------------------------------------------------------
// SearXNG - a metasearch proxy. Its JSON API (?format=json) is a
// stable, documented, self-describing format regardless of which
// instance answers, which is why one implementation serves both "a
// user-trusted self-hosted instance" (SearxngURL config) and "a list
// of known public instances tried in order" (searxngPublicInstances)
// - see NewDefaultSearcher for how those are wired into the chain.
// ---------------------------------------------------------------------

type searxngSearcher struct {
	baseURL string
	client  *http.Client
}

func newSearxngSearcher(baseURL string) *searxngSearcher {
	return &searxngSearcher{baseURL: strings.TrimRight(baseURL, "/"), client: NewSafeHTTPClient(SearchTimeout)}
}

func (s *searxngSearcher) Name() string { return "searxng(" + s.baseURL + ")" }

type searxngResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (s *searxngSearcher) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	endpoint := fmt.Sprintf("%s/search?q=%s&format=json", s.baseURL, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; go-code-agent-websearch/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d (instance may not expose the JSON API)", resp.StatusCode)
	}
	body, _, err := ReadLimited(resp.Body, FetchMaxBytes)
	if err != nil {
		return nil, err
	}
	var parsed searxngResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("invalid JSON response: %w", err)
	}
	results := make([]SearchResult, 0, min(limit, len(parsed.Results)))
	for _, r := range parsed.Results {
		if len(results) >= limit {
			break
		}
		results = append(results, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return results, nil
}

// searxngPublicInstances is a small, curated list of long-running
// public SearXNG instances used only when the operator hasn't
// configured SEARXNG_URL. It is intentionally short: each is tried
// with a short SearchTimeout and the chain moves on immediately on
// failure, so listing many wouldn't meaningfully improve reliability
// but would slow down the "all public instances are down today" case.
// Not a live-fetched list (querying searx.space itself would be one
// more network call that can fail) - just a periodically-refreshable
// constant. Overridable/extendable via SEARXNG_INSTANCES (comma-separated).
var searxngPublicInstances = []string{
	"https://searx.be",
	"https://priv.au",
	"https://searxng.site",
}

// ---------------------------------------------------------------------
// Tavily - key-based, purpose-built for LLM agents. Used only when
// WEB_SEARCH_API_KEY is set and WEB_SEARCH_PROVIDER=tavily (or it is
// the auto-detected provider from having a key of this shape - see
// NewDefaultSearcher).
// ---------------------------------------------------------------------

type tavilySearcher struct {
	apiKey string
	client *http.Client
}

func newTavilySearcher(apiKey string) *tavilySearcher {
	return &tavilySearcher{apiKey: apiKey, client: NewSafeHTTPClient(SearchTimeout)}
}

func (t *tavilySearcher) Name() string { return "tavily" }

type tavilyRequest struct {
	APIKey     string `json:"api_key"`
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type tavilyResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (t *tavilySearcher) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	payload, err := json.Marshal(tavilyRequest{APIKey: t.apiKey, Query: query, MaxResults: limit})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, _, err := ReadLimited(resp.Body, FetchMaxBytes)
	if err != nil {
		return nil, err
	}
	var parsed tavilyResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("invalid JSON response: %w", err)
	}
	results := make([]SearchResult, 0, min(limit, len(parsed.Results)))
	for _, r := range parsed.Results {
		if len(results) >= limit {
			break
		}
		results = append(results, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return results, nil
}

// ---------------------------------------------------------------------
// Brave Search - key-based, requires billing setup on Brave's side
// (their free tier still needs a card on file), included for parity
// since it's a common choice, but never auto-selected by default (see
// NewDefaultSearcher) - only used if the operator explicitly sets
// WEB_SEARCH_PROVIDER=brave.
// ---------------------------------------------------------------------

type braveSearcher struct {
	apiKey string
	client *http.Client
}

func newBraveSearcher(apiKey string) *braveSearcher {
	return &braveSearcher{apiKey: apiKey, client: NewSafeHTTPClient(SearchTimeout)}
}

func (b *braveSearcher) Name() string { return "brave" }

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func (b *braveSearcher) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	endpoint := "https://api.search.brave.com/res/v1/web/search?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.apiKey)

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, _, err := ReadLimited(resp.Body, FetchMaxBytes)
	if err != nil {
		return nil, err
	}
	var parsed braveResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("invalid JSON response: %w", err)
	}
	results := make([]SearchResult, 0, min(limit, len(parsed.Web.Results)))
	for _, r := range parsed.Web.Results {
		if len(results) >= limit {
			break
		}
		results = append(results, SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return results, nil
}

// NewDefaultSearcher assembles the Searcher web_search uses, honoring
// WEB_SEARCH_PROVIDER/WEB_SEARCH_API_KEY/SEARXNG_URL/SEARXNG_INSTANCES:
//
//   - WEB_SEARCH_PROVIDER=tavily|brave + WEB_SEARCH_API_KEY set: use
//     ONLY that backend. An operator who paid for and configured a
//     specific provider should not be silently downgraded to a public
//     instance or DDG if it hiccups - that would be a surprising and
//     hard-to-debug behavior change from what they asked for.
//   - Otherwise (the zero-config default): a downgrade chain -
//     1. SEARXNG_URL if set (a self-hosted/trusted instance the
//     operator explicitly named), tried alone (same reasoning as above
//     for explicit config).
//     2. Else the built-in public SearXNG instance list (or
//     SEARXNG_INSTANCES override), tried in order.
//     3. DuckDuckGo Lite, the zero-key backend that is always
//     available and always last (least structured/most likely to break
//     on markup drift, but requires nothing from the operator).
//
// web_search is therefore NEVER unavailable purely for lack of
// configuration - it just may be slower/less accurate without a paid
// key, which callers see reflected in which backend Name() ends up
// answering (surfaced in the tool's Reply so users can tell whether to
// consider adding a key).
func NewDefaultSearcher() Searcher {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("WEB_SEARCH_PROVIDER")))
	apiKey := strings.TrimSpace(os.Getenv("WEB_SEARCH_API_KEY"))

	switch provider {
	case "tavily":
		if apiKey != "" {
			return newTavilySearcher(apiKey)
		}
	case "brave":
		if apiKey != "" {
			return newBraveSearcher(apiKey)
		}
	}

	var chain []Searcher
	if u := strings.TrimSpace(os.Getenv("SEARXNG_URL")); u != "" {
		chain = append(chain, newSearxngSearcher(u))
	} else {
		instances := searxngPublicInstances
		if override := strings.TrimSpace(os.Getenv("SEARXNG_INSTANCES")); override != "" {
			instances = splitAndTrim(override, ",")
		}
		for _, inst := range instances {
			chain = append(chain, newSearxngSearcher(inst))
		}
	}
	chain = append(chain, newDDGSearcher())
	return &chainSearcher{backends: chain}
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
