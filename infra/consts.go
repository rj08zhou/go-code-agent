package infra

import "time"

const AppRootDirName = ".go-code-agent"

// Project-wide configuration constants. All tunable thresholds and timeouts.

const (
	StuckThreshold         = 20    // rounds without completing a task = "stuck" (raised from 10: 10 fired too often on long multi-step tasks)
	ReflectInterval        = 20    // periodic reflection every N tool rounds (raised from 10: periodic+stuck were firing ~every 1-2 min)
	MaxConsecutiveFailures = 3     // same tool failing → force strategy change
	MaxRounds              = 100   // hard safety cap for agent loop
	LessonThreshold        = 3     // min tool rounds before auto-lesson prompt
	SubagentMaxRounds      = 30    // subagent inner loop cap
	TeammateWorkMaxRounds  = 50    // teammate workPhase inner loop cap
	DefaultMaxOutputTokens = 16384 // default max output tokens for LLM calls
)

const (
	TokenThreshold = 300000 // autoCompact trigger (estimated total tokens) - raised from 200000 to reduce compaction frequency
	KeepRecent     = 15     // microCompact keeps N most recent tool messages - raised from 10 to retain more context
	MaxOutputLen   = 500000 // max bytes per tool output (truncation limit, 500KB)

	// KeepRecentMessages is how many of the most recent conversation
	// messages AutoCompact keeps VERBATIM (progressive compaction): only
	// the older prefix is summarized. The actual split is snapped to a
	// safe turn boundary (never orphaning a tool_call/tool_result pair),
	// so this is a target, not an exact count. Distinct from KeepRecent
	// above, which governs microCompact's tool-result folding.
	KeepRecentMessages = 20

	// CompactionThresholdFrac: AutoCompact fires when estimated tokens
	// exceed this fraction of the model's context window (see
	// ContextWindowTokens). Kept below 1.0 to leave headroom for the
	// next turn's output + the summarization call itself. The absolute
	// TokenThreshold above is still honored as an upper cap.
	CompactionThresholdFrac = 0.75
)

// Model context-window sizes (in tokens), used to make the compaction
// threshold model-aware instead of a single hard-coded number. These
// are deliberately conservative round numbers matched by model-id
// prefix in ContextWindowTokens; an exact figure is unnecessary since
// CompactionThresholdFrac already leaves headroom.
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
	// LlmMaxRetries: most public LLM gateways throttle on a ~60s window;
	// 3 retries (cumulative ~43s of backoff) cannot reliably outlast that
	// window. Bumping to 5 pushes the cumulative wait to ~120s, which
	// covers the typical rate-limit window.
	LlmMaxRetries = 5
	LlmBaseDelay  = 1 * time.Second
	// LlmRateLimitDelay: 429-specific base backoff. Raised from 5s to 10s
	// so we do not keep retrying inside the same throttle window (which
	// only earns us another 429).
	LlmRateLimitDelay = 10 * time.Second
	// LlmMaxDelay: per-attempt backoff cap. Raised from 30s to 60s to
	// match the rate-limit window length of typical LLM gateways.
	LlmMaxDelay = 60 * time.Second

	// LlmCallTimeout caps the wall-clock time of one provider Call/Stream
	// attempt. Without it a hung backend (e.g. an OpenAI-compatible
	// gateway that holds the SSE socket open without sending chunks) can
	// freeze the agent loop indefinitely, with no subprocess and no
	// audit trail of where it stopped. Generous enough for a long
	// reasoning + large-output completion; the retry wrapper still gets
	// up to LlmMaxRetries chances after a timeout.
	LlmCallTimeout = 5 * time.Minute

	// LlmHTTPTimeout is the HTTP-client level timeout we install on the
	// underlying transport. It is intentionally larger than
	// LlmCallTimeout so the per-call ctx deadline is what fires first
	// in the normal case; the HTTP timeout is a hard backstop for
	// whatever ignores ctx (older SDKs, custom transports).
	LlmHTTPTimeout = 6 * time.Minute

	// Process-wide LLM throttle. Both the main agent and every spawned
	// subagent / teammate share one token-bucket + concurrency cap so
	// that fan-out (a reflect that spawns 3 verifiers) cannot overwhelm
	// the upstream gateway and trigger a 429 storm.
	//
	// Defaults are conservative; override via env vars at startup:
	//   LLM_MAX_QPS         float, requests-per-second  (default 2.0)
	//   LLM_MAX_BURST       int,   bucket capacity      (default 4)
	//   LLM_MAX_CONCURRENCY int,   in-flight calls cap  (default 2)
	LlmDefaultMaxQPS         = 2.0
	LlmDefaultMaxBurst       = 4
	LlmDefaultMaxConcurrency = 2

	// SpawnMinInterval throttles teammate Spawn calls so that a single
	// reflect step that fires off N subagents staggers their first LLM
	// hit instead of all hitting the gateway at the same instant.
	SpawnMinInterval = 750 * time.Millisecond
)

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
	// Weights for merging keyword (BM25) + vector (hash-based) scores.
	// Both inputs are normalized to [0, 1] before weighting, so weights sum should be 1.
	// Keyword weight is higher: BM25 is a much stronger signal than hash-vector
	// (which is essentially a random projection of bag-of-words).
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

// Per-tool execution safety
//
// Even though individual tools (bash, network, ...) own their own
// timeouts, we wrap each handler invocation in agentLoop with a
// hard ceiling so that a buggy / hung handler can never freeze the
// REPL. The ceiling is intentionally generous; well-behaved tools
// finish far below it.
const (
	PerToolTimeout = 5 * time.Minute // hard ceiling per tool handler call

	// SubagentTimeout is the task tool's own (more generous) hard
	// ceiling, overriding PerToolTimeout for that one tool. A read-only
	// subagent exploring a real codebase (many read_file/bash rounds)
	// routinely needs longer than the general-purpose 5-minute ceiling;
	// see SubagentSoftDeadlineBuffer below for how it avoids actually
	// hitting this hard limit in the common case.
	SubagentTimeout = 10 * time.Minute

	// SubagentSoftDeadlineBuffer: runSubagent checks its own elapsed
	// time against (deadline - this buffer) before starting each new
	// round, so it can stop itself and return a summary of progress
	// so far instead of being hard-killed by SubagentTimeout with its
	// entire investigation discarded (the failure mode we're fixing:
	// a task call that ran out of PerToolTimeout used to throw away
	// every file it had already read).
	SubagentSoftDeadlineBuffer = 30 * time.Second

	// estimateTokens is O(N); recomputing every round on long sessions
	// is wasteful. We re-check at most every TokenCheckInterval rounds.
	TokenCheckInterval = 3

	// After lessonsWritten=true, we allow at most this many extra rounds
	// for the model to actually persist the lesson via memory_write,
	// preventing an unbounded "post-lesson" tail.
	LessonRoundsLimit = 3

	// Skip the planning-gate prompt for trivial single-line user
	// queries (e.g., "read README", "what does X do?"). Length-based
	// heuristic; longer / multi-line tasks still get the gate.
	PlanningGateMinTaskChars = 80
)

const (
	MaxTeamMessageSize = 64 * 1024 // 64KB max team message size (prevents inbox flooding)
)

// LLM-as-Judge verification
//
// The judge runs a SECOND LLM call after task completion to evaluate whether
// the agent actually achieved the user's goal (vs. just claiming it did).
// See judge.go.
//
// The judge is configured entirely through JUDGE_* environment variables
// (model, endpoint, credentials and behaviour), so it is set up through one
// consistent mechanism rather than a mix of CLI flags and env vars:
//
//	JUDGE_ENABLED   turn the judge on        (1 | true | yes | on)
//	JUDGE_MODEL     judge model id           (empty = reuse main model)
//	JUDGE_MIN_SCORE retry threshold 1-10     (default JudgeMinScore)
//	JUDGE_PROVIDER  explicit backend SDK     (openai | anthropic | gemini)
//	JUDGE_API_KEY   judge-only key           (else the backend's standard key)
//	JUDGE_BASE_URL  judge-only endpoint      (else the backend's standard url)
//
// Backend routing (JUDGE_PROVIDER/API_KEY/BASE_URL) is resolved in
// llm.JudgeProvider; the rest is read by judgeConfigFromEnv.
const (
	JudgeMinScore        = 7 // verdicts below this force a retry (scale 1-10)
	JudgeMaxRetryInjects = 2 // at most N verification-failed injections per agentLoop run
)

// Human-in-the-loop approval
//
// When enabled, high-risk tool invocations (delete, bash, critical paths)
// pause the agent and ask an operator to approve / reject / modify.
// See human_approval.go.
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
