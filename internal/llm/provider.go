package llm

import (
	"errors"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/logging"
	"strings"
	"sync"
)

// Provider selection and registry.
//
// Current backends: openai (full), anthropic (full), gemini (stub).
// Selection: LLM_PROVIDER env > MODEL_ID prefix inference > fallback to openai.

// stdoutStreamSink prints streaming output to the terminal.
type stdoutStreamSink struct {
	started bool
}

// NewStdoutStreamSink creates a StreamSink that prints to the terminal.
func NewStdoutStreamSink() StreamSink { return &stdoutStreamSink{} }

func (s *stdoutStreamSink) OnTextDelta(text string) {
	if text == "" {
		return
	}
	if !s.started {
		logging.PrintAgentBegin()
		s.started = true
	}
	logging.PrintAgentDelta(text)
}

func (s *stdoutStreamSink) OnDone() {
	if s.started {
		logging.PrintAgentEnd()
		s.started = false
	}
}

// SilentStreamSink discards all stream events.
type SilentStreamSink struct{}

func (SilentStreamSink) OnTextDelta(string) {}
func (SilentStreamSink) OnDone()            {}

// PrefixedStreamSink prints streaming output with a colored prefix.
// Used by subagent and teammate loops to show real-time LLM output
// without conflicting with the main agent's output.
type PrefixedStreamSink struct {
	Prefix  string // e.g. "  [sub]" or "  [alice]"
	Color   string // ANSI color code (e.g. logging.ColorCyan)
	started bool
}

func (s *PrefixedStreamSink) OnTextDelta(text string) {
	if text == "" {
		return
	}
	if !s.started {
		fmt.Print(s.Color + s.Prefix + " ")
		s.started = true
	}
	fmt.Print(text)
}

func (s *PrefixedStreamSink) OnDone() {
	if s.started {
		fmt.Println(logging.ColorReset)
		s.started = false
	}
}

// providerRegistry is filled by each provider_*.go file's init(). We
// key by lowercase name to be forgiving of env var casing.
var providerRegistry = map[string]Provider{}

// providerBuilders maps a backend name to a constructor that builds a
// fresh provider with explicit (apiKey, baseURL) credentials. Empty
// args make the builder fall back to that backend's standard env vars.
// Registered by each provider_*.go init(). This lets secondary callers
// (the LLM-as-Judge) spin up an isolated instance pointing at a
// different key/endpoint than the main agent.
var providerBuilders = map[string]func(apiKey, baseURL string) Provider{}

// RegisterProvider adds a provider to the registry. Called by each
// provider_*.go's init() function.
func RegisterProvider(p Provider) {
	if p == nil {
		return
	}
	providerRegistry[strings.ToLower(p.Name())] = p
}

// RegisterProviderBuilder registers a credential-aware constructor for a
// backend. Called by each provider_*.go's init() function.
func RegisterProviderBuilder(name string, fn func(apiKey, baseURL string) Provider) {
	if fn == nil {
		return
	}
	providerBuilders[strings.ToLower(name)] = fn
}

// PickProvider chooses the active backend. Called once from main.
func PickProvider(modelID string) (Provider, error) {
	// 1. explicit override wins.
	if name := strings.ToLower(infra.Cfg.LLMProvider); name != "" {
		if p, ok := providerRegistry[name]; ok {
			return p, nil
		}
		return nil, fmt.Errorf("unknown LLM_PROVIDER=%q (registered: %s)",
			name, strings.Join(registeredNames(), ", "))
	}
	// 2. infer from model id.
	if name := inferProviderName(modelID); name != "" {
		if p, ok := providerRegistry[name]; ok {
			return p, nil
		}
	}
	// 3. fallback to openai.
	if p, ok := providerRegistry["openai"]; ok {
		return p, nil
	}
	return nil, errors.New("no LLM provider registered")
}

// inferProviderName returns the provider name implied by a model-id
// prefix, or "" when no rule matches. Centralizes the prefix->backend
// mapping so PickProvider, ProviderForModel and JudgeProvider stay in
// sync.
func inferProviderName(modelID string) string {
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

// ProviderForModel resolves which backend should serve modelID. Unlike
// PickProvider it never errors: it is meant for secondary calls (e.g.
// the LLM-as-Judge) that may run a different model than the main one,
// and it falls back to the active (main) provider when resolution is
// ambiguous.
//
// Resolution order:
//  1. LLM_PROVIDER env override (a single gateway serving every model).
//  2. inference from the model-id prefix.
//  3. the active (main) provider.
func ProviderForModel(modelID string) Provider {
	// 1. explicit global override wins - keeps parity with PickProvider
	//    so an OpenAI-compatible gateway hosting "claude-*" models is
	//    not mis-routed to the anthropic SDK.
	if name := strings.ToLower(infra.Cfg.LLMProvider); name != "" {
		if p, ok := providerRegistry[name]; ok {
			return p
		}
	}
	// 2. infer from the model-id prefix.
	if name := inferProviderName(modelID); name != "" {
		if p, ok := providerRegistry[name]; ok {
			return p
		}
	}
	// 3. fall back to the active (main) provider.
	return activeProvider
}

// judgeProviderCache memoizes provider instances built from JUDGE_* env
// vars, keyed by "name|apiKey|baseURL", so repeated Verify() calls reuse
// one http client / connection pool instead of rebuilding it each time.
var (
	judgeProviderCache = map[string]Provider{}
	judgeProviderMu    sync.Mutex
)

// JudgeProvider resolves the backend that should serve the LLM-as-Judge.
//
// It adds a dedicated-endpoint capability on top of ProviderForModel:
// the judge is designed to run a different (often cheaper) model, which
// may live behind its own gateway with its own key. The JUDGE_* env
// vars let users point it there without touching the main agent's
// provider:
//
//	JUDGE_PROVIDER  explicit backend SDK (openai | anthropic | gemini)
//	JUDGE_API_KEY   judge-only key  (else the backend's standard key)
//	JUDGE_BASE_URL  judge-only endpoint (else the backend's standard url)
//
// Resolution:
//   - None of the JUDGE_* vars set -> identical to ProviderForModel, so
//     existing setups are completely unchanged.
//   - JUDGE_API_KEY or JUDGE_BASE_URL set -> build (and cache) an
//     isolated instance via the backend's registered builder.
//   - Only JUDGE_PROVIDER set -> reuse the shared registered instance of
//     that backend (just routes the judge to a different vendor).
//
// Backend name is chosen as: JUDGE_PROVIDER > model-id prefix > the
// active (main) provider. It never errors; ambiguous cases fall back to
// the active provider so the judge degrades gracefully.
func JudgeProvider(modelID string) Provider {
	judgeName := strings.ToLower(infra.Cfg.JudgeProvider)
	apiKey := infra.Cfg.JudgeAPIKey
	baseURL := infra.Cfg.JudgeBaseURL

	// No judge-specific config at all -> preserve the prior behaviour
	// (honours LLM_PROVIDER, model-id prefix, active-provider fallback).
	if judgeName == "" && apiKey == "" && baseURL == "" {
		return ProviderForModel(modelID)
	}

	// Decide the backend SDK for the judge.
	name := judgeName
	if name == "" {
		name = inferProviderName(modelID)
	}
	if name == "" && activeProvider != nil {
		name = activeProvider.Name()
	}

	// Dedicated credentials -> build (cache) an isolated instance so the
	// judge talks to its own endpoint/key.
	if apiKey != "" || baseURL != "" {
		if build, ok := providerBuilders[name]; ok {
			cacheKey := name + "|" + apiKey + "|" + baseURL
			judgeProviderMu.Lock()
			defer judgeProviderMu.Unlock()
			if p, ok := judgeProviderCache[cacheKey]; ok {
				return p
			}
			p := build(apiKey, baseURL)
			judgeProviderCache[cacheKey] = p
			return p
		}
	}

	// JUDGE_PROVIDER set but no dedicated creds -> reuse the shared
	// registered instance of that backend.
	if p, ok := providerRegistry[name]; ok {
		return p
	}
	return activeProvider
}

func registeredNames() []string {
	out := make([]string, 0, len(providerRegistry))
	for n := range providerRegistry {
		out = append(out, n)
	}
	return out
}
