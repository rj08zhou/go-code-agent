package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"go-code-agent/internal/config"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// SearchTimeout caps a single backend HTTP attempt. Kept aligned with the
// per-backend context timeout so a hung backend cannot consume the whole
// web_search budget and starve the fallback chain.
const SearchTimeout = config.WebSearchPerBackendTimeout

type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

type SearchProvider interface {
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
	Name() string
}

// --- chainSearchProvider ---

type chainSearchProvider struct{ backends []SearchProvider }

func (c *chainSearchProvider) Name() string { return "chain" }

func (c *chainSearchProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	var errs []string
	for _, b := range c.backends {
		// Stop early if the overall budget is already spent; otherwise give
		// each backend its own bounded attempt so one slow/hung backend
		// cannot starve the remaining fallbacks.
		if ctx.Err() != nil {
			errs = append(errs, fmt.Sprintf("%s: skipped (%v)", b.Name(), ctx.Err()))
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, config.WebSearchPerBackendTimeout)
		results, err := b.Search(attemptCtx, query, limit)
		cancel()
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
	return nil, fmt.Errorf("all backends failed: %s", strings.Join(errs, "; "))
}

// --- DuckDuckGo ---

type ddgSearchProvider struct{ client *http.Client }

func newDDGSearchProvider() *ddgSearchProvider {
	return &ddgSearchProvider{client: NewSafeHTTPClient(SearchTimeout)}
}

func (d *ddgSearchProvider) Name() string { return "duckduckgo" }

func (d *ddgSearchProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	form := url.Values{"q": {query}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://lite.duckduckgo.com/lite/", strings.NewReader(form.Encode()))
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
	return parseDDGLiteHTML(string(body), limit)
}

func parseDDGLiteHTML(rawHTML string, limit int) ([]SearchResult, error) {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return nil, fmt.Errorf("parsing DDG: %w", err)
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

// --- SearXNG ---

type searxngSearchProvider struct {
	baseURL string
	client  *http.Client
}

func newSearxngSearchProvider(baseURL string) *searxngSearchProvider {
	return &searxngSearchProvider{baseURL: strings.TrimRight(baseURL, "/"), client: NewSafeHTTPClient(SearchTimeout)}
}

func (s *searxngSearchProvider) Name() string { return "searxng(" + s.baseURL + ")" }

func (s *searxngSearchProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	endpoint := fmt.Sprintf("%s/search?q=%s&format=json", s.baseURL, url.QueryEscape(query))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; go-code-agent-websearch/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
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
	type searxngResp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	var parsed searxngResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
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

var searxngPublicInstances = []string{
	"https://searx.be", "https://priv.au", "https://searxng.site",
}

// --- Tavily ---

type tavilySearchProvider struct {
	apiKey string
	client *http.Client
}

func newTavilySearchProvider(apiKey string) *tavilySearchProvider {
	return &tavilySearchProvider{apiKey: apiKey, client: NewSafeHTTPClient(SearchTimeout)}
}

func (t *tavilySearchProvider) Name() string { return "tavily" }

func (t *tavilySearchProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	payload, _ := json.Marshal(map[string]any{
		"api_key": t.apiKey, "query": query, "max_results": limit,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.tavily.com/search", strings.NewReader(string(payload)))
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
	type tavilyResp struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	var parsed tavilyResp
	if json.Unmarshal(body, &parsed) != nil {
		return nil, fmt.Errorf("invalid JSON response")
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

// --- Brave ---

type braveSearchProvider struct {
	apiKey string
	client *http.Client
}

func newBraveSearchProvider(apiKey string) *braveSearchProvider {
	return &braveSearchProvider{apiKey: apiKey, client: NewSafeHTTPClient(SearchTimeout)}
}

func (b *braveSearchProvider) Name() string { return "brave" }

func (b *braveSearchProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	endpoint := "https://api.search.brave.com/res/v1/web/search?q=" + url.QueryEscape(query)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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
	type braveResp struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	var parsed braveResp
	if json.Unmarshal(body, &parsed) != nil {
		return nil, fmt.Errorf("invalid JSON response")
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

// --- Factory ---

// SearchConfig contains all web-search backend selection settings. Keeping it
// explicit makes configuration testable and prevents the application layer
// from silently depending on process environment reads.
type SearchConfig struct {
	Provider         string
	APIKey           string
	SearxngURL       string
	SearxngInstances string
}

// SearchConfigFromEnv reads the README-compatible search environment variables.
func SearchConfigFromEnv() SearchConfig {
	return SearchConfig{
		Provider:         strings.TrimSpace(os.Getenv("WEB_SEARCH_PROVIDER")),
		APIKey:           strings.TrimSpace(os.Getenv("WEB_SEARCH_API_KEY")),
		SearxngURL:       strings.TrimSpace(os.Getenv("SEARXNG_URL")),
		SearxngInstances: strings.TrimSpace(os.Getenv("SEARXNG_INSTANCES")),
	}
}

// NewSearchProvider builds the configured provider or the SearXNG -> DDG fallback chain.
func NewSearchProvider(cfg SearchConfig) SearchProvider {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	apiKey := strings.TrimSpace(cfg.APIKey)

	switch provider {
	case "tavily":
		if apiKey != "" {
			return newTavilySearchProvider(apiKey)
		}
	case "brave":
		if apiKey != "" {
			return newBraveSearchProvider(apiKey)
		}
	}

	var chain []SearchProvider
	if u := strings.TrimSpace(cfg.SearxngURL); u != "" {
		chain = append(chain, newSearxngSearchProvider(u))
	} else {
		instances := searxngPublicInstances
		if override := strings.TrimSpace(cfg.SearxngInstances); override != "" {
			instances = splitTrim(override, ",")
		}
		for _, inst := range instances {
			chain = append(chain, newSearxngSearchProvider(inst))
		}
	}
	chain = append(chain, newDDGSearchProvider())
	return &chainSearchProvider{backends: chain}
}

func NewDefaultSearchProvider() SearchProvider { return NewSearchProvider(SearchConfigFromEnv()) }

func splitTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
