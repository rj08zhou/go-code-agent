package infra

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Centralized runtime configuration. Load() parses env once at init; Cfg
// is the process-wide snapshot.

const defaultModelID = "claude-opus-4.7"

// Config holds every env-derived setting, parsed once by Load.
type Config struct {
	// Core
	ModelID         string // MODEL_ID (defaults to defaultModelID)
	SnapshotEnabled bool   // SNAPSHOT_ENABLED == "1"

	// LLM provider selection + credentials
	LLMProvider      string // LLM_PROVIDER (explicit backend override)
	OpenAIAPIKey     string // OPENAI_API_KEY
	OpenAIBaseURL    string // OPENAI_BASE_URL
	AnthropicAPIKey  string // ANTHROPIC_API_KEY
	AnthropicBaseURL string // ANTHROPIC_BASE_URL

	// LLM throttle (0 for QPS => disabled; see limiter.go)
	LLMMaxQPS         float64 // LLM_MAX_QPS
	LLMMaxBurst       int     // LLM_MAX_BURST
	LLMMaxConcurrency int     // LLM_MAX_CONCURRENCY

	// LLM-as-Judge
	JudgeEnabled  bool   // JUDGE_ENABLED (1|true|yes|on)
	JudgeModel    string // JUDGE_MODEL (empty => reuse main model)
	JudgeMinScore int    // JUDGE_MIN_SCORE (defaults to JudgeMinScore const)
	JudgeProvider string // JUDGE_PROVIDER
	JudgeAPIKey   string // JUDGE_API_KEY
	JudgeBaseURL  string // JUDGE_BASE_URL

	// Context window
	ContextWindowOverride int // CONTEXT_WINDOW_TOKENS (0 => infer from model id)
}

// Cfg is the process-wide config snapshot, populated once at init.
var Cfg = Load()

// Load reads and parses the full environment into a Config. It never
// fails; malformed numeric values fall back to their defaults (use
// Validate to surface suspicious settings at startup).
func Load() *Config {
	c := &Config{
		ModelID:          firstNonEmptyEnv("MODEL_ID", defaultModelID),
		SnapshotEnabled:  os.Getenv("SNAPSHOT_ENABLED") == "1",
		LLMProvider:      strings.TrimSpace(os.Getenv("LLM_PROVIDER")),
		OpenAIAPIKey:     strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIBaseURL:    strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
		AnthropicAPIKey:  strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		AnthropicBaseURL: strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),

		LLMMaxQPS:         envFloat("LLM_MAX_QPS", LlmDefaultMaxQPS),
		LLMMaxBurst:       envInt("LLM_MAX_BURST", LlmDefaultMaxBurst),
		LLMMaxConcurrency: envInt("LLM_MAX_CONCURRENCY", LlmDefaultMaxConcurrency),

		JudgeEnabled:  envBool("JUDGE_ENABLED"),
		JudgeModel:    strings.TrimSpace(os.Getenv("JUDGE_MODEL")),
		JudgeMinScore: envInt("JUDGE_MIN_SCORE", JudgeMinScore),
		JudgeProvider: strings.TrimSpace(os.Getenv("JUDGE_PROVIDER")),
		JudgeAPIKey:   strings.TrimSpace(os.Getenv("JUDGE_API_KEY")),
		JudgeBaseURL:  strings.TrimSpace(os.Getenv("JUDGE_BASE_URL")),

		ContextWindowOverride: envInt("CONTEXT_WINDOW_TOKENS", 0),
	}
	return c
}

// ContextWindowTokens returns the assumed context-window size for a model id.
// Explicit override wins; otherwise inferred from model-id prefix.
func ContextWindowTokens(model string) int {
	if Cfg != nil && Cfg.ContextWindowOverride > 0 {
		return Cfg.ContextWindowOverride
	}
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "claude"):
		return ContextWindowClaude
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"):
		return ContextWindowGPT
	case strings.HasPrefix(m, "gemini"):
		return ContextWindowGemini
	default:
		return ContextWindowDefault
	}
}

// CompactionThreshold returns the token count at which AutoCompact fires:
// the smaller of window*CompactionThresholdFrac and the absolute TokenThreshold.
func CompactionThreshold(model string) int {
	windowBudget := int(float64(ContextWindowTokens(model)) * CompactionThresholdFrac)
	if windowBudget < TokenThreshold {
		return windowBudget
	}
	return TokenThreshold
}

// Validate returns non-fatal warnings about suspicious config.
func (c *Config) Validate() []string {
	var warns []string

	// No credentials is the most common misconfig.
	if c.OpenAIAPIKey == "" && c.AnthropicAPIKey == "" {
		warns = append(warns, "no LLM API key found (set OPENAI_API_KEY or ANTHROPIC_API_KEY; ignore if your gateway needs none)")
	}
	if c.JudgeEnabled && (c.JudgeMinScore < 1 || c.JudgeMinScore > 10) {
		warns = append(warns, fmt.Sprintf("JUDGE_MIN_SCORE=%d is out of the expected 1-10 range", c.JudgeMinScore))
	}
	if c.LLMMaxConcurrency < 0 {
		warns = append(warns, fmt.Sprintf("LLM_MAX_CONCURRENCY=%d is negative; treated as unbounded", c.LLMMaxConcurrency))
	}
	return warns
}

func firstNonEmptyEnv(key, dflt string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return dflt
}

// envBool reports whether an env var is set to a truthy token.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envFloat parses a float env var, falling back to dflt on missing/malformed.
func envFloat(key string, dflt float64) float64 {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return dflt
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return dflt
	}
	return v
}

// envInt parses an int env var, falling back to dflt on missing or
// malformed input.
func envInt(key string, dflt int) int {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return dflt
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return dflt
	}
	return v
}
