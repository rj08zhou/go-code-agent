// Package config holds process-wide configuration parsed from environment.
// It is immutable after construction; no package-level mutable state.
package config

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const defaultModelID = "claude-opus-4.7"

// Config is the immutable configuration snapshot.
type Config struct {
	ModelID         string
	SnapshotEnabled bool

	LLMProvider      string
	OpenAIAPIKey     string
	OpenAIBaseURL    string
	AnthropicAPIKey  string
	AnthropicBaseURL string

	LLMMaxQPS         float64
	LLMMaxBurst       int
	LLMMaxConcurrency int

	JudgeEnabled  bool
	JudgeModel    string
	JudgeMinScore int
	JudgeProvider string
	JudgeAPIKey   string
	JudgeBaseURL  string

	ContextWindowOverride int

	// Web search and outbound web policy configuration.
	WebSearchProvider  string
	WebSearchAPIKey    string
	SearxngURL         string
	SearxngInstances   string
	WebAllowPrivateIPs bool
}

// Load reads env vars into an immutable Config.
func Load() *Config {
	return &Config{
		ModelID:               firstNonEmptyEnv("MODEL_ID", defaultModelID),
		SnapshotEnabled:       os.Getenv("SNAPSHOT_ENABLED") == "1",
		LLMProvider:           strings.TrimSpace(os.Getenv("LLM_PROVIDER")),
		OpenAIAPIKey:          strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
		OpenAIBaseURL:         strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
		AnthropicAPIKey:       strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
		AnthropicBaseURL:      strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
		LLMMaxQPS:             envFloat("LLM_MAX_QPS", 4.0),
		LLMMaxBurst:           envInt("LLM_MAX_BURST", 8),
		LLMMaxConcurrency:     envInt("LLM_MAX_CONCURRENCY", 4),
		JudgeEnabled:          envBool("JUDGE_ENABLED"),
		JudgeModel:            strings.TrimSpace(os.Getenv("JUDGE_MODEL")),
		JudgeMinScore:         envInt("JUDGE_MIN_SCORE", 7),
		JudgeProvider:         strings.TrimSpace(os.Getenv("JUDGE_PROVIDER")),
		JudgeAPIKey:           strings.TrimSpace(os.Getenv("JUDGE_API_KEY")),
		JudgeBaseURL:          strings.TrimSpace(os.Getenv("JUDGE_BASE_URL")),
		ContextWindowOverride: envInt("CONTEXT_WINDOW_TOKENS", 0),
		WebSearchProvider:     strings.TrimSpace(os.Getenv("WEB_SEARCH_PROVIDER")),
		WebSearchAPIKey:       strings.TrimSpace(os.Getenv("WEB_SEARCH_API_KEY")),
		SearxngURL:            strings.TrimSpace(os.Getenv("SEARXNG_URL")),
		SearxngInstances:      strings.TrimSpace(os.Getenv("SEARXNG_INSTANCES")),
		WebAllowPrivateIPs:    envBool("WEB_ALLOW_PRIVATE_IPS"),
	}
}

func (c *Config) ContextWindowTokens(model string) int {
	if c.ContextWindowOverride > 0 {
		return c.ContextWindowOverride
	}
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "claude"):
		return 200000
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"):
		return 128000
	case strings.HasPrefix(m, "gemini"):
		return 1000000
	default:
		return 128000
	}
}

func (c *Config) CompactionThreshold(model string) int {
	windowBudget := int(float64(c.ContextWindowTokens(model)) * 0.75)
	if windowBudget < 300000 {
		return windowBudget
	}
	return 300000
}

func (c *Config) Validate() []string {
	var warns []string
	if c.OpenAIAPIKey == "" && c.AnthropicAPIKey == "" {
		warns = append(warns, "no LLM API key found (set OPENAI_API_KEY or ANTHROPIC_API_KEY)")
	}
	return warns
}

func firstNonEmptyEnv(key, dflt string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return dflt
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

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

// --- Compile-time constants (not env-configurable) ---

const (
	StuckThreshold              = 12
	ExploreStuckThreshold       = 8
	InvestigateStuckThreshold   = 10
	ReflectInterval             = 12
	MaxConsecutiveFailures      = 3
	MaxRounds                   = 100
	DefaultContextTokens        = 128000
	LessonThreshold             = 3
	SubagentMaxRounds           = 30
	TeammateWorkMaxRounds       = 50
	DefaultMaxOutputTokens      = 16384
	SubagentPromptTokenBudget   = 150000
	SubagentCompactionThreshold = 12000
	MaxExploreDelegations       = 3
	MaxRepeatedToolCalls        = 3
)

const (
	TokenThreshold          = 300000
	KeepRecent              = 15
	MaxOutputLen            = 64 * 1024
	KeepRecentMessages      = 20
	CompactionThresholdFrac = 0.75
)

const (
	PollInterval = 5 * time.Second
	IdleTimeout  = 60 * time.Second
)

const (
	LlmMaxRetries     = 5
	LlmBaseDelay      = 1 * time.Second
	LlmRateLimitDelay = 10 * time.Second
	LlmMaxDelay       = 60 * time.Second
	LlmCallTimeout    = 5 * time.Minute
	LlmHTTPTimeout    = 6 * time.Minute
)

const MaxActiveWorktrees = 10

const (
	MemoryTTLDays        = 90
	DeduplicateThreshold = 0.7
	MaxMemoryContentLen  = 2000
	MaxEvergreenChars    = 8000
)

const (
	ReadFileDefaultLimit       = 500
	BashTimeout                = 120 * time.Second
	PerToolTimeout             = 5 * time.Minute
	SubagentTimeout            = 10 * time.Minute
	SubagentSoftDeadlineBuffer = 30 * time.Second
)

const (
	MaxTeamMessageSize = 64 * 1024
	MaxBgResultChars   = 200
	MaxBgNotifications = 10
	MaxInboxBytes      = 4096
)

const (
	JudgeMinScore        = 7
	JudgeMaxRetryInjects = 2
)

const (
	PlanRequestTTL  = 30 * time.Minute
	ApprovedPlanTTL = 24 * time.Hour
)

const (
	WebFetchTimeout  = 20 * time.Second
	WebFetchMaxBytes = 128 * 1024
	WebSearchTimeout = 8 * time.Second
)

// SpawnMinInterval staggers teammate starts.
const SpawnMinInterval = 750 * time.Millisecond

// LessonRoundsLimit caps extra rounds after lesson injection.
const LessonRoundsLimit = 3

// PlanningGateMinTaskChars is the minimum task chars for planning gate.
const PlanningGateMinTaskChars = 80

// TokenCheckInterval controls re-check frequency.
const TokenCheckInterval = 3

var globalCfg atomic.Pointer[Config]

func CurrentConfig() *Config { return globalCfg.Load() }

func SetConfig(cfg *Config) {
	// Compare-and-swap ensures exactly-once initialization.
	if !globalCfg.CompareAndSwap(nil, cfg) {
		// Already initialized: allow forced update for hot-reload scenarios.
		globalCfg.Store(cfg)
	}
}
