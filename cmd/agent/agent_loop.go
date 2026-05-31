package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/hitl_audit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/log"
	"strings"
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

// ----------------------------------------------------------------------------
// agentLoop
// ----------------------------------------------------------------------------

// agentLoop drives the think -> plan -> act -> reflect cycle.
func agentLoop(ctx context.Context, messages *[]llm.Message) error {
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
		sr, err := llm.StreamLLMWithRetry(ctx, "agent", llm.CallParams{Model: model, Messages: *messages, Tools: toolDefs, MaxTokens: infra.DefaultMaxOutputTokens})
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
			log.PrintSystem("[truncated] LLM output hit max_tokens, requesting continuation")
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
				*messages = append(*messages, llm.UserMessage(app.PromptLoader.Load("auto_lesson")))
				continue
			}
			return nil
		}

		// 5) Run tools, gathering classification + judge data.
		execResult := executeToolCalls(ctx, sr, messages, st)

		// 6) Planning module (see plan.go).
		originalTask := lastUserMessage(*messages)
		if prompt := checkPlanningGate(
			st.toolRounds, execResult.usedPlanning, execResult.usedThink, execResult.usedExplore,
			originalTask,
		); prompt != "" {
			*messages = append(*messages, llm.UserMessage(prompt))
		}
		if prompt := checkDAGDependency(st.toolRounds); prompt != "" {
			*messages = append(*messages, llm.UserMessage(prompt))
		}

		// 7) Judge first, then progress injection. Putting judge
		//    before the success-flavored progress summary avoids
		//    feeding the model contradictory signals when the judge
		//    rejects the verdict.
		if execResult.taskCompletedThisRound {
			if runJudgeIfApplicable(ctx, st, messages, execResult.roundToolResults) {
				if ps := app.DagSched().ProgressSummary(); ps != "" {
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
		prompts, resetFailures, resetTodoNag, resetStuck := reflect(
			st.consecutiveFailures, st.lastFailedTool, infra.MaxConsecutiveFailures,
			st.toolRounds, st.totalFailures,
			st.roundsSinceLastComplete, st.roundsWithoutTodo,
			infra.StuckThreshold, infra.ReflectInterval, app.Todo().HasOpenItems(),
		)
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
			log.PrintSystem("[manual compact]")
			*messages = autoCompact(ctx, *messages, system)
		}

		// 12) Lesson stage budget enforcement: once lessonsWritten,
		//     allow only lessonRoundsLimit additional rounds.
		if st.lessonsWritten {
			st.lessonRoundsRemaining--
			if st.lessonRoundsRemaining <= 0 {
				return nil
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Pre-round helpers
// ----------------------------------------------------------------------------

// preRound: token compression, drain background notifications, pull inbox.
func preRound(ctx context.Context, messages *[]llm.Message, st *loopState) {
	// micro-compact is in-place and cheap; safe to run every round.
	microCompact(*messages)

	if st.toolRounds-st.cachedTokensAt >= infra.TokenCheckInterval || st.cachedTokens == 0 {
		st.cachedTokens = estimateTokens(*messages)
		st.cachedTokensAt = st.toolRounds
	}
	if st.cachedTokens > infra.TokenThreshold {
		log.PrintSystem("[auto-compact triggered]")
		*messages = autoCompact(ctx, *messages, system)
		// After compaction the slice shrinks dramatically; force a
		// fresh estimate next round.
		st.cachedTokens = 0
	}

	// Drain background task notifications.
	if notifs := app.BgMgr().Drain(); len(notifs) > 0 {
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
	if inbox := app.Bus().ReadInbox("lead"); len(inbox) > 0 {
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
			log.PrintTool(tc.Name, out)
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
		if outcome == OutcomeSuccess && tc.Name == "TodoWrite" {
			res.usedTodo = true
		}

		if globalJudge.IsEnabled() {
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
	approved, reason := checkToolApproval(tc.Name)
	if !approved && !globalApproval.IsAutoApproveAll() {
		if _, isSafe := toolSecurityMap[tc.Name]; !isSafe || !globalApproval.IsAutoApproveSafe() {
			out := fmt.Sprintf("[SECURITY] %s", reason)
			log.PrintTool(tc.Name, out)
			*messages = append(*messages, llm.ToolMessage(out, tc.ID))
			return OutcomeSecurityBlocked, out
		}
	}

	if needReview, risk, hitlReason := hitl_audit.HitlManager.NeedsReview(tc.Name, tc.Arguments); needReview {
		sid := ""
		if app != nil && app.SessionManager.Active() != nil {
			sid = app.SessionManager.Active().ID()
		}
		resp := hitl_audit.HitlManager.RequestApproval(hitl_audit.HITLRequest{
			ToolName:  tc.Name,
			Arguments: tc.Arguments,
			RiskLevel: risk,
			Reason:    hitlReason,
			SessionID: sid,
		})
		switch resp.Decision {
		case hitl_audit.HITLReject:
			out := fmt.Sprintf("[HITL-REJECTED] %s", hitlReason)
			log.PrintTool(tc.Name, out)
			*messages = append(*messages, llm.ToolMessage(out, tc.ID))
			*hitlNotes = append(*hitlNotes, hitl_audit.FormatRejectMessage(tc.Name, hitlReason))
			return OutcomeHITLRejected, out
		case hitl_audit.HITLModify:
			out := fmt.Sprintf("[HITL-MODIFY] %s", truncate(resp.Feedback, 200))
			log.PrintTool(tc.Name, out)
			*messages = append(*messages, llm.ToolMessage(out, tc.ID))
			*hitlNotes = append(*hitlNotes, hitl_audit.FormatModifyMessage(tc.Name, resp.Feedback))
			return OutcomeHITLModified, out
		case hitl_audit.HITLApprove:
			// fall through
		}
	}

	// --- execute under timeout ---
	result := runToolWithTimeout(ctx, tc)
	log.PrintTool(tc.Name, result.Output)
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

// runToolWithTimeout invokes a handler with perToolTimeout + snapshot/rollback.
func runToolWithTimeout(ctx context.Context, tc llm.ToolCall) ToolResult {
	return withSnapshot(tc.Name, func() ToolResult {
		tCtx, cancel := context.WithTimeout(ctx, infra.PerToolTimeout)
		defer cancel()

		resultCh := make(chan ToolResult, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					resultCh <- mkErr(fmt.Sprintf("tool panicked: %v", r))
				}
			}()
			resultCh <- invokeToolHandler(tCtx, tc)
		}()

		select {
		case out := <-resultCh:
			return out
		case <-tCtx.Done():
			if ctx.Err() != nil {
				return mkErr(fmt.Sprintf("tool '%s' cancelled: %v", tc.Name, ctx.Err()))
			}
			return mkErr(fmt.Sprintf("tool '%s' exceeded %s timeout", tc.Name, infra.PerToolTimeout))
		}
	})
}

// invokeToolHandler resolves and runs the handler (built-in, MCP, or fallback).
func invokeToolHandler(ctx context.Context, tc llm.ToolCall) ToolResult {
	if h, ok := toolHandlers[tc.Name]; ok {
		result := h(ctx, json.RawMessage(tc.Arguments))
		return result
	}
	if mcpMgr != nil && mcpMgr.IsMCPTool(tc.Name) {
		result := mcpMgr.Call(tc.Name, json.RawMessage(tc.Arguments))
		if secretsSanitizer.Detect(result.Output) {
			log.PrintSystem(fmt.Sprintf("[security] potential secrets detected in MCP output '%s'", tc.Name))
			result.Output = secretsSanitizer.Sanitize(result.Output)
		}
		return result
	}
	return mkErr(fmt.Sprintf("Unknown tool: %s", tc.Name))
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
	if !globalJudge.IsEnabled() {
		return true
	}
	if st.judgeRetryInjects >= infra.JudgeMaxRetryInjects {
		return true
	}

	originalTask := lastUserMessage(*msgs)
	verdict, err := globalJudge.Verify(ctx, originalTask, *msgs, results)
	if err != nil {
		log.PrintSystem(fmt.Sprintf("[judge] verify error: %v", err))
		return true // soft-fail open
	}
	if verdict == nil {
		return true
	}

	log.PrintSystem(fmt.Sprintf("[judge] score=%d approved=%v retry=%v reason=%s",
		verdict.Score, verdict.Approved, verdict.ShouldRetry,
		truncate(verdict.Reason, 120)))

	if verdict.Approved && !verdict.ShouldRetry {
		return true
	}

	*msgs = append(*msgs, llm.UserMessage(verdict.FormatFeedback()))
	st.judgeRetryInjects++
	if verdict.Score <= 3 {
		*msgs = append(*msgs, llm.UserMessage(app.PromptLoader.Load("judge_critical")))
	}
	return false
}

// finalizeMaxRounds wraps up when the hard maxRounds ceiling is hit.
func finalizeMaxRounds(ctx context.Context, messages *[]llm.Message) error {
	*messages = append(*messages, llm.UserMessage(
		"<limit>Maximum tool rounds reached. Wrap up and respond now in plain text.</limit>"))
	sr, err := llm.StreamLLMWithRetry(ctx, "agent-final", llm.CallParams{Model: model, Messages: *messages, Tools: nil})
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
