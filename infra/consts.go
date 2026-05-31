package infra

import "time"

const AppRootDirName = ".go-code-agent"

// Project-wide configuration constants. All tunable thresholds and timeouts.

const (
	StuckThreshold         = 10  // rounds without completing a task = "stuck"
	ReflectInterval        = 5   // periodic reflection every N tool rounds
	MaxConsecutiveFailures = 3   // same tool failing → force strategy change
	MaxRounds              = 100 // hard safety cap for agent loop
	LessonThreshold        = 3   // min tool rounds before auto-lesson prompt
	SubagentMaxRounds      = 30  // subagent inner loop cap
	TeammateWorkMaxRounds  = 50  // teammate workPhase inner loop cap
	DefaultMaxOutputTokens = 16384 // default max output tokens for LLM calls
)

const (
	TokenThreshold = 100000  // autoCompact trigger (estimated total tokens)
	KeepRecent     = 3       // microCompact keeps N most recent tool messages
	MaxOutputLen   = 500000  // max bytes per tool output (truncation limit, 500KB)
)

const (
	PollInterval = 5 * time.Second  // idle teammate polls inbox this often
	IdleTimeout  = 60 * time.Second // idle teammate shuts down after this
)

const (
	LlmMaxRetries     = 3
	LlmBaseDelay      = 1 * time.Second
	LlmRateLimitDelay = 5 * time.Second  // 429 专用：更长的基础退避，避免反复触发限流
	LlmMaxDelay       = 30 * time.Second // 最大退避上限（429 场景需要更长等待）

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
const (
	JudgeMinScore        = 7  // verdicts below this force a retry (scale 1-10)
	JudgeDefaultModel    = "" // empty = reuse main model; override via --judge-model
	JudgeMaxRetryInjects = 2  // at most N verification-failed injections per agentLoop run
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
