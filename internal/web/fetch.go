package web

import (
	"context"
	"fmt"
	"go-code-agent-refactor/internal/config"
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

	body, truncated, err := ReadLimited(resp.Body, FetchMaxBytes)
	if err != nil && len(body) == 0 {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	text := extractText(body, ct)

	return &FetchResult{
		URL:         cleanURL,
		StatusCode:  resp.StatusCode,
		ContentType: ct,
		Text:        text,
		Truncated:   truncated,
	}, nil
}

func extractText(body []byte, contentType string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "text/html"), strings.Contains(ct, "application/xhtml"):
		return HTMLToText(string(body))
	case strings.Contains(ct, "text/"), strings.Contains(ct, "json"), strings.Contains(ct, "xml"),
		strings.Contains(ct, "javascript"), ct == "":
		return string(body)
	default:
		return fmt.Sprintf("[non-text content, Content-Type: %s, %d bytes]", contentType, len(body))
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
