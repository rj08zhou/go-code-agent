package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"io"
	"net/http"
	"os"
	"strings"
)

type GeminiProvider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewGemini(apiKey, baseURL string) model.Provider {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	return &GeminiProvider{apiKey: apiKey, baseURL: baseURL, client: &http.Client{}}
}

func (p *GeminiProvider) Name() string { return "gemini" }

func (p *GeminiProvider) Call(ctx context.Context, params llm.CallParams) (*llm.Completion, error) {
	key := p.apiKey
	if key == "" {
		key = strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
	}
	if key == "" {
		return nil, fmt.Errorf("gemini: no API key (set GEMINI_API_KEY)")
	}

	contents := make([]map[string]any, 0, len(params.Messages))
	for _, m := range params.Messages {
		role := "user"
		if m.Role == llm.RoleAssistant {
			role = "model"
		}
		parts := []map[string]any{{"text": m.Content}}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}

	body := map[string]any{
		"contents": contents,
	}
	data, _ := json.Marshal(body)

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s", p.baseURL, params.Model, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, string(respData))
	}

	var result map[string]any
	if json.Unmarshal(respData, &result) != nil {
		return nil, fmt.Errorf("invalid response")
	}

	candidates, _ := result["candidates"].([]any)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}
	candidate := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)

	var textContent string
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			if t, ok := pm["text"].(string); ok {
				textContent += t
			}
		}
	}

	return &llm.Completion{
		Content:      textContent,
		FinishReason: "stop",
	}, nil
}

func (p *GeminiProvider) Stream(ctx context.Context, params llm.CallParams, sink model.StreamSink) (*llm.StreamResult, error) {
	comp, err := p.Call(ctx, params)
	if err != nil {
		return nil, err
	}
	sink.OnTextDelta(comp.Content)
	sink.OnDone()
	return &llm.StreamResult{
		Content:      comp.Content,
		ToolCalls:    comp.ToolCalls,
		FinishReason: comp.FinishReason,
		Usage:        comp.Usage,
	}, nil
}

var _ = json.Valid
