package web

import (
	"context"
	"fmt"
	"strings"
)

const defaultSearchLimit = 5

// Service exposes the Web capabilities used by Agent tools.
//
// It owns the conversion from structured Web results to the text returned to
// the model, while preserving errors for the tool executor to classify.
type Service struct {
	provider SearchProvider
}

func NewService(provider SearchProvider) *Service {
	return &Service{provider: provider}
}

func (s *Service) Fetch(ctx context.Context, rawURL string) (string, error) {
	result, err := Fetch(ctx, rawURL)
	if err != nil {
		return "", err
	}
	out := result.Text
	if result.Truncated {
		out += "\n[content truncated]"
	}
	return out, nil
}

func (s *Service) Search(ctx context.Context, query string) (string, error) {
	if s == nil || s.provider == nil {
		return "", fmt.Errorf("web search unavailable")
	}
	results, err := s.provider.Search(ctx, query, defaultSearchLimit)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, result := range results {
		fmt.Fprintf(&sb, "- [%s](%s)\n  %s\n", result.Title, result.URL, result.Snippet)
	}
	return sb.String(), nil
}
