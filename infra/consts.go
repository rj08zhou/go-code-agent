package infra

import "time"

// Project-wide configuration constants. All tunable thresholds and timeouts.

const (
	StuckThreshold         = 20    // rounds without completing a task = "stuck"
	ReflectInterval        = 20    // periodic reflection every N tool rounds
	MaxConsecutiveFailures = 3     // same tool failing → force strategy change
	MaxRounds              = 100   // hard safety cap for agent loop
	LessonThreshold        = 3     // min tool rounds before auto-lesson prompt
	SubagentMaxRounds      = 30    // subagent inner loop cap
	TeammateWorkMaxRounds  = 50    // teammate workPhase inner loop cap
	DefaultMaxOutputTokens = 16384 // default max output tokens for LLM calls
)

const (
	TokenThreshold = 300000 // autoCompact trigger (estimated total tokens)
	KeepRecent     = 15     // microCompact keeps N most recent tool messages
	MaxOutputLen   = 500000 // max bytes per tool output (truncation limit, 500KB)

	// KeepRecentMessages: AutoCompact keeps this many recent messages verbatim;
	// older prefix is summarized. Snapped to a safe turn boundary.
	KeepRecentMessages = 20

	// CompactionThresholdFrac: AutoCompact fires when tokens exceed this
	// fraction of the context window.
	CompactionThresholdFrac = 0.75
)

// Model context-window sizes (tokens), matched by model-id prefix.
const (
	ContextWindowClaude  = 200000
	ContextWindowGPT     = 128000
	ContextWindowGemini  = 1000000
	ContextWindowDefault = 128000
)

const (
	PollInterval = 5 * time.Second  // idle teammate polls inbox this often
	IdleTimeout  = 60 * time.Second // idle teammate shuts down after this
)

const (
	// LlmMaxRetries: 5 retries to outlast typical ~60s gateway rate-limit windows.
	LlmMaxRetries = 5
	LlmBaseDelay  = 1 * time.Second
	// LlmRateLimitDelay: 429-specific base backoff, above typical throttle window.
	LlmRateLimitDelay = 10 * time.Second
	// LlmMaxDelay: per-attempt backoff cap matching typical gateway rate-limit window.
	LlmMaxDelay = 60 * time.Second

	// LlmCallTimeout caps one provider Call/Stream attempt, preventing a hung
	// backend from freezing the agent loop.
	LlmCallTimeout = 5 * time.Minute

	// LlmHTTPTimeout: HTTP-client level timeout, intentionally larger than
	// LlmCallTimeout so the per-call ctx deadline fires first; hard backstop.
	LlmHTTPTimeout = 6 * time.Minute

	// Process-wide LLM throttle (shared by main agent + subagents). Override via
	// LLM_MAX_QPS / LLM_MAX_BURST / LLM_MAX_CONCURRENCY.
	LlmDefaultMaxQPS         = 4.0
	LlmDefaultMaxBurst       = 8
	LlmDefaultMaxConcurrency = 4

	// SpawnMinInterval staggers teammate Spawn calls so their first LLM hits don't coincide.
	SpawnMinInterval = 750 * time.Millisecond
)

// MaxActiveWorktrees caps concurrent teammate git worktrees to prevent disk exhaustion.
const MaxActiveWorktrees = 10

const (
	MemoryTTLDays        = 90   // daily files older than this are auto-deleted
	DeduplicateThreshold = 0.7  // Jaccard similarity threshold for dedup
	MaxMemoryContentLen  = 2000 // max chars per memory entry
	MaxEvergreenChars    = 8000 // MEMORY.md truncation when injecting to prompt
)

const (
	Bm25K1 = 1.5  // tf saturation: higher → less saturation (1.2-2.0 typical)
	Bm25B  = 0.75 // length normalization: 0 = none, 1 = full (0.75 is Lucene default)
)

const (
	// Weights for merging BM25 + vector scores. Both normalized to [0,1]; weights sum to 1.
	HybridKeywordWeight = 0.65
	HybridVectorWeight  = 0.35
)

const (
	PlanRequestTTL  = 30 * time.Minute // pending requests expire after 30 min
	ApprovedPlanTTL = 24 * time.Hour   // approved/rejected requests expire after 24h
)

const (
	BashTimeout = 120 * time.Second // bash / background_run default timeout
)

// Per-tool execution safety: a hard ceiling on each handler invocation.
const (
	PerToolTimeout = 5 * time.Minute // hard ceiling per tool handler call

	// SubagentTimeout: a more generous ceiling for the task tool, overriding PerToolTimeout.
	SubagentTimeout = 10 * time.Minute

	// SubagentSoftDeadlineBuffer: runSubagent self-terminates with a progress summary
	// before the hard deadline fires, instead of being hard-killed.
	SubagentSoftDeadlineBuffer = 30 * time.Second

	// TokenCheckInterval: re-check tokens at most every N rounds (estimateTokens is O(N)).
	TokenCheckInterval = 3

	// LessonRoundsLimit: max extra rounds after lessonsWritten to persist the lesson.
	LessonRoundsLimit = 3

	// PlanningGateMinTaskChars: skip planning-gate for trivial short queries.
	PlanningGateMinTaskChars = 80
)

const (
	MaxTeamMessageSize = 64 * 1024 // 64KB max team message size (prevents inbox flooding)
)

// LLM-as-Judge verification (see judge.go). Configured via JUDGE_* env vars:
//
//	JUDGE_ENABLED   turn the judge on        (1 | true | yes | on)
//	JUDGE_MODEL     judge model id           (empty = reuse main model)
//	JUDGE_MIN_SCORE retry threshold 1-10     (default JudgeMinScore)
//	JUDGE_PROVIDER  explicit backend SDK     (openai | anthropic | gemini)
//	JUDGE_API_KEY   judge-only key           (else the backend's standard key)
//	JUDGE_BASE_URL  judge-only endpoint      (else the backend's standard url)
const (
	JudgeMinScore        = 7 // verdicts below this force a retry (scale 1-10)
	JudgeMaxRetryInjects = 2 // at most N verification-failed injections per agentLoop run
)

// Human-in-the-loop approval (see human_approval.go).
const (
	HitlDefaultMode = "interactive" // interactive | auto-approve | auto-reject | notify-only
)
const (
	KindSystem     = "system"
	KindUser       = "user"
	KindAssistant  = "assistant"
	KindTool       = "tool"
	KindCheckpoint = "checkpoint"
)

// Outbound web access env vars (read by internal/web and internal/security, not via infra.Config).
//
//	WEB_ALLOW_PRIVATE_IPS  opt into private/internal network access, default deny
//	WEB_SEARCH_PROVIDER    force a backend: tavily|brave; unset = auto
//	WEB_SEARCH_API_KEY     API key for the forced provider
//	SEARXNG_URL            a specific SearXNG instance
//	SEARXNG_INSTANCES      comma-separated override of the built-in public list
const (
	WebFetchTimeout  = 20 * time.Second // web_fetch: whole request+redirects
	WebFetchMaxBytes = 2 * 1024 * 1024  // web_fetch: response body cap (2MB)
	WebSearchTimeout = 8 * time.Second  // web_search: per-backend timeout in the downgrade chain
)
