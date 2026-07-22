package web

import (
	"context"
	"fmt"
	"go-code-agent/internal/config"
	"net/http"
	"strings"
)

const (
	FetchMaxBytes = config.WebFetchMaxBytes
	FetchTimeout  = config.WebFetchTimeout
)

type FetchResult struct {
	URL         string
	StatusCode  int
	ContentType string
	Text        string
	Truncated   bool
	// NoStaticText is set when the page returned HTML but yielded no
	// extractable static text (e.g. a JS-rendered SPA). In that case Text
	// holds a diagnostic note / metadata rather than article content, and
	// the truncation marker is meaningless.
	NoStaticText bool
}

func Fetch(ctx context.Context, rawURL string) (*FetchResult, error) {
	cleanURL, err := ValidateRequestURL(rawURL)
	if err != nil {
		return nil, err
	}
	client := NewSafeHTTPClient(FetchTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cleanURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("User-Agent", "go-code-agent-webfetch/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.5")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", cleanURL, err)
	}
	defer resp.Body.Close()

	// Reject error responses instead of returning the error page body as if
	// it were article content (observed: 404/502 gateway pages polluting
	// web_fetch results and wasting a subagent round).
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d %s for %s",
			resp.StatusCode, http.StatusText(resp.StatusCode), cleanURL)
	}

	body, truncated, err := ReadLimited(resp.Body, FetchMaxBytes)
	if err != nil && len(body) == 0 {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	text, noStaticText := extractText(body, ct)

	return &FetchResult{
		URL:          cleanURL,
		StatusCode:   resp.StatusCode,
		ContentType:  ct,
		Text:         text,
		Truncated:    truncated,
		NoStaticText: noStaticText,
	}, nil
}

// extractText converts a response body to text. The second return value is
// true when the page was HTML but had no extractable static text (SPA), in
// which case the returned string is a diagnostic note / metadata.
func extractText(body []byte, contentType string) (string, bool) {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "text/html"), strings.Contains(ct, "application/xhtml"):
		text := HTMLToText(string(body))
		// Empty OR chrome-only extractions (nav/promo CTAs with no article)
		// both count as "no useful static text" — fall back to metadata so
		// the subagent doesn't thrash on junk.
		if strings.TrimSpace(text) != "" && !isBoilerplateText(text) {
			return text, false
		}
		if meta := HTMLMetaFallback(string(body)); meta != "" {
			note := "[This page is rendered client-side (JavaScript); no static article text was found. "
			if strings.TrimSpace(text) != "" {
				note = "[Extracted text looked like site chrome/promos with no article body. "
			}
			return note + "Metadata extracted below.]\n\n" + meta, true
		}
		return "[This page returned HTML with no extractable article text " +
			"(JS-rendered SPA or chrome-only page). Try an alternative source or a direct API/raw URL.]", true
	case strings.Contains(ct, "text/"), strings.Contains(ct, "json"), strings.Contains(ct, "xml"),
		strings.Contains(ct, "javascript"), ct == "":
		return string(body), false
	default:
		return fmt.Sprintf("[non-text content, Content-Type: %s, %d bytes]", contentType, len(body)), false
	}
}

func ValidateRequestURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url is empty")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("unsupported scheme (only http/https allowed)")
	}
	return raw, nil
}
