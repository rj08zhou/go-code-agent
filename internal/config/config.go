// Package config holds process-wide configuration parsed from environment
// and compile-time tuning constants.
//
// Config is immutable after Load(). Callers receive *Config via explicit
// injection from the composition root (Application); there is no process
// global accessor.
package config

import (
	"os"
	"strconv"
	"strings"
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

	// ReasoningEnabled opts agent execution calls into provider-native
	// reasoning. ReasoningEffort is a provider-interpreted hint; the OpenAI
	// adapter validates and maps it to reasoning_effort.
	ReasoningEnabled bool
	ReasoningEffort  string

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
		ReasoningEnabled:      envBool("REASONING_ENABLED"),
		ReasoningEffort:       firstNonEmptyEnv("REASONING_EFFORT", "medium"),
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
	StuckThreshold            = 12
	ExploreStuckThreshold     = 8
	InvestigateStuckThreshold = 10
	ReflectInterval           = 12
	MaxConsecutiveFailures    = 3
	MaxRounds                 = 100
	DefaultContextTokens      = 128000
	LessonThreshold           = 3
	// SubagentMaxRounds is a soft ceiling for explore tool rounds. The
	// sharper cost brake is SubagentPromptTokenBudget; keep rounds a bit
	// looser so a healthy investigation is not cut solely by round count.
	SubagentMaxRounds = 16
	// WebFetchSubagentMaxRounds bounds a web_fetch subagent's fetch/search
	// rounds. It only has WebFetchSubagentBudget (60s) of wall time and
	// should converge in a handful of attempts; the generous explore cap
	// just lets it thrash fetch->search->fetch until the clock runs out.
	WebFetchSubagentMaxRounds = 6
	TeammateWorkMaxRounds     = 50
	DefaultMaxOutputTokens    = 16384
	// SubagentPromptTokenBudget is the SUM of estimated prompt sizes across
	// explore rounds (not a single-request cap). 150k let one thrashing
	// explore bill ~130k; 80k still cuts that roughly in half while leaving
	// room for a healthy multi-file investigation.
	SubagentPromptTokenBudget   = 80000
	SubagentCompactionThreshold = 12000
	MaxExploreDelegations       = 3
	MaxRepeatedToolCalls        = 3
	// MaxReadsAfterExplore caps lead read_file/list_dir calls after a
	// successful explore in the same Run(). Stops the common failure mode
	// where lead re-walks the tree "to be more concrete" after explore
	// already returned a summary. search_content/search_file stay unlimited.
	MaxReadsAfterExplore = 2
	// SubagentToolOutputMaxChars caps each tool result inside an explore
	// subagent. Sized for a mid-size file via read_file (default 500 lines)
	// without letting bash/list dumps balloon every subsequent prompt.
	// 16KB keeps a healthy multi-file investigation readable while still
	// bounding the damage a bash dump does to the prompt prefix cache.
	SubagentToolOutputMaxChars = 16000
	// ExploreBudgetWarnFrac injects a "start synthesizing" nudge once the
	// cumulative explore prompt spend crosses this fraction of
	// SubagentPromptTokenBudget, so the model can wrap up before the hard
	// budget forces a no-tools turn (master's soft-deadline spirit).
	ExploreBudgetWarnFrac = 0.70
	// SearchFileMaxMatches caps search_file results so a broad glob like
	// "*.go" cannot dump the whole tree into the explore context.
	SearchFileMaxMatches = 40
)

const (
	TokenThreshold          = 300000
	KeepRecent              = 15
	MaxOutputLen            = 64 * 1024
	KeepRecentMessages      = 20
	// BashOutputMaxChars caps the lead agent's bash output. Unlike read_file,
	// bash has no offset paging, so oversized output is kept as a head/tail
	// window (diagnostics like build errors usually land at the end) plus a
	// hint to narrow with head/tail/grep instead of flooding context.
	BashOutputMaxChars = 32 * 1024
	// CompressToolOutputChars is how much of each old tool result is kept
	// when building the compression summary handed to the LLM. 200 chars
	// forced the model to reconstruct dropped details; 1KB preserves enough
	// of a truncated read/bash window to stay useful after compaction.
	CompressToolOutputChars = 1024
	CompactionThresholdFrac = 0.75
	// MicroCompactThresholdFrac gates the light-weight tool-result clearing on
	// actual context pressure instead of a fixed round cadence. MicroCompact
	// only runs once estimated usage exceeds this fraction of the context
	// window, so short/medium sessions keep their prompt-cache prefix intact.
	// Kept below CompactionThresholdFrac so this cheaper tier fires first and
	// delays full (LLM) compaction.
	MicroCompactThresholdFrac = 0.5
	// MicroCompactMinClearBytes is the clear_at_least guard: MicroCompact skips
	// clearing unless it can reclaim at least this many bytes, so we never
	// invalidate the cache prefix for a negligible saving (mirrors Anthropic
	// context-editing's clear_at_least). ~4KB ≈ ~1K tokens.
	MicroCompactMinClearBytes = 4096
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
	// WebFetchTimeout bounds a single raw HTTP fetch (used by the inner
	// web_fetch subagent that actually pulls the page).
	WebFetchTimeout = 20 * time.Second
	// WebFetchSubagentBudget is the total time the lead agent's web_fetch
	// tool gives its delegated read-only subagent (which may issue several
	// fetch rounds). The web_fetch tool definition's Timeout must be at
	// least this large, otherwise the executor kills the subagent before
	// its own budget expires.
	WebFetchSubagentBudget = 60 * time.Second
	WebFetchMaxBytes       = 128 * 1024
	// WebSearchTimeout is the overall budget for a web_search call across
	// the whole provider fallback chain.
	WebSearchTimeout = 25 * time.Second
	// WebSearchPerBackendTimeout caps a single backend attempt so one hung
	// backend cannot consume the entire budget and starve the fallbacks.
	WebSearchPerBackendTimeout = 7 * time.Second
)

// SpawnMinInterval staggers teammate starts.
const SpawnMinInterval = 750 * time.Millisecond

// LessonRoundsLimit caps extra rounds after lesson injection.
const LessonRoundsLimit = 3

// PlanningGateMinTaskChars is the minimum task chars for planning gate.
const PlanningGateMinTaskChars = 80

// TokenCheckInterval controls re-check frequency.
const TokenCheckInterval = 3
