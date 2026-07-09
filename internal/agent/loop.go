package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/security"
	"go-code-agent/utils"
	"strings"
	"time"
)

// ----------------------------------------------------------------------------
// Tool outcome model
// ----------------------------------------------------------------------------

// ToolOutcome classifies a tool-call dispatch result.
type ToolOutcome int

const (
	OutcomeSuccess ToolOutcome = iota
	OutcomeFailure
	OutcomeSecurityBlocked
	OutcomeHITLRejected
	OutcomeHITLModified
	OutcomeTimeout
)

// lastUserMessage returns the most recent real user message (skipping
// control blocks injected by the loop).
func lastUserMessage(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		c := strings.TrimSpace(msgs[i].Content)
		if c == "" {
			continue
		}
		if strings.HasPrefix(c, "<") {
			continue // control block injected by the loop itself
		}
		return c
	}
	return ""
}

// ----------------------------------------------------------------------------
// Loop state
// ----------------------------------------------------------------------------

// loopState owns mutable counters driving reflection, planning, and judge.
type loopState struct {
	toolRounds              int
	consecutiveFailures     int
	totalFailures           int
	lastFailedTool          string
	roundsSinceLastComplete int
	roundsWithoutTodo       int

	// auto-lesson stage
	lessonsWritten        bool
	lessonRoundsRemaining int

	// judge stage
	judgeRetryInjects int

	// last-known token estimate, refreshed every tokenCheckInterval rounds
	cachedTokens   int
	cachedTokensAt int // toolRounds value when cachedTokens was computed

	// reflectLastTriggered records the toolRounds value at which each
	// reflection trigger kind last fired. Used by reflect() to
	// suppress duplicate prompts within a kind-specific cool-down
	// window. nil-safe: allocated lazily on first use.
	reflectLastTriggered map[string]int

	// touchedTasks is true once this turn has run any task-mutating
	// tool (see taskMutationTools). It gates the end-of-turn progress
	// summary: DAG tasks persist across turns, so without this a turn
	// that never touched tasks would still re-print the stale count of
	// whatever the session finished long ago (e.g. a perpetual
	// "<progress>4/4 tasks completed. All tasks done!</progress>" on
	// every unrelated turn). We only surface progress on turns that
	// actually changed task state.
	touchedTasks bool
}

// ----------------------------------------------------------------------------
// Static tool classification
// ----------------------------------------------------------------------------

// planningTools are tools that count as "planning activity".
var planningTools = map[string]bool{
	"task_create": true, "task_list": true, "task_update": true,
	"task_add_dep": true, "task_remove_dep": true,
	"task_dag": true, "task_ready": true, "task_get": true,
	"TodoWrite": true,
}

// exploreTools are read-only / discovery tools that count as
// "thinking activity" — gathering information before planning is
// encouraged. `think` is the most explicit form of thinking.
var exploreTools = map[string]bool{
	"think":         true,
	"memory_search": true, "memory_stats": true,
	"read_file": true, "search_content": true,
	"list_dir": true, "search_file": true,
}

// taskMutationTools are the tools that actually change the DAG-task set
// or a task's status - i.e. the state ProgressSummary reports. Used to
// decide whether a turn "touched tasks" and therefore whether its
// end-of-turn progress line is meaningful (vs. a stale re-print of a
// long-finished session's task count). Deliberately excludes read-only
// task queries (task_list/task_get/task_dag/task_ready) and TodoWrite
// (a separate TodoManager not counted by ProgressSummary).
var taskMutationTools = map[string]bool{
	"task_create":     true,
	"task_update":     true,
	"task_add_dep":    true,
	"task_remove_dep": true,
	"claim_task":      true,
}

// ----------------------------------------------------------------------------
// Run (the agent loop)
// ----------------------------------------------------------------------------

// Run drives the think -> plan -> act -> reflect cycle for one user turn.
func Run(ctx context.Context, messages *[]llm.Message) error {
	// Generate trace ID for usage correlation.
	traceID := llm.NewTraceID()
	ctx = llm.WithTraceID(ctx, traceID)

	st := &loopState{}

	for {
		// 0) Cancellation check — Ctrl-C / parent ctx done.
		if err := ctx.Err(); err != nil {
			return err
		}

		// 1) Hard safety cap.
		if st.toolRounds >= infra.MaxRounds {
			return finalizeMaxRounds(ctx, messages)
		}

		// 2) Pre-round housekeeping: compress, drain background / inbox.
		preRound(ctx, messages, st)

		// 3) LLM call.
		sr, err := llm.NewClient(nil).StreamWithRetry(ctx, "agent", llm.CallParams{Model: App.Model, Messages: *messages, Tools: ToolDefs, MaxTokens: infra.DefaultMaxOutputTokens})
		if err != nil {
			return fmt.Errorf("API call failed: %w", err)
		}
		if sr == nil {
			return fmt.Errorf("API call failed: empty response")
		}
		*messages = append(*messages, sr.ToAssistantMessage())

		// 3.5) Truncation detection: if the model hit its output token
		//      limit, the response is incomplete. Inject a continuation
		//      prompt so the model can finish its thought / tool calls.
		if sr.FinishReason == "length" {
			logging.PrintSystem("[truncated] LLM output hit max_tokens, requesting continuation")
			*messages = append(*messages, llm.UserMessage(
				"<system>Your previous response was truncated due to output length limits. "+
					"Some tool calls may have been lost. Please continue from where you left off. "+
					"Do NOT repeat tool calls that already succeeded above.</system>"))
			continue
		}

		// 4) No tool calls → either we're done, or we need one more
		//    round to write the auto-lesson.
		if len(sr.ToolCalls) == 0 {
			if shouldRequestLesson(st) {
				st.lessonsWritten = true
				st.lessonRoundsRemaining = infra.LessonRoundsLimit
				logging.PrintDecision(DecisionMemory, "task finished after a long run — asking the model to record lessons/preferences to long-term memory (persists across sessions)")
				*messages = append(*messages, llm.UserMessage(App.PromptLoader.Load("auto_lesson")))
				continue
			}
			return finalizeTurn(st)
		}

		// 5) Run tools, gathering classification + judge data.
		execResult := executeToolCalls(ctx, sr, messages, st)

		// 6) Planning module (see plan.go).
		originalTask := lastUserMessage(*messages)
		if prompt := checkPlanningGate(
			st.toolRounds, execResult.usedPlanning, execResult.usedThink, execResult.usedExplore,
			originalTask,
		); prompt != "" {
			logging.PrintDecision(DecisionPlan, "injected a planning gate (enforcing think -> plan -> act before execution)")
			*messages = append(*messages, llm.UserMessage(prompt))
		}
		if prompt := checkDAGDependency(st.toolRounds); prompt != "" {
			logging.PrintDecision(DecisionPlan, "tasks created without dependencies — nudging the model to define a DAG before executing")
			*messages = append(*messages, llm.UserMessage(prompt))
		}

		// 7) Judge first, then progress injection. Putting judge
		//    before the success-flavored progress summary avoids
		//    feeding the model contradictory signals when the judge
		//    rejects the verdict.
		if execResult.taskCompletedThisRound {
			if runJudgeIfApplicable(ctx, st, messages, execResult.roundToolResults) {
				if ps := App.DagSched().ProgressSummary(); ps != "" {
					*messages = append(*messages, llm.UserMessage(ps))
				}
			} else {
				// Judge rejected — un-mark completion so post-round
				// bookkeeping doesn't reset failure counters as if done.
				execResult.taskCompletedThisRound = false
			}
		}

		// 8) Apply deferred HITL feedback (one consolidated user message).
		if execResult.hitlFeedback != "" {
			*messages = append(*messages, llm.UserMessage(execResult.hitlFeedback))
		}

		// 9) Bookkeeping for this round.
		st.toolRounds++
		if execResult.touchedTasks {
			st.touchedTasks = true
		}
		if execResult.taskCompletedThisRound {
			st.roundsSinceLastComplete = 0
		} else {
			st.roundsSinceLastComplete++
		}
		if execResult.usedTodo {
			st.roundsWithoutTodo = 0
		} else {
			st.roundsWithoutTodo++
		}

		// 10) Reflection module (see reflection.go).
		if st.reflectLastTriggered == nil {
			st.reflectLastTriggered = make(map[string]int)
		}
		prompts, resetFailures, resetTodoNag, resetStuck, triggered := reflect(
			st.consecutiveFailures, st.lastFailedTool, infra.MaxConsecutiveFailures,
			st.toolRounds, st.totalFailures,
			st.roundsSinceLastComplete, st.roundsWithoutTodo,
			infra.StuckThreshold, infra.ReflectInterval, App.Todo().HasOpenItems(),
			st.reflectLastTriggered,
		)
		// Record the round at which each kind fired so the next
		// invocation can honor its cool-down. Only stamp kinds that
		// actually emitted a prompt this round.
		for _, kind := range triggered {
			st.reflectLastTriggered[kind] = st.toolRounds
		}
		if len(prompts) > 0 {
			// Determine reflection type based on consecutiveFailures
			reflectKind := "reflection"
			if st.consecutiveFailures >= infra.MaxConsecutiveFailures {
				reflectKind = "strategy-change"
			} else if st.consecutiveFailures > 0 {
				reflectKind = "self-correction"
			}
			logging.PrintDecision(DecisionReflect, fmt.Sprintf("%s triggered — injected %d reflection/strategy prompt(s) (consecutiveFailures=%d, roundsSinceComplete=%d, kinds=%v)",
				reflectKind, len(prompts), st.consecutiveFailures, st.roundsSinceLastComplete, triggered))
		}
		for _, p := range prompts {
			*messages = append(*messages, llm.UserMessage(p))
		}
		if resetFailures {
			st.consecutiveFailures = 0
		}
		if resetTodoNag {
			st.roundsWithoutTodo = 0
		}
		if resetStuck {
			st.roundsSinceLastComplete = 0
		}

		// 11) Manual compress requested by LLM via the `compress` tool.
		if execResult.manualCompress {
			logging.PrintSystem("[manual compact]")
			*messages = AutoCompact(ctx, *messages, App.System)
		}

		// 12) Lesson stage budget enforcement: once lessonsWritten,
		//     allow only lessonRoundsLimit additional rounds.
		if st.lessonsWritten {
			st.lessonRoundsRemaining--
			if st.lessonRoundsRemaining <= 0 {
				return finalizeTurn(st)
			}
		}
	}
}

// finalizeTurn emits a concise end-of-turn summary so the user can see
// the loop completed a full request cycle instead of silently dropping
// back to the prompt. It is the single normal-completion exit: every
// non-error `return nil` from Run should flow through here so the
// closing signal stays consistent across exit paths.
func finalizeTurn(st *loopState) error {
	var b strings.Builder
	fmt.Fprintf(&b, "turn complete — %d tool round(s)", st.toolRounds)
	if st.totalFailures > 0 {
		fmt.Fprintf(&b, ", %d tool failure(s)", st.totalFailures)
	}
	// Only surface the DAG progress summary when this turn actually
	// changed task state. Tasks persist across turns, so on a turn that
	// never touched them this would otherwise re-print a stale count
	// from a long-finished session (the perpetual "4/4 tasks completed.
	// All tasks done!" that had nothing to do with the current turn).
	if st.touchedTasks && App != nil && App.DagSched() != nil {
		if ps := App.DagSched().ProgressSummary(); ps != "" {
			b.WriteString(" — ")
			b.WriteString(ps)
		}
	}
	logging.PrintDecision(DecisionTurn, b.String())
	return nil
}

// ----------------------------------------------------------------------------
// Pre-round helpers
// ----------------------------------------------------------------------------

// preRound: token compression, drain background notifications, pull inbox.
func preRound(ctx context.Context, messages *[]llm.Message, st *loopState) {
	// micro-compact: only run every N rounds to avoid excessive context loss.
	// Surface a decision event when it actually folds something.
	const microCompactInterval = 6 // run microCompact every 6 rounds (raised from 3: was compacting ~1x/min, too noisy)
	if st.toolRounds%microCompactInterval == 0 {
		if cleared := microCompact(*messages); cleared > 0 {
			logging.PrintDecision(DecisionContext, fmt.Sprintf(
				"micro-compacted %d old tool result(s) out of context (kept most recent %d)",
				cleared, infra.KeepRecent))
		}
	}

	if st.toolRounds-st.cachedTokensAt >= infra.TokenCheckInterval || st.cachedTokens == 0 {
		st.cachedTokens = estimateTokens(*messages)
		st.cachedTokensAt = st.toolRounds
	}
	compactAt := infra.CompactionThreshold(App.Model)
	if st.cachedTokens > compactAt {
		logging.PrintDecision(DecisionContext, fmt.Sprintf(
			"context ~%d tokens exceeded threshold %d (model %q) — auto-compacting: summarizing older history, keeping recent messages verbatim",
			st.cachedTokens, compactAt, App.Model))
		*messages = AutoCompact(ctx, *messages, App.System)
		// After compaction the slice shrinks dramatically; force a
		// fresh estimate next round.
		st.cachedTokens = 0
	}

	// Drain background task notifications.
	if notifs := App.BgMgr().Drain(); len(notifs) > 0 {
		var lines []string
		for _, n := range notifs {
			lines = append(lines, fmt.Sprintf("[bg:%s] %s: %s", n["task_id"], n["status"], n["result"]))
		}
		*messages = append(*messages,
			llm.UserMessage(fmt.Sprintf("<background-results>\n%s\n</background-results>", strings.Join(lines, "\n"))),
			llm.AssistantMessage("Noted background results."),
		)
	}

	// Check lead inbox for teammate messages.
	if inbox := App.Bus().ReadInbox("lead"); len(inbox) > 0 {
		data, _ := json.MarshalIndent(inbox, "", "  ")
		*messages = append(*messages,
			llm.UserMessage(fmt.Sprintf("<inbox>%s</inbox>", string(data))),
			llm.AssistantMessage("Noted inbox messages."),
		)
	}
}

// ----------------------------------------------------------------------------
// Tool execution
// ----------------------------------------------------------------------------

// roundExecResult collects per-round tool execution outcomes.
type roundExecResult struct {
	usedTodo               bool
	manualCompress         bool
	usedPlanning           bool
	usedExplore            bool
	usedThink              bool
	taskCompletedThisRound bool
	touchedTasks           bool

	hitlFeedback string

	roundToolResults []JudgeToolResult
}

// executeToolCalls runs all tool calls with security/HITL gates and timeout.
func executeToolCalls(ctx context.Context, sr *llm.StreamResult, messages *[]llm.Message, st *loopState) roundExecResult {
	res := roundExecResult{
		usedThink: sr.Content != "",
	}

	var hitlNotes []string

	for _, tc := range sr.ToolCalls {
		// Guard: skip tool calls with truncated/invalid JSON arguments.
		// This happens when the LLM output is cut off by max_tokens mid-
		// way through a tool_call's arguments field.
		if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
			out := fmt.Sprintf("[SKIPPED] tool call '%s' has truncated arguments (incomplete JSON)", tc.Name)
			logging.PrintTool(tc.Name, out)
			*messages = append(*messages, llm.ToolMessage(out, tc.ID))
			continue
		}

		if tc.Name == "compress" {
			res.manualCompress = true
		}
		if planningTools[tc.Name] {
			res.usedPlanning = true
		}
		if exploreTools[tc.Name] {
			res.usedExplore = true
		}

		outcome, output := dispatchTool(ctx, tc, messages, &hitlNotes)

		switch outcome {
		case OutcomeFailure, OutcomeTimeout:
			st.totalFailures++
			if tc.Name == st.lastFailedTool {
				st.consecutiveFailures++
			} else {
				st.consecutiveFailures = 1
				st.lastFailedTool = tc.Name
			}
		case OutcomeSuccess:
			st.consecutiveFailures = 0
			st.lastFailedTool = ""
		case OutcomeSecurityBlocked, OutcomeHITLRejected, OutcomeHITLModified:
		}

		if outcome == OutcomeSuccess && tc.Name == "task_update" && strings.Contains(tc.Arguments, `"completed"`) {
			res.taskCompletedThisRound = true
		}
		if outcome == OutcomeSuccess && taskMutationTools[tc.Name] {
			res.touchedTasks = true
		}
		if outcome == OutcomeSuccess && tc.Name == "TodoWrite" {
			res.usedTodo = true
		}

		if App.Judge.IsEnabled() {
			res.roundToolResults = append(res.roundToolResults, JudgeToolResult{
				ToolName: tc.Name,
				Args:     tc.Arguments,
				Output:   output,
				Failed:   outcome == OutcomeFailure || outcome == OutcomeTimeout,
			})
		}
	}

	if len(hitlNotes) > 0 {
		res.hitlFeedback = "<human-feedback>\n" + strings.Join(hitlNotes, "\n") + "\n</human-feedback>"
	}

	return res
}

// dispatchTool runs security gate, HITL gate, then the handler under timeout.
func dispatchTool(ctx context.Context, tc llm.ToolCall, messages *[]llm.Message, hitlNotes *[]string) (ToolOutcome, string) {
	// checkToolApproval already encodes the tool's ApprovalLevel together
	// with the current /approve setting (auto/safe/danger/off), so its
	// returned bool is authoritative — nothing extra to layer on top.
	//
	// NOTE: an earlier version re-checked `ToolSecurityMap[tc.Name]` here
	// (as a proxy for "is this tool safe-level") before honoring
	// IsAutoApproveSafe(). Because that lookup's second return value is
	// just "found in the map" (not the tool's actual level), it made
	// `/approve safe` accidentally auto-approve every *known* tool
	// including ApproveDanger ones (bash, delete_file, ...). Trusting
	// checkToolApproval's own bool fixes that and also means unknown /
	// ApproveBlocked tools stay blocked even under `/approve danger`
	// (fail-safe default), instead of slipping through via the old
	// `!globalApproval.IsAutoApproveAll()` short-circuit.
	if approved, reason := checkToolApproval(tc.Name, tc.Arguments); !approved {
		out := fmt.Sprintf("[SECURITY] %s", reason)
		logging.PrintTool(tc.Name, out)
		*messages = append(*messages, llm.ToolMessage(out, tc.ID))
		return OutcomeSecurityBlocked, out
	}

	if g := runHITLGate(tc); g != nil {
		if g.Rejected {
			out := fmt.Sprintf("[HITL-REJECTED] %s", g.Reason)
			logging.PrintTool(tc.Name, out)
			*messages = append(*messages, llm.ToolMessage(out, tc.ID))
			*hitlNotes = append(*hitlNotes, hitlaudit.FormatRejectMessage(tc.Name, g.Reason))
			return OutcomeHITLRejected, out
		}
		out := fmt.Sprintf("[HITL-MODIFY] %s", utils.Truncate(g.Feedback, 200))
		logging.PrintTool(tc.Name, out)
		*messages = append(*messages, llm.ToolMessage(out, tc.ID))
		*hitlNotes = append(*hitlNotes, hitlaudit.FormatModifyMessage(tc.Name, g.Feedback))
		return OutcomeHITLModified, out
	}

	// --- execute under timeout ---
	result := runToolWithTimeout(ctx, tc)
	logging.PrintTool(tc.Name, result.Output)
	*messages = append(*messages, llm.ToolMessage(result.Output, tc.ID))

	if !result.OK {
		// Determine if it's a timeout or a plain failure by sniffing
		// the output (runToolWithTimeout decorates timeout/ancel
		// messages with "timed out" / "cancelled" keywords).
		lower := strings.ToLower(result.Output)
		if strings.Contains(lower, "timed out") || strings.Contains(lower, "cancelled") {
			return OutcomeTimeout, result.Output
		}
		return OutcomeFailure, result.Output
	}
	return OutcomeSuccess, result.Output
}

// interactiveConfirmTools are handlers that may block on synchronous,
// unbounded-duration terminal input - write_file/edit_file's diff-
// preview confirm (security.PreviewAndConfirm) - rather than on real
// work. They are run directly in the calling goroutine by
// runToolWithTimeout below instead of the timeout-wrapped goroutine,
// for two independent reasons:
//
//  1. A human's decision time has nothing to do with how long a tool
//     should be allowed to run. Wrapping the confirm prompt in
//     PerToolTimeout means the operator can be mid-read of a diff
//     when the tool is force-timed-out out from under them - their
//     eventual answer is simply discarded once resultCh's writer loses
//     the race in the select below.
//  2. security.ReadLine (see main.go) is wired to the same
//     chzyer/readline Instance driving the main REPL loop. That
//     Instance is designed to be read sequentially - one Readline()
//     call at a time - and is normally only ever driven from this one
//     goroutine; running the confirm prompt in a second, timeout-
//     wrapped goroutine adds a needless (if likely benign) doubt about
//     cross-goroutine reentrancy for zero benefit, since these tools'
//     actual file I/O is local and fast regardless.
//
// bash is deliberately NOT in this set: most bash calls need no
// confirmation and DO need the timeout ceiling to stop a hung command
// from freezing the REPL forever (that protection matters far more
// than the rarer bash-danger-confirm path colliding with it).
var interactiveConfirmTools = map[string]bool{
	"write_file": true,
	"edit_file":  true,
}

// toolTimeoutOverrides lets specific tools use a longer hard ceiling
// than the general-purpose PerToolTimeout, without weakening the
// default protection for every other tool. task is the only entry
// today: a read-only subagent exploring a real codebase routinely
// needs more than 5 minutes of read_file/bash rounds. runSubagent
// paired with this uses infra.SubagentSoftDeadlineBuffer to return a
// progress summary before this hard ceiling would otherwise fire and
// discard its entire investigation (see runSubagent's doc comment).
var toolTimeoutOverrides = map[string]time.Duration{
	"task": infra.SubagentTimeout,
}

// runToolWithTimeout invokes a handler with perToolTimeout + snapshot/rollback.
// interactiveConfirmTools bypass the timeout wrapper - see its doc comment.
func runToolWithTimeout(ctx context.Context, tc llm.ToolCall) ToolResult {
	return withSnapshot(tc.Name, func() ToolResult {
		if interactiveConfirmTools[tc.Name] {
			return invokeToolHandler(ctx, tc)
		}

		timeout := infra.PerToolTimeout
		if override, ok := toolTimeoutOverrides[tc.Name]; ok {
			timeout = override
		}
		tCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		resultCh := make(chan ToolResult, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					resultCh <- llm.MkErr(fmt.Sprintf("tool panicked: %v", r))
				}
			}()
			resultCh <- invokeToolHandler(tCtx, tc)
		}()

		select {
		case out := <-resultCh:
			return out
		case <-tCtx.Done():
			if ctx.Err() != nil {
				return llm.MkErr(fmt.Sprintf("tool '%s' cancelled: %v", tc.Name, ctx.Err()))
			}
			return llm.MkErr(fmt.Sprintf("tool '%s' exceeded %s timeout", tc.Name, timeout))
		}
	})
}

// invokeToolHandler resolves and runs the handler (built-in, MCP, or fallback).
func invokeToolHandler(ctx context.Context, tc llm.ToolCall) ToolResult {
	if h, ok := ToolHandlers[tc.Name]; ok {
		result := h(ctx, json.RawMessage(tc.Arguments))
		return result
	}
	if App.MCPMgr != nil && App.MCPMgr.IsMCPTool(tc.Name) {
		result := App.MCPMgr.Call(tc.Name, json.RawMessage(tc.Arguments))
		if security.GlobalSecretsSanitizer.Detect(result.Output) {
			logging.PrintSystem(fmt.Sprintf("[security] potential secrets detected in MCP output '%s'", tc.Name))
			result.Output = security.GlobalSecretsSanitizer.Sanitize(result.Output)
		}
		return result
	}
	return llm.MkErr(fmt.Sprintf("Unknown tool: %s", tc.Name))
}

// ----------------------------------------------------------------------------
// Auto-lesson + judge helpers
// ----------------------------------------------------------------------------

// shouldRequestLesson returns true when the LLM should write a lesson entry.
func shouldRequestLesson(st *loopState) bool {
	return st.toolRounds >= infra.LessonThreshold && !st.lessonsWritten
}

// runJudgeIfApplicable runs post-completion verification. Returns true
// if completion stands, false if judge rejected.
func runJudgeIfApplicable(ctx context.Context, st *loopState, msgs *[]llm.Message, results []JudgeToolResult) bool {
	if !App.Judge.IsEnabled() {
		return true
	}
	if st.judgeRetryInjects >= infra.JudgeMaxRetryInjects {
		return true
	}

	originalTask := lastUserMessage(*msgs)
	verdict, err := App.Judge.Verify(ctx, originalTask, *msgs, results, App.Model)
	if err != nil {
		logging.PrintSystem(fmt.Sprintf("[judge] verify error: %v", err))
		return true // soft-fail open
	}
	if verdict == nil {
		return true
	}

	logging.PrintDecision(DecisionJudge, fmt.Sprintf("self-evaluated completion: score=%d approved=%v retry=%v — %s",
		verdict.Score, verdict.Approved, verdict.ShouldRetry,
		utils.Truncate(verdict.Reason, 120)))

	if verdict.Approved && !verdict.ShouldRetry {
		return true
	}

	*msgs = append(*msgs, llm.UserMessage(verdict.FormatFeedback()))
	st.judgeRetryInjects++
	if verdict.Score <= 3 {
		*msgs = append(*msgs, llm.UserMessage(App.PromptLoader.Load("judge_critical")))
	}
	return false
}

// finalizeMaxRounds wraps up when the hard maxRounds ceiling is hit.
func finalizeMaxRounds(ctx context.Context, messages *[]llm.Message) error {
	*messages = append(*messages, llm.UserMessage(
		"<limit>Maximum tool rounds reached. Wrap up and respond now in plain text.</limit>"))
	sr, err := llm.NewClient(nil).StreamWithRetry(ctx, "agent-final", llm.CallParams{Model: App.Model, Messages: *messages, Tools: nil})
	if err != nil {
		// Even on error, leave a closing assistant turn so the next
		// REPL prompt isn't waiting on an unanswered user message.
		*messages = append(*messages, llm.AssistantMessage(
			"[interrupted] Hit max-rounds and the wrap-up call failed; please retry."))
		return fmt.Errorf("max-rounds wrap-up failed: %w", err)
	}
	if sr != nil && sr.Content != "" {
		*messages = append(*messages, sr.ToAssistantMessage())
	} else {
		*messages = append(*messages, llm.AssistantMessage(
			"Reached the maximum tool-rounds limit. Please refine the request and try again."))
	}
	return nil
}
