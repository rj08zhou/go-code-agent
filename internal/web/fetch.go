package web

import (
	"context"
	"fmt"
	"go-code-agent/infra"
	"net/http"
	"strings"
)

// FetchTimeout / FetchMaxBytes are re-exported aliases of the
// infra-level constants (see infra/consts.go's "Outbound web access"
// block for the full rationale/env-var overview) - kept as package-
// level names here so callers/tests in this package don't need to
// import infra just to reference them.
const (
	FetchTimeout  = infra.WebFetchTimeout
	FetchMaxBytes = infra.WebFetchMaxBytes
)

// FetchResult is what web_fetch hands back to the caller (and,
// wrapped as tool output, to the LLM).
type FetchResult struct {
	URL         string
	StatusCode  int
	ContentType string
	Text        string // extracted readable text (HTML pages) or raw body (non-HTML text)
	Truncated   bool   // body exceeded FetchMaxBytes and was cut
}

// Fetch retrieves url through the SSRF-hardened client and returns its
// readable text content. HTML responses are run through HTMLToText;
// other text-ish content types (plain text, JSON, markdown...) are
// returned as-is (secrets sanitization and any size/content warnings
// are the caller's job - this function only does network + extraction).
func Fetch(ctx context.Context, rawURL string) (*FetchResult, error) {
	u, err := ValidateRequestURL(rawURL)
	if err != nil {
		return nil, err
	}

	client := NewSafeHTTPClient(FetchTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	// Identify the agent (courteous to server operators, and lets them
	// choose to block it if they don't want automated fetches) without
	// pretending to be a browser.
	req.Header.Set("User-Agent", "go-code-agent-webfetch/1.0 (+tool; respects robots via server-side blocking)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", u.String(), err)
	}
	defer resp.Body.Close()

	body, truncated, err := ReadLimited(resp.Body, FetchMaxBytes)
	if err != nil && len(body) == 0 {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	text := extractText(body, ct)

	return &FetchResult{
		URL:         u.String(),
		StatusCode:  resp.StatusCode,
		ContentType: ct,
		Text:        text,
		Truncated:   truncated,
	}, nil
}

// extractText decides, from the response's declared Content-Type,
// whether to run HTML extraction or just decode the bytes as text.
// Binary/unrecognized types get a short placeholder rather than dumping
// raw bytes (which would be useless to an LLM and could contain
// control characters that mangle the transcript).
func extractText(body []byte, contentType string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "text/html"), strings.Contains(ct, "application/xhtml"):
		return HTMLToText(string(body))
	case strings.Contains(ct, "text/"), strings.Contains(ct, "json"), strings.Contains(ct, "xml"),
		strings.Contains(ct, "javascript"), ct == "":
		// Empty Content-Type: assume text rather than guessing binary,
		// since a hostile/misconfigured server omitting it is common
		// and we'd rather show something than nothing.
		return string(body)
	default:
		return fmt.Sprintf("[non-text content, Content-Type: %s, %d bytes - not displayed]", contentType, len(body))
	}
}
