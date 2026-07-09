package infra

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Centralized runtime configuration.
//
// Config is the single, typed snapshot of every environment variable
// the process honours. Previously each subsystem read os.Getenv on its
// own (main.go, judge.go, limiter.go, provider*.go), so "what knobs
// exist" was scattered across the tree and impossible to see at a
// glance. Load() is now the ONE place that parses env; Cfg is the
// process-wide snapshot taken once, at package-init time.
//
// Init-order note: this file lives in infra, the lowest-level package
// that everything else imports. Go initializes imported packages
// before their importers, so `var Cfg = Load()` runs before the init()
// of internal/llm (where the provider constructors read credentials)
// and before main(). That is why credentials read at provider-init
// time can safely read Cfg. Environment is fixed for the process's
// lifetime, so reading it once here loses nothing.
//
// Dependency note: Load()/Validate() deliberately import only the
// standard library. infra must stay free of any internal/* import to
// remain the dependency-free base layer; callers (main.go) do the
// logging of Validate()'s findings.

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

// Cfg is the process-wide configuration snapshot, populated once at
// package-init time. Tests that need to exercise parsing against a
// mutated environment should call Load() directly rather than mutating
// this value.
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

// ContextWindowTokens returns the assumed context-window size (in
// tokens) for a model id. An explicit CONTEXT_WINDOW_TOKENS env
// override wins; otherwise it is inferred from the model-id prefix,
// falling back to ContextWindowDefault for unrecognized ids. The
// figure only needs to be in the right ballpark - CompactionThresholdFrac
// leaves headroom on top of it.
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

// CompactionThreshold returns the estimated-token count at which
// AutoCompact should fire for a given model: the smaller of the
// model-window-derived budget (window * CompactionThresholdFrac) and
// the absolute TokenThreshold cap. Making it model-aware prevents a
// small-window model from silently blowing past its real limit, while
// the TokenThreshold cap preserves existing behavior for large-window
// models.
func CompactionThreshold(model string) int {
	windowBudget := int(float64(ContextWindowTokens(model)) * CompactionThresholdFrac)
	if windowBudget < TokenThreshold {
		return windowBudget
	}
	return TokenThreshold
}

// Validate returns human-readable warnings about a suspicious (but not
// necessarily fatal) configuration. main() prints these at startup.
// They are intentionally non-fatal: e.g. a dedicated OpenAI-compatible
// gateway may authenticate purely via LLM_PROVIDER + OPENAI_BASE_URL,
// so a missing standard key is a warning, not an error.
func (c *Config) Validate() []string {
	var warns []string

	// No credentials at all is the single most common misconfig - the
	// first LLM call would otherwise fail with an opaque 401.
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

// envFloat parses a float env var, falling back to dflt on missing or
// malformed input. A parsed value <= 0 is preserved (the limiter reads
// LLM_MAX_QPS=0 as "disable throttling").
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
