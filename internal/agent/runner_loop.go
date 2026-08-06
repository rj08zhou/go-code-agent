package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/utils"
	"strings"
	"time"
)

func (r *Runner) resetTurnState() {
	r.turn = newTurnState()
	if r.todoState != nil {
		r.turn.planning.HasOpenItems, _ = r.todoState()
	}
}

// emit fills session/agent identity and forwards to the event sink when set.
func (r *Runner) emit(e event.Event) {
	if r.eventSink == nil {
		return
	}
	if e.SessionID == "" && r.scope != nil {
		e.SessionID = r.scope.SessionID
	}
	if e.AgentID == "" && r.scope != nil {
		e.AgentID = r.scope.AgentID
	}
	r.eventSink.Emit(e)
}

// toolEventPayload exposes only non-sensitive display metadata for filesystem
// investigation tools. It never copies file contents or mutation text.
func toolEventPayload(tc llm.ToolCall) map[string]string {
	var args struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
		return nil
	}

	payload := map[string]string{}
	switch tc.Name {
	case "read_file":
		if args.Path == "" {
			return nil
		}
		payload["path"] = args.Path
		if args.Offset > 0 {
			payload["offset"] = fmt.Sprintf("%d", args.Offset)
		}
		if args.Limit > 0 {
			payload["limit"] = fmt.Sprintf("%d", args.Limit)
		}
	case "list_dir":
		payload["path"] = args.Path
		if args.Path == "" {
			payload["path"] = "."
		}
	case "search_file", "search_content":
		if args.Pattern == "" {
			return nil
		}
		payload["pattern"] = args.Pattern
		payload["path"] = args.Path
		if args.Path == "" {
			payload["path"] = "."
		}
	default:
		return nil
	}
	return payload
}

func (r *Runner) injectTurnContext(messages []llm.Message) []llm.Message {
	r.turn.originalTask = lastUserMessage(messages)
	messages = r.injectDynamicContext(messages)
	return r.injectMemoryRecall(messages)
}

func (r *Runner) injectDynamicContext(messages []llm.Message) []llm.Message {
	if r.dynamicContext == nil {
		return messages
	}
	block := strings.TrimSpace(r.dynamicContext())
	if block == "" {
		return messages
	}
	return append(messages, llm.UserMessage(block))
}

func (r *Runner) injectMemoryRecall(messages []llm.Message) []llm.Message {
	if r.memoryRecall == nil || r.turn.originalTask == "" {
		return messages
	}
	recalled := strings.TrimSpace(r.memoryRecall(r.turn.originalTask))
	if recalled == "" || recalled == "No memories found." {
		return messages
	}
	return append(messages, llm.UserMessage("<memory-recall>\n"+recalled+"\n</memory-recall>"))
}

// prepareRound runs pre-LLM housekeeping for one loop iteration.
// A non-nil early outcome means the caller should return immediately.
func (r *Runner) prepareRound(
	ctx context.Context,
	messages []llm.Message,
	modelID, traceID string,
	maxRounds, ctxWindowTokens int,
	out *TurnOutcome,
) (msgs []llm.Message, toolDefs []llm.ToolDef, early *TurnOutcome) {
	// Soft deadline (master pattern): explore/web_fetch subagents stop a
	// buffer before the hard ctx deadline and turn in a no-tools summary
	// instead of being cancelled mid-tool.
	if r.profile.Role == "explore" {
		if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl.Add(-config.SubagentSoftDeadlineBuffer)) {
			done := r.finalizeWithoutTools(ctx, messages, modelID, traceID, out,
				"soft_deadline",
				"<limit>Time budget almost exhausted. Do NOT call any tools. "+
					"Synthesize a concise summary from what you have already observed. "+
					"Mark anything unverified explicitly.</limit>")
			return messages, nil, &done
		}
	}

	// Drop one-time procedural nudges (reflection / judge / plan-gate /
	// convergence) that the model has already responded to. They carry no
	// durable context, so re-sending them every round just re-bills the
	// same tokens until compaction. Nudges not yet consumed (injected after
	// the last assistant message) are preserved so the model still sees
	// them on the upcoming call. Nudges are RoleUser messages that are not
	// persisted by the REPL, so removing them is transparent to history.
	if trimmed, removed := dropConsumedNudges(messages); removed > 0 {
		messages = trimmed
		r.turn.tokens.invalidateCache()
		r.emit(event.Event{
			Type:    event.ContextDecision,
			TraceID: traceID,
			Payload: map[string]string{
				"action":  "drop_nudges",
				"removed": fmt.Sprintf("%d", removed),
				"rounds":  fmt.Sprintf("%d", r.turn.rounds),
			},
		})
	}

	if r.turn.rounds >= maxRounds {
		done := r.finalizeWithoutTools(ctx, messages, modelID, traceID, out,
			"max_rounds",
			"<limit>Maximum tool rounds reached. Wrap up and respond now in plain text.</limit>")
		return messages, nil, &done
	}

	toolDefs = r.executor.ToolDefs()

	// --- Auto-compaction check ---
	if r.compress != nil && r.turn.rounds > 0 && r.turn.rounds%config.TokenCheckInterval == 0 {
		// Refresh cached token estimate every TokenCheckInterval rounds
		// (matching original preRound pattern).
		if r.turn.rounds-r.turn.tokens.CachedAt >= config.TokenCheckInterval || r.turn.tokens.Cached == 0 {
			r.turn.tokens.refreshCache(llm.EstimateRequestTokens(messages, toolDefs), r.turn.rounds)
		}
		shouldCompact := NeedsCompaction(messages, toolDefs, ctxWindowTokens)
		if r.profile.Role == "explore" && r.turn.tokens.Cached > config.SubagentCompactionThreshold {
			shouldCompact = true
		}
		if shouldCompact {
			if r.profile.Role == "explore" {
				r.turn.tokens.PromptUsed += int64(r.turn.tokens.Cached)
			}
			before := len(messages)
			messages = r.compress.AutoCompact(ctx, messages, r.profile.SystemPrompt)
			// Invalidate cache after a compaction attempt since the summary call
			// and any rebuilt message slice make the estimate stale.
			r.turn.tokens.invalidateCache()
			if len(messages) < before {
				r.emit(event.Event{
					Type:    event.ContextDecision,
					TraceID: traceID,
					Payload: map[string]string{
						"action":  "auto_compact",
						"summary": fmt.Sprintf("%d -> %d messages", before, len(messages)),
					},
				})
			}
		}
	}

	// --- MicroCompact: light-weight clearing of old tool results ---
	// Gated on actual context pressure (not a fixed cadence): only once
	// estimated usage crosses MicroCompactThresholdFrac of the window, and
	// only when it frees at least MicroCompactMinClearBytes. This mirrors
	// the trigger + clear_at_least design of Anthropic's server-side
	// context editing, so short/medium sessions keep their cache prefix
	// intact and we stop busting the cache while there's ample headroom.
	const microCompactInterval = 6
	if r.turn.rounds > 0 && r.turn.rounds%microCompactInterval == 0 {
		// Refresh the token estimate if the auto-compaction block above
		// didn't already do it this round (e.g. when compression is off).
		if r.turn.tokens.Cached == 0 || r.turn.rounds-r.turn.tokens.CachedAt >= config.TokenCheckInterval {
			r.turn.tokens.refreshCache(llm.EstimateRequestTokens(messages, toolDefs), r.turn.rounds)
		}
		microThreshold := int(float64(ctxWindowTokens) * config.MicroCompactThresholdFrac)
		if r.turn.tokens.Cached > microThreshold {
			cleared, reclaimed := MicroCompact(messages, config.MicroCompactMinClearBytes)
			if cleared > 0 {
				// Reclaimed bytes changed the message list; force a
				// re-estimate on the next token check.
				r.turn.tokens.invalidateCache()
				r.emit(event.Event{
					Type:    event.ContextDecision,
					TraceID: traceID,
					Payload: map[string]string{
						"action":    "micro_compact",
						"cleared":   fmt.Sprintf("%d", cleared),
						"reclaimed": fmt.Sprintf("%d", reclaimed),
						"rounds":    fmt.Sprintf("%d", r.turn.rounds),
					},
				})
			}
		}
	}

	// --- Planning gate (round 0 & 1 only) ---
	if r.planGate != nil && r.turn.rounds <= 1 {
		if planMsg, action := r.planGate.Eval(
			r.turn.rounds, r.turn.planning.PlanEstablished, r.turn.originalTask, r.taskBatchID(),
		); planMsg != "" {
			r.emit(event.Event{
				Type:    event.PlanningDecision,
				TraceID: traceID,
				Payload: map[string]string{
					"action": action,
				},
			})
			messages = append(messages, llm.UserMessage(planMsg))
		}
	}

	// --- Reflection check ---
	if r.reflection != nil {
		progressSummary := ""
		if r.taskProgress != nil {
			progressSummary = r.taskProgress()
		}
		taskCount := 0
		if r.turn.planning.HasOpenItems || r.turn.originalTask != "" {
			taskCount = 1
		}
		stuckThresh := config.StuckThreshold
		if r.profile.Role == "explore" {
			stuckThresh = config.ExploreStuckThreshold
		}
		reflPrompts, resetF, resetNag, resetStuck, triggered := r.reflection.Eval(
			r.turn.failure.Consecutive, r.turn.failure.LastTool,
			config.MaxConsecutiveFailures,
			r.turn.rounds, r.turn.failures,
			r.turn.failure.RoundsSinceComplete, r.turn.planning.RoundsWithoutTodo,
			stuckThresh, config.ReflectInterval,
			r.turn.planning.HasOpenItems, r.turn.planning.LastTriggered,
			taskCount, progressSummary,
		)
		for _, k := range triggered {
			r.turn.planning.markTriggered(k, r.turn.rounds)
			r.emit(event.Event{
				Type:    event.ReflectionTriggered,
				TraceID: traceID,
				Payload: map[string]string{
					"kind":                  k,
					"consecutive_fails":     fmt.Sprintf("%d", r.turn.failure.Consecutive),
					"rounds_since_complete": fmt.Sprintf("%d", r.turn.failure.RoundsSinceComplete),
					"prompt_count":          fmt.Sprintf("%d", len(reflPrompts)),
				},
			})
		}
		// Reset counters only after the events above captured the values
		// that actually satisfied the trigger conditions.
		if resetF {
			r.turn.failure.clearConsecutive()
		}
		if resetNag {
			r.turn.planning.clearRoundsWithoutTodo()
		}
		if resetStuck {
			r.turn.failure.clearRoundsSinceComplete()
		}
		for _, p := range reflPrompts {
			messages = append(messages, llm.UserMessage(p))
		}
	}

	return messages, toolDefs, nil
}

// callModel repairs tool pairing, enforces explore prompt budget, then streams.
// A non-nil early outcome means the caller should return immediately.
func (r *Runner) callModel(
	ctx context.Context,
	messages []llm.Message,
	toolDefs []llm.ToolDef,
	modelID, traceID string,
	out *TurnOutcome,
) (msgs []llm.Message, sr *llm.StreamResult, early *TurnOutcome) {
	// Repair any broken tool_call / tool_result pairing before the
	// request leaves the process. OpenAI-compatible providers (DeepSeek
	// included) hard-fail with 400 when a non-tool message interrupts
	// the response block.
	if repaired, n := ensureToolCallPairing(messages); n > 0 {
		messages = repaired
		r.emit(event.Event{
			Type:    event.ContextDecision,
			TraceID: traceID,
			Payload: map[string]string{
				"action": "repair_tool_pairing",
				"filled": fmt.Sprintf("%d", n),
				"rounds": fmt.Sprintf("%d", r.turn.rounds),
			},
		})
	}
	if r.profile.Role == "explore" {
		estimatedPrompt := int64(llm.EstimateRequestTokens(messages, toolDefs))
		if r.turn.tokens.PromptUsed > 0 {
			frac := float64(r.turn.tokens.PromptUsed) / float64(config.SubagentPromptTokenBudget)
			if !r.turn.tokens.BudgetWarnInjected && frac >= config.ExploreBudgetWarnFrac {
				messages = append(messages, llm.UserMessage(
					"<budget-warn>You have used most of your prompt budget. "+
						"Prefer synthesizing a summary now. At most one more targeted "+
						"read/search if a critical fact is still missing — then stop.</budget-warn>"))
				r.turn.tokens.BudgetWarnInjected = true
			}
		}
		if r.turn.tokens.PromptUsed > 0 && r.turn.tokens.PromptUsed+estimatedPrompt > config.SubagentPromptTokenBudget {
			// Turn in a summary instead of tearing the investigation
			// (master soft-deadline spirit applied to prompt budget).
			done := r.finalizeWithoutTools(ctx, messages, modelID, traceID, out,
				"prompt_budget",
				"<limit>Prompt budget exhausted. Do NOT call any tools. "+
					"Synthesize a concise, well-structured summary from the files and "+
					"search results already in this conversation. Mark anything not "+
					"verified explicitly. Partial answers beat an incomplete stub.</limit>")
			return messages, nil, &done
		}
		r.turn.tokens.PromptUsed += estimatedPrompt
	}

	started := time.Now()
	sr, err := r.gateway.Stream(ctx, r.profile.Role, llm.CallParams{
		Model:     modelID,
		Messages:  messages,
		Tools:     toolDefs,
		MaxTokens: r.profile.MaxTokens,
		Reasoning: reasoningRequestFromConfig(r.cfg),
	}, newPrefixedSink(r.profile.Role))
	if err != nil {
		out.Error = fmt.Errorf("API call failed: %w", err)
		out.Messages = messages
		return messages, nil, out
	}
	// Accumulate every usage dimension for the turn-level summary.
	r.turn.usage.Add(sr.Usage)
	r.emit(event.Event{
		Type:     event.ModelCalled,
		TraceID:  traceID,
		Duration: time.Since(started),
		Usage:    &sr.Usage,
	})
	messages = append(messages, sr.ToAssistantMessage())
	return messages, sr, nil
}

func (r *Runner) handleTruncation(messages []llm.Message, sr *llm.StreamResult, traceID string) []llm.Message {
	// Truncation detection: injected early so incomplete tool calls
	// are never executed (matching original). If the LLM hit its
	// output limit, ask it to continue from where it left off.
	//
	// OpenAI/DeepSeek require every tool_call_id to have a following
	// tool message. If the truncated assistant message already lists
	// tool_calls, synthesize error tool results before the user nudge
	// so the next request stays protocol-valid.
	r.emit(event.Event{
		Type:    event.ReflectionTriggered,
		TraceID: traceID,
		Payload: map[string]string{"kind": "truncated"},
	})
	for _, tc := range sr.ToolCalls {
		result := tool.Failed(fmt.Sprintf(
			"tool call '%s' was truncated before execution; please re-issue if still needed", tc.Name))
		messages = append(messages, llm.ToolMessage(result.ToToolMessage(), tc.ID))
	}
	return append(messages, llm.UserMessage(
		prompt.Render(r.loader().MustLoad("response_truncated"), map[string]string{})))
}

// handleNoToolCalls runs lesson/judge wrap-up when the model returns plain text.
// cont=true means the outer loop should continue (lesson or judge retry).
func (r *Runner) handleNoToolCalls(
	ctx context.Context,
	messages []llm.Message,
	modelID, traceID string,
	out *TurnOutcome,
) (msgs []llm.Message, cont bool, done TurnOutcome) {
	out.Completed = true

	// Auto-Lesson: after enough rounds with real tool failures, inject a
	// prompt asking the model to record lessons, then continue the loop.
	// Only for agents with memory capability (lead agent).
	// Subagents (explore/teammate) have CanMemory=false and
	// would fail trying to call memory_write.
	//
	// A run that completed without any tool execution failure does not need
	// a lesson: there is nothing to learn from success. Planning-gate denials
	// (beforeDenyEarly) are excluded from the failure count, so a run that was
	// merely told to plan first also does not trigger a lesson.
	if r.profile.CanMemory && r.turn.rounds >= config.LessonThreshold &&
		r.turn.failures > 0 && !r.turn.lesson.Written && r.lessonWriter != nil {
		r.turn.lesson.Written = true
		r.turn.lesson.RoundsRemaining = config.LessonRoundsLimit
		r.emit(event.Event{
			Type:    event.MemoryDecision,
			TraceID: traceID,
			Payload: map[string]string{"rounds": fmt.Sprintf("%d", r.turn.rounds)},
		})
		r.lessonWriter.RecordFailure(ctx, messages)
		if !r.turn.lesson.PromptInjected {
			r.turn.lesson.PromptInjected = true
			messages = append(messages, llm.UserMessage(
				"<auto-lesson>Record any lessons, preferences, or patterns learned in this session to long-term memory using memory_write.</auto-lesson>"))
		}
		out.Completed = false
		return messages, true, TurnOutcome{}
	}

	if r.judge != nil && r.judge.IsEnabled() && r.turn.judge.RetryInjects < config.JudgeMaxRetryInjects {
		taskText := r.turn.originalTask
		judgeResults := make([]JudgeToolResult, 0, len(out.ToolResults))
		for _, tr := range out.ToolResults {
			judgeResults = append(judgeResults, JudgeToolResult{
				ToolName: tr.Name,
				Args:     tr.Args,
				Status:   tr.Status,
				Output:   tr.Output,
			})
		}
		verdict, jerr := r.judge.Verify(ctx, taskText, messages, judgeResults, modelID)
		if verdict != nil {
			judgeEvent := event.Event{
				Type:    event.JudgeDecision,
				TraceID: traceID,
				Status:  "ok",
				Payload: map[string]string{
					"score":    fmt.Sprintf("%d", verdict.Score),
					"approved": fmt.Sprintf("%v", verdict.Approved),
					"retry":    fmt.Sprintf("%v", verdict.ShouldRetry),
					"reason":   utils.Truncate(verdict.Reason, 200),
				},
			}
			if jerr != nil {
				judgeEvent.Status = "degraded"
				judgeEvent.Error = utils.Truncate(jerr.Error(), 300)
			}
			r.emit(judgeEvent)
		}
		if jerr != nil {
			// Judge availability remains fail-open, but the degraded decision is
			// explicit in the event stream instead of looking like an approval.
		} else if verdict != nil && !(verdict.Approved && !verdict.ShouldRetry) {
			r.turn.judge.RetryInjects++
			messages = append(messages, llm.UserMessage(verdict.FormatFeedback()))
			if verdict.Score <= 3 {
				messages = append(messages, llm.UserMessage(
					"<judge-critical>Your previous attempt scored very low. Carefully re-read the feedback and make substantial corrections before finishing.</judge-critical>"))
			}
			out.Completed = false
			out.Error = nil
			return messages, true, TurnOutcome{}
		}
	}

	out.Rounds = r.turn.rounds
	out.ToolFailures = r.turn.failures
	out.Messages = messages

	// End-of-turn summary (matches original DecisionTurn).
	r.emit(event.Event{
		Type:    event.TurnComplete,
		TraceID: traceID,
		Payload: map[string]string{
			"summary": fmt.Sprintf("rounds=%d failures=%d", r.turn.rounds, r.turn.failures),
		},
		Usage: &r.turn.usage,
	})
	return messages, false, *out
}

type toolBatchResult struct {
	messages       []llm.Message
	turnToolCount  int
	turnFailCount  int
	manualCompress bool
}

func (r *Runner) executeToolBatch(
	ctx context.Context,
	messages []llm.Message,
	toolCalls []llm.ToolCall,
	traceID string,
	out *TurnOutcome,
) toolBatchResult {
	var turnToolCount, turnFailCount int
	var manualCompress bool
	var pendingNudges []string
	hooks := defaultToolInterceptors(r.loader())
	// Planning authorization is snapshotted for the entire assistant tool-call
	// batch. A plan created in this batch only authorizes side effects starting
	// with the next model round.
	//
	// The hard guard deliberately does NOT depend on planGate being wired:
	// planGate only supplies nudge copy, while blocking is a safety property
	// that must hold even when the optional nudge component is missing.
	planEstablishedAtBatchStart := r.turn.planning.PlanEstablished
	enforcePlanning := r.profile.Role == "lead" &&
		needsPlan(r.turn.originalTask, planEstablishedAtBatchStart)

	for _, tc := range toolCalls {
		if tc.Arguments != "" && !strings.HasPrefix(tc.Arguments, "{") {
			result := tool.InvalidArgs(fmt.Sprintf("tool call '%s' has truncated arguments", tc.Name))
			messages = append(messages, llm.ToolMessage(result.ToToolMessage(), tc.ID))
			out.ToolResults = append(out.ToolResults, ToolResultRecord{Name: tc.Name, Args: tc.Arguments, Status: result.Status, Output: result.Output})
			r.turn.failures++
			turnFailCount++
			turnToolCount++
			continue
		}

		key := tc.Name + "\x00" + tc.Arguments
		r.turn.tools.bump(key)
		env := &toolCallEnv{role: r.profile.Role, turn: &r.turn, key: key}

		toolStart := time.Now()
		toolPayload := toolEventPayload(tc)
		r.emit(event.Event{
			Type:       event.ToolStarted,
			TraceID:    traceID,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Payload:    toolPayload,
		})

		// Before hooks: planning guard runs first, then deferred nudges are
		// collected and the first non-continue decision wins.
		var decision beforeDecision
		var result tool.Result
		if enforcePlanning {
			definition, known := r.executor.Definition(tc.Name)
			if classification, blocked := unplannedToolBlock(tc.Name, definition, known, tc.Arguments); blocked {
				reason := fmt.Sprintf(
					"tool %q blocked: this non-trivial run has no established plan (%s); "+
						"successfully call TodoWrite or task_create, then retry this tool in the next model round",
					tc.Name, classification,
				)
				decision = beforeDenyEarly
				result = tool.Denied(reason)
				r.emit(event.Event{
					Type:       event.PlanningDecision,
					TraceID:    traceID,
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
					Status:     "blocked",
					Error:      reason,
					Output:     fmt.Sprintf("round=%d classification=%s", r.turn.rounds, classification),
					Payload: map[string]string{
						"action":         "block_unplanned_side_effect",
						"classification": classification,
						"round":          fmt.Sprintf("%d", r.turn.rounds),
					},
				})
			}
		}
		if decision == beforeContinue {
			for _, h := range hooks {
				br := h.Before(env, tc)
				pendingNudges = append(pendingNudges, br.nudges...)
				if br.decision != beforeContinue {
					decision = br.decision
					result = br.result
					break
				}
			}
		}

		switch decision {
		case beforeDenyEarly:
			out.ToolResults = append(out.ToolResults, ToolResultRecord{
				Name: tc.Name, Args: tc.Arguments, Status: result.Status, Output: result.Output,
			})
			messages = append(messages, llm.ToolMessage(result.ToToolMessage(), tc.ID))
			// Planning-gate denials are not execution failures: the tool never
			// ran. Counting them as failures would inflate lesson triggers and
			// reflection/stuck heuristics. The model just needs to establish a
			// plan first; that is expected flow, not a lesson to learn.
			turnToolCount++
			r.emit(event.Event{
				Type: event.ToolFinished, TraceID: traceID,
				ToolCallID: tc.ID, ToolName: tc.Name,
				Duration: time.Since(toolStart), Status: string(result.Status), Output: result.Output,
				Payload: toolPayload,
			})
			continue
		case beforeOverride:
			// synthetic failure; fall through to after-hooks
		default:
			if r.snapshot != nil && r.snapshot.ShouldWrap(tc.Name) {
				result = r.snapshot.WithSnapshot(tc.Name, func() tool.Result {
					return r.executor.Execute(ctx, r.scope, tc)
				})
			} else {
				result = r.executor.Execute(ctx, r.scope, tc)
			}
		}

		out.ToolResults = append(out.ToolResults, ToolResultRecord{
			Name:   tc.Name,
			Args:   tc.Arguments,
			Status: result.Status,
			Output: result.Output,
		})
		messages = append(messages, llm.ToolMessage(result.ToToolMessage(), tc.ID))

		r.emit(event.Event{
			Type:       event.ToolFinished,
			TraceID:    traceID,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Duration:   time.Since(toolStart),
			Status:     string(result.Status),
			Output:     result.Output,
			Payload:    toolPayload,
		})

		if !result.Succeeded() {
			r.turn.failures++
			turnFailCount++
		}
		for _, h := range hooks {
			ar := h.After(env, tc, result)
			pendingNudges = append(pendingNudges, ar.nudges...)
			if ar.manualCompress {
				manualCompress = true
			}
		}
		turnToolCount++
	}

	// Inject deferred nudges only after the full tool_call → tool_result
	// block is closed, preserving OpenAI/DeepSeek message ordering.
	for _, nudge := range pendingNudges {
		messages = append(messages, llm.UserMessage(nudge))
	}

	return toolBatchResult{
		messages:       messages,
		turnToolCount:  turnToolCount,
		turnFailCount:  turnFailCount,
		manualCompress: manualCompress,
	}
}

// afterTools handles post-batch bookkeeping. Non-nil early means return now.
func (r *Runner) afterTools(
	ctx context.Context,
	messages []llm.Message,
	traceID string,
	batch toolBatchResult,
	out *TurnOutcome,
) (msgs []llm.Message, early *TurnOutcome) {
	r.turn.rounds++

	// Manual compress requested by LLM via the `compress` tool
	// (matching original).
	if batch.manualCompress && r.compress != nil {
		messages = r.compress.AutoCompact(ctx, messages, r.profile.SystemPrompt)
		r.turn.tokens.invalidateCache()
	}

	// Lesson stage budget: stop cleanly if lesson collection keeps calling
	// tools beyond its bounded extra rounds.
	if r.turn.lesson.Written {
		r.turn.lesson.RoundsRemaining--
		if r.turn.lesson.RoundsRemaining <= 0 {
			out.Rounds = r.turn.rounds
			out.ToolFailures = r.turn.failures
			out.StoppedReason = "lesson_budget"
			out.Messages = messages
			r.emit(event.Event{
				Type:    event.TurnComplete,
				TraceID: traceID,
				Payload: map[string]string{"summary": fmt.Sprintf("rounds=%d", r.turn.rounds), "note": "lesson budget exhausted"},
				Usage:   &r.turn.usage,
			})
			return messages, out
		}
	}

	// Per-turn summary (matches original finalizeTurn).
	if batch.turnToolCount > 0 {
		r.emit(event.Event{
			Type:    event.TurnDecision,
			TraceID: traceID,
			Payload: map[string]string{
				"round":      fmt.Sprintf("%d", r.turn.rounds),
				"tool_calls": fmt.Sprintf("%d", batch.turnToolCount),
				"failures":   fmt.Sprintf("%d", batch.turnFailCount),
			},
		})
	}
	return messages, nil
}
