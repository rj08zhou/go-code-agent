// Package provider implements the LLM backend adapters.
package provider

import (
	"fmt"
	"go-code-agent-refactor/internal/config"
	"go-code-agent-refactor/internal/llm"
	"go-code-agent-refactor/internal/model"
	"strings"
	"sync"
)

// Registry holds all registered providers.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]model.Provider
	builders  map[string]func(apiKey, baseURL string) model.Provider
}

func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]model.Provider),
		builders:  make(map[string]func(apiKey, baseURL string) model.Provider),
	}
}

func (r *Registry) Register(p model.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[strings.ToLower(p.Name())] = p
}

func (r *Registry) RegisterBuilder(name string, fn func(apiKey, baseURL string) model.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.builders[strings.ToLower(name)] = fn
}

func (r *Registry) Pick(cfg *config.Config) (model.Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Explicit override wins
	if name := strings.ToLower(cfg.LLMProvider); name != "" {
		if p, ok := r.providers[name]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("unknown LLM_PROVIDER=%q", name)
	}
	// Infer from model ID
	if name := inferName(cfg.ModelID); name != "" {
		if p, ok := r.providers[name]; ok {
			return p, nil
		}
	}
	// Fallback to openai
	if p, ok := r.providers["openai"]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no LLM provider available")
}

func (r *Registry) JudgeProvider(cfg *config.Config) model.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	name := strings.ToLower(cfg.JudgeProvider)
	if name == "" {
		name = inferName(cfg.JudgeModel)
	}
	if name == "" {
		return nil
	}

	// Dedicated credentials -> build isolated instance
	if cfg.JudgeAPIKey != "" || cfg.JudgeBaseURL != "" {
		if build, ok := r.builders[name]; ok {
			return build(cfg.JudgeAPIKey, cfg.JudgeBaseURL)
		}
	}
	if p, ok := r.providers[name]; ok {
		return p
	}
	return nil
}

func inferName(modelID string) string {
	m := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.HasPrefix(m, "claude-") || strings.HasPrefix(m, "claude."):
		return "anthropic"
	case strings.HasPrefix(m, "gemini-") || strings.HasPrefix(m, "gemini."):
		return "gemini"
	case strings.HasPrefix(m, "gpt-") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3"):
		return "openai"
	}
	return ""
}

func (r *Registry) ForModel(cfg *config.Config, modelID string) model.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name := strings.ToLower(cfg.LLMProvider); name != "" {
		if p, ok := r.providers[name]; ok {
			return p
		}
	}
	if name := inferName(modelID); name != "" {
		if p, ok := r.providers[name]; ok {
			return p
		}
	}
	return nil
}

// --- Helper: centralize provider selection ---

func BuildGateway(cfg *config.Config, registry *Registry) (*model.Gateway, *model.RoleThrottle, error) {
	p, err := registry.Pick(cfg)
	if err != nil {
		return nil, nil, err
	}
	throttle := model.NewRoleThrottle(cfg.LLMMaxConcurrency)
	gw := model.NewGateway(p, throttle)

	if cfg.JudgeEnabled && cfg.JudgeModel != "" {
		if jp := registry.JudgeProvider(cfg); jp != nil {
			gw.SetJudgeProvider(jp)
		}
	}
	return gw, throttle, nil
}

// MkOk / MkErr helpers
func MkOk(output string) llm.Completion {
	return llm.Completion{Content: output, FinishReason: "stop"}
}

func MkErr(msg string) llm.Completion {
	return llm.Completion{Content: "[ERROR] " + msg, FinishReason: "stop"}
}
