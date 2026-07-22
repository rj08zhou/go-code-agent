package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/utils"
	"strings"
	"time"
)

// Profile defines an agent's role, capabilities, and loop behavior.
type Profile struct {
	Role         string
	Name         string
	SystemPrompt string
	MaxRounds    int
	MaxTokens    int
	CanRead      bool
	CanWrite     bool
	CanExecute   bool
	CanNetwork   bool
	CanTeam      bool
	CanMemory    bool
}

// Runner is the unified agent execution engine with integrated
// compression, reflection, judge, and snapshot support.
type Runner struct {
	profile  Profile
	gateway  *model.Gateway
	executor *tool.Executor
	scope    *tool.ToolScope

	// Integrated modules
	compress      *Compression
	reflection    *Reflection
	judge         *Judge
	snapshot      *SnapshotManager
	subagent      *SubagentRunner
	planGate      *PlanGate
	lessonWriter  LessonWriter
	lessonWritten bool
	memoryRecall  func(string) string
	todoState     func() (bool, string)
	taskProgress  func() string

	eventSink event.Sink

	// State tracked per-run
	rounds                int
	failures              int
	consecutiveFails      int
	lastFailedTool        string
	roundsSinceComplete   int
	roundsWithoutTodo     int
	hasOpenItems          bool
	lastTriggered         map[string]int
	usedThink             bool
	usedExplore           bool
	usedPlanning          bool
	originalTask          string
	lessonRoundsRemaining int
	lessonPromptInjected  bool
	judgeRetryInjects     int
	turnUsage             llm.Usage
	promptTokensUsed      int64
	toolCallCounts        map[string]int
	exploreDelegations    int
	cachedTokens          int
	cachedTokensAt        int
	readCounts            map[string]int
	budgetWarnInjected    bool
}

func NewRunner(
	profile Profile,
	gateway *model.Gateway,
	executor *tool.Executor,
	scope *tool.ToolScope,
) *Runner {
	if scope == nil {
		scope = &tool.ToolScope{Role: profile.Role}
	}
	scope.CanRead = profile.CanRead
	scope.CanWrite = profile.CanWrite
	scope.CanExecute = profile.CanExecute
	scope.CanNetwork = profile.CanNetwork
	scope.CanTeam = profile.CanTeam
	scope.CanMemory = profile.CanMemory

	return &Runner{
		profile:       profile,
		gateway:       gateway,
		executor:      executor,
		scope:         scope,
		lessonWriter:  nopLessonWriter{},
		lastTriggered: make(map[string]int),
	}
}

// LessonWriter persists auto-lessons from failed tool calls.
type LessonWriter interface {
	RecordFailure(ctx context.Context, messages []llm.Message)
	HasLesson(issue string) bool
}

type nopLessonWriter struct{}

func (nopLessonWriter) RecordFailure(context.Context, []llm.Message) {}
func (nopLessonWriter) HasLesson(string) bool                        { return false }

// SetLessonWriter sets the lesson persistence backend.
func (r *Runner) SetLessonWriter(w LessonWriter) {
	if w != nil {
		r.lessonWriter = w
	}
}

func (r *Runner) Role() string                 { return r.profile.Role }
func (r *Runner) Rounds() int                  { return r.rounds }
func (r *Runner) Failures() int                { return r.failures }
func (r *Runner) SetEventSink(sink event.Sink) { r.eventSink = sink }

// SetCompression wires the auto-compaction module.
func (r *Runner) SetCompression(c *Compression) { r.compress = c }

// ManualCompact explicitly compacts a conversation using the same pipeline as automatic compaction.
func (r *Runner) ManualCompact(ctx context.Context, messages []llm.Message) []llm.Message {
	if r.compress == nil {
		return messages
	}
	return r.compress.AutoCompact(ctx, messages, r.profile.SystemPrompt)
}

// SystemPrompt returns the configured system prompt.
func (r *Runner) SystemPrompt() string { return r.profile.SystemPrompt }

// SetReflection wires the reflection triggers.
func (r *Runner) SetReflection(rf *Reflection) { r.reflection = rf }

// SetJudge wires the LLM-as-Judge verifier.
func (r *Runner) SetJudge(j *Judge) { r.judge = j }

// SetSnapshot wires the git snapshot manager.
func (r *Runner) SetSnapshot(sm *SnapshotManager) { r.snapshot = sm }

// SetSubagentRunner wires the subagent executor.
func (r *Runner) SetSubagentRunner(sr *SubagentRunner) { r.subagent = sr }
func (r *Runner) SetPlanGate(pg *PlanGate)             { r.planGate = pg }

// SetMemoryRecall wires dynamic relevant-memory retrieval for each user turn.
func (r *Runner) SetMemoryRecall(fn func(string) string) { r.memoryRecall = fn }

// SetTodoState supplies the current checklist state so TodoWrite nagging
// continues across REPL turns instead of resetting at every Run call.
func (r *Runner) SetTodoState(fn func() (bool, string)) { r.todoState = fn }

// SetTaskProgress supplies the latest persistent DAG summary for each loop.
func (r *Runner) SetTaskProgress(fn func() string) { r.taskProgress = fn }

// TurnOutcome reports the result of one agent loop run.
type TurnOutcome struct {
	Rounds        int
	ToolFailures  int
	Completed     bool
	Error         error
	StoppedReason string
	Messages      []llm.Message
	ToolResults   []ToolResultRecord
}

// ToolResultRecord captures tool execution details for the TurnOutcome.
type ToolResultRecord struct {
	Name   string
	Args   string
	Status tool.Status
	Output string
}

// Run drives the agent loop, integrating all modules.
func (r *Runner) Run(ctx context.Context, thread []llm.Message, traceID string) TurnOutcome {
	// Runner instances are reused across REPL turns; loop counters and gates
	// are per turn, so reset them before starting a new execution.
	r.rounds = 0
	r.failures = 0
	r.consecutiveFails = 0
	r.lastFailedTool = ""
	r.roundsSinceComplete = 0
	r.roundsWithoutTodo = 0
	r.hasOpenItems = false
	r.lastTriggered = make(map[string]int)
	r.usedThink = false
	r.usedExplore = false
	r.usedPlanning = false
	r.lessonWritten = false
	r.lessonRoundsRemaining = 0
	r.lessonPromptInjected = false
	r.judgeRetryInjects = 0
	r.turnUsage = llm.Usage{}
	r.promptTokensUsed = 0
	r.toolCallCounts = make(map[string]int)
	r.exploreDelegations = 0
	r.cachedTokens = 0
	r.cachedTokensAt = 0
	r.readCounts = make(map[string]int)
	r.budgetWarnInjected = false
	if r.todoState != nil {
		r.hasOpenItems, _ = r.todoState()
	}

	ctx = model.WithTraceID(ctx, traceID)
	messages := append([]llm.Message{}, thread...)

	// Capture original task for plan gate and inject relevant memory once per turn.
	// Use UserMessage (not SystemMessage) so the OpenAI-compatible system block
	// stays stable across turns and prompt caching can hit.
	r.originalTask = lastUserMessage(messages)
	if r.memoryRecall != nil && r.originalTask != "" {
		if recalled := strings.TrimSpace(r.memoryRecall(r.originalTask)); recalled != "" && recalled != "No memories found." {
			messages = append(messages, llm.UserMessage("Relevant memory:\n"+recalled))
		}
	}

	out := TurnOutcome{}
	maxRounds := r.profile.MaxRounds
	if maxRounds <= 0 {
		maxRounds = config.MaxRounds
	}
	cfg := config.CurrentConfig()
	modelID := "default"
	ctxWindowTokens := config.DefaultContextTokens
	if cfg != nil {
		modelID = cfg.ModelID
		ctxWindowTokens = cfg.ContextWindowTokens(cfg.ModelID)
	}

	if r.eventSink != nil {
		r.eventSink.Emit(event.Event{
			Type:      event.AgentStarted,
			TraceID:   traceID,
			SessionID: r.scope.SessionID,
			AgentID:   r.scope.AgentID,
		})
	}

	for {
		if err := ctx.Err(); err != nil {
			out.Error = err
			out.Messages = messages
			return out
		}

		// Soft deadline (master pattern): explore/web_fetch subagents stop a
		// buffer before the hard ctx deadline and turn in a no-tools summary
		// instead of being cancelled mid-tool.
		if r.profile.Role == "explore" {
			if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl.Add(-config.SubagentSoftDeadlineBuffer)) {
				return r.finalizeWithoutTools(ctx, messages, modelID, traceID, &out,
					"soft_deadline",
					"<limit>Time budget almost exhausted. Do NOT call any tools. "+
						"Synthesize a concise summary from what you have already observed. "+
						"Mark anything unverified explicitly.</limit>")
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
			r.cachedTokens = 0
			if r.eventSink != nil {
				r.eventSink.Emit(event.Event{
					Type:      event.ContextDecision,
					TraceID:   traceID,
					SessionID: r.scope.SessionID,
					AgentID:   r.scope.AgentID,
					Payload: map[string]string{
						"action":  "drop_nudges",
						"removed": fmt.Sprintf("%d", removed),
						"rounds":  fmt.Sprintf("%d", r.rounds),
					},
				})
			}
		}

		if r.rounds >= maxRounds {
			return r.finalizeWithoutTools(ctx, messages, modelID, traceID, &out,
				"max_rounds",
				"<limit>Maximum tool rounds reached. Wrap up and respond now in plain text.</limit>")
		}

		// --- Auto-compaction check ---
		toolDefs := r.executor.ToolDefs()
		if r.compress != nil && r.rounds > 0 && r.rounds%config.TokenCheckInterval == 0 {
			// Refresh cached token estimate every TokenCheckInterval rounds
			// (matching original preRound pattern).
			if r.rounds-r.cachedTokensAt >= config.TokenCheckInterval || r.cachedTokens == 0 {
				r.cachedTokens = llm.EstimateRequestTokens(messages, toolDefs)
				r.cachedTokensAt = r.rounds
			}
			shouldCompact := NeedsCompaction(messages, toolDefs, ctxWindowTokens)
			if r.profile.Role == "explore" && r.cachedTokens > config.SubagentCompactionThreshold {
				shouldCompact = true
			}
			if shouldCompact {
				if r.profile.Role == "explore" {
					r.promptTokensUsed += int64(r.cachedTokens)
				}
				messages = r.compress.AutoCompact(ctx, messages, r.profile.SystemPrompt)
				// Invalidate cache after compaction since the message slice
				// was rebuilt.
				r.cachedTokens = 0
				if r.eventSink != nil {
					r.eventSink.Emit(event.Event{
						Type:      event.ContextDecision,
						TraceID:   traceID,
						SessionID: r.scope.SessionID,
						AgentID:   r.scope.AgentID,
						Payload: map[string]string{
							"action": "auto_compact",
							"rounds": fmt.Sprintf("%d", r.rounds),
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
		if r.rounds > 0 && r.rounds%microCompactInterval == 0 {
			// Refresh the token estimate if the auto-compaction block above
			// didn't already do it this round (e.g. when compression is off).
			if r.cachedTokens == 0 || r.rounds-r.cachedTokensAt >= config.TokenCheckInterval {
				r.cachedTokens = llm.EstimateRequestTokens(messages, toolDefs)
				r.cachedTokensAt = r.rounds
			}
			microThreshold := int(float64(ctxWindowTokens) * config.MicroCompactThresholdFrac)
			if r.cachedTokens > microThreshold {
				cleared, reclaimed := MicroCompact(messages, config.MicroCompactMinClearBytes)
				if cleared > 0 {
					// Reclaimed bytes changed the message list; force a
					// re-estimate on the next token check.
					r.cachedTokens = 0
					if r.eventSink != nil {
						r.eventSink.Emit(event.Event{
							Type:      event.ContextDecision,
							TraceID:   traceID,
							SessionID: r.scope.SessionID,
							AgentID:   r.scope.AgentID,
							Payload: map[string]string{
								"action":    "micro_compact",
								"cleared":   fmt.Sprintf("%d", cleared),
								"reclaimed": fmt.Sprintf("%d", reclaimed),
								"rounds":    fmt.Sprintf("%d", r.rounds),
							},
						})
					}
				}
			}
		}

		// --- Planning gate (round 0 & 1 only) ---
		if r.planGate != nil && r.rounds <= 1 {
			if planMsg := r.planGate.Eval(
				r.rounds, r.usedPlanning, r.usedThink, r.usedExplore,
				r.originalTask,
			); planMsg != "" {
				if r.eventSink != nil {
					r.eventSink.Emit(event.Event{
						Type:      event.PlanningDecision,
						TraceID:   traceID,
						SessionID: r.scope.SessionID,
						AgentID:   r.scope.AgentID,
					})
				}
				messages = append(messages, llm.UserMessage(planMsg))
			}
		}

		// Show pending tasks ONCE at round 0 as a user note (not system),
		// preserving system block stability for prompt caching.
		if r.rounds == 0 && r.taskProgress != nil {
			if progress := strings.TrimSpace(r.taskProgress()); progress != "" {
				messages = append(messages, llm.UserMessage(
					"Note: the following tasks from a previous session are still open. "+
						"Only resume them if the user explicitly asks. "+
						"If the user says nothing about them, ignore them:\n"+progress))
			}
		}

		// --- Reflection check ---
		if r.reflection != nil {
			progressSummary := ""
			taskCount := 0
			if r.hasOpenItems || r.originalTask != "" {
				taskCount = 1
			}
			stuckThresh := config.StuckThreshold
			if r.profile.Role == "explore" {
				stuckThresh = config.ExploreStuckThreshold
			}
			reflPrompts, resetF, resetNag, resetStuck, triggered := r.reflection.Eval(
				r.consecutiveFails, r.lastFailedTool,
				config.MaxConsecutiveFailures,
				r.rounds, r.failures,
				r.roundsSinceComplete, r.roundsWithoutTodo,
				stuckThresh, config.ReflectInterval,
				r.hasOpenItems, r.lastTriggered,
				taskCount, progressSummary,
			)
			if resetF {
				r.consecutiveFails = 0
			}
			if resetNag {
				r.roundsWithoutTodo = 0
			}
			if resetStuck {
				r.roundsSinceComplete = 0
			}
			for _, k := range triggered {
				r.lastTriggered[k] = r.rounds
				if r.eventSink != nil {
					r.eventSink.Emit(event.Event{
						Type:      event.ReflectionTriggered,
						TraceID:   traceID,
						SessionID: r.scope.SessionID,
						AgentID:   r.scope.AgentID,
						Payload: map[string]string{
							"kind":                  k,
							"consecutive_fails":     fmt.Sprintf("%d", r.consecutiveFails),
							"rounds_since_complete": fmt.Sprintf("%d", r.roundsSinceComplete),
							"prompt_count":          fmt.Sprintf("%d", len(reflPrompts)),
						},
					})
				}
			}
			for _, p := range reflPrompts {
				messages = append(messages, llm.UserMessage(p))
			}
		}

		// --- LLM call ---
		// Repair any broken tool_call / tool_result pairing before the
		// request leaves the process. OpenAI-compatible providers (DeepSeek
		// included) hard-fail with 400 when a non-tool message interrupts
		// the response block.
		if repaired, n := ensureToolCallPairing(messages); n > 0 {
			messages = repaired
			if r.eventSink != nil {
				r.eventSink.Emit(event.Event{
					Type:      event.ContextDecision,
					TraceID:   traceID,
					SessionID: r.scope.SessionID,
					AgentID:   r.scope.AgentID,
					Payload: map[string]string{
						"action": "repair_tool_pairing",
						"filled": fmt.Sprintf("%d", n),
						"rounds": fmt.Sprintf("%d", r.rounds),
					},
				})
			}
		}
		if r.profile.Role == "explore" {
			estimatedPrompt := int64(llm.EstimateRequestTokens(messages, toolDefs))
			if r.promptTokensUsed > 0 {
				frac := float64(r.promptTokensUsed) / float64(config.SubagentPromptTokenBudget)
				if !r.budgetWarnInjected && frac >= config.ExploreBudgetWarnFrac {
					messages = append(messages, llm.UserMessage(
						"<budget-warn>You have used most of your prompt budget. "+
							"Prefer synthesizing a summary now. At most one more targeted "+
							"read/search if a critical fact is still missing — then stop.</budget-warn>"))
					r.budgetWarnInjected = true
				}
			}
			if r.promptTokensUsed > 0 && r.promptTokensUsed+estimatedPrompt > config.SubagentPromptTokenBudget {
				// Turn in a summary instead of tearing the investigation
				// (master soft-deadline spirit applied to prompt budget).
				return r.finalizeWithoutTools(ctx, messages, modelID, traceID, &out,
					"prompt_budget",
					"<limit>Prompt budget exhausted. Do NOT call any tools. "+
						"Synthesize a concise, well-structured summary from the files and "+
						"search results already in this conversation. Mark anything not "+
						"verified explicitly. Partial answers beat an incomplete stub.</limit>")
			}
			r.promptTokensUsed += estimatedPrompt
		}
		started := time.Now()
		sr, err := r.gateway.Stream(ctx, r.profile.Role, llm.CallParams{
			Model:     modelID,
			Messages:  messages,
			Tools:     toolDefs,
			MaxTokens: r.profile.MaxTokens,
		}, newPrefixedSink(r.profile.Role))
		if err != nil {
			out.Error = fmt.Errorf("API call failed: %w", err)
			out.Messages = messages
			return out
		}
		// Accumulate per-round usage for turn-level summary.
		if !sr.Usage.IsZero() {
			r.turnUsage.PromptTokens += sr.Usage.PromptTokens
			r.turnUsage.CompletionTokens += sr.Usage.CompletionTokens
			r.turnUsage.TotalTokens += sr.Usage.TotalTokens
			r.turnUsage.CachedReadTokens += sr.Usage.CachedReadTokens
			r.turnUsage.CacheMissTokens += sr.Usage.CacheMissTokens
			r.turnUsage.CacheCreateTokens += sr.Usage.CacheCreateTokens
		}
		if r.eventSink != nil {
			r.eventSink.Emit(event.Event{
				Type:      event.ModelCalled,
				TraceID:   traceID,
				SessionID: r.scope.SessionID,
				AgentID:   r.scope.AgentID,
				Duration:  time.Since(started),
				Usage:     &sr.Usage,
			})
		}

		messages = append(messages, sr.ToAssistantMessage())

		// Truncation detection: injected early so incomplete tool calls
		// are never executed (matching original). If the LLM hit its
		// output limit, ask it to continue from where it left off.
		//
		// OpenAI/DeepSeek require every tool_call_id to have a following
		// tool message. If the truncated assistant message already lists
		// tool_calls, synthesize error tool results before the user nudge
		// so the next request stays protocol-valid.
		if sr.FinishReason == "length" {
			if r.eventSink != nil {
				r.eventSink.Emit(event.Event{
					Type:      event.ReflectionTriggered,
					TraceID:   traceID,
					SessionID: r.scope.SessionID,
					AgentID:   r.scope.AgentID,
					Payload:   map[string]string{"kind": "truncated"},
				})
			}
			for _, tc := range sr.ToolCalls {
				result := tool.Failed(fmt.Sprintf(
					"tool call '%s' was truncated before execution; please re-issue if still needed", tc.Name))
				messages = append(messages, llm.ToolMessage(result.ToToolMessage(), tc.ID))
			}
			messages = append(messages, llm.UserMessage(
				"<system>Your previous response was truncated due to output length limits. "+
					"Some tool calls may have been lost. Please continue from where you left off. "+
					"Do NOT repeat tool calls that already succeeded above.</system>"))
			continue
		}

		// No tool calls → either we're done, or we inject an auto-lesson
		// prompt and loop one more time (matching original).
		if len(sr.ToolCalls) == 0 {
			out.Completed = true

			// Auto-Lesson: after enough rounds, inject a prompt asking
			// the model to record lessons, then continue the loop.
			// Only for agents with memory capability (lead agent).
			// Subagents (explore/teammate) have CanMemory=false and
			// would fail trying to call memory_write.
			if r.profile.CanMemory && r.rounds >= config.LessonThreshold && !r.lessonWritten && r.lessonWriter != nil {
				r.lessonWritten = true
				r.lessonRoundsRemaining = config.LessonRoundsLimit
				if r.eventSink != nil {
					r.eventSink.Emit(event.Event{
						Type:      event.MemoryDecision,
						TraceID:   traceID,
						SessionID: r.scope.SessionID,
						AgentID:   r.scope.AgentID,
						Payload:   map[string]string{"rounds": fmt.Sprintf("%d", r.rounds)},
					})
				}
				r.lessonWriter.RecordFailure(ctx, messages)
				if !r.lessonPromptInjected {
					r.lessonPromptInjected = true
					messages = append(messages, llm.UserMessage(
						"<auto-lesson>Record any lessons, preferences, or patterns learned in this session to long-term memory using memory_write.</auto-lesson>"))
				}
				continue
			}

			if r.judge != nil && r.judge.IsEnabled() && r.judgeRetryInjects < config.JudgeMaxRetryInjects {
				taskText := lastUserMessage(messages)
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
				if r.eventSink != nil && verdict != nil {
					r.eventSink.Emit(event.Event{
						Type:      event.JudgeDecision,
						TraceID:   traceID,
						SessionID: r.scope.SessionID,
						AgentID:   r.scope.AgentID,
						Payload: map[string]string{
							"score":    fmt.Sprintf("%d", verdict.Score),
							"approved": fmt.Sprintf("%v", verdict.Approved),
							"retry":    fmt.Sprintf("%v", verdict.ShouldRetry),
							"reason":   utils.Truncate(verdict.Reason, 200),
						},
					})
				}
				if jerr != nil {
					// Soft-fail open on judge errors (matches original).
				} else if verdict != nil && !(verdict.Approved && !verdict.ShouldRetry) {
					r.judgeRetryInjects++
					messages = append(messages, llm.UserMessage(verdict.FormatFeedback()))
					if verdict.Score <= 3 {
						messages = append(messages, llm.UserMessage(
							"<judge-critical>Your previous attempt scored very low. Carefully re-read the feedback and make substantial corrections before finishing.</judge-critical>"))
					}
					out.Completed = false
					out.Error = nil
					continue // extra round for correction
				}
			}

			out.Rounds = r.rounds
			out.ToolFailures = r.failures
			out.Messages = messages

			// End-of-turn summary (matches original DecisionTurn).
			if r.eventSink != nil {
				r.eventSink.Emit(event.Event{
					Type:      event.TurnComplete,
					TraceID:   traceID,
					SessionID: r.scope.SessionID,
					AgentID:   r.scope.AgentID,
					Payload: map[string]string{
						"summary": fmt.Sprintf("rounds=%d failures=%d", r.rounds, r.failures),
					},
					Usage: &r.turnUsage,
				})
			}
			return out
		}

		// --- Execute tools with snapshot ---
		var turnToolCount, turnFailCount int
		var manualCompress bool
		var pendingNudges []string
		results := make([]tool.Result, 0, len(sr.ToolCalls))
		for _, tc := range sr.ToolCalls {
			if tc.Arguments != "" && !strings.HasPrefix(tc.Arguments, "{") {
				result := tool.InvalidArgs(fmt.Sprintf("tool call '%s' has truncated arguments", tc.Name))
				messages = append(messages, llm.ToolMessage(result.ToToolMessage(), tc.ID))
				out.ToolResults = append(out.ToolResults, ToolResultRecord{Name: tc.Name, Args: tc.Arguments, Status: result.Status, Output: result.Output})
				r.failures++
				turnFailCount++
				turnToolCount++
				continue
			}

			key := tc.Name + "\x00" + tc.Arguments
			r.toolCallCounts[key]++
			// Track per-file reads to detect convergence failures.
			// Defer the nudge until AFTER every tool result for this turn is
			// appended: OpenAI/DeepSeek reject any non-tool message between an
			// assistant(tool_calls) and its tool responses.
			switch tc.Name {
			case "read_file", "list_dir":
				filePath := extractFilePath(tc.Arguments)
				if filePath != "" {
					r.readCounts[filePath]++
					// Explore: nudge on the 2nd hit and hard-fail the 3rd so
					// re-reads cannot burn the prompt budget (lead keeps the
					// softer 3rd-hit nudge).
					if r.profile.Role == "explore" {
						if r.readCounts[filePath] == 2 {
							pendingNudges = append(pendingNudges,
								"<convergence-nudge>You already read/list-dir '"+filePath+
									"'. Do NOT re-read it — use the prior result, or "+
									"search_content for a specific fact.</convergence-nudge>")
						}
					} else if r.readCounts[filePath] == 3 {
						pendingNudges = append(pendingNudges,
							"<convergence-nudge>You have read/list-dir '"+filePath+
								"' 3 times. STOP re-reading it. "+
								"Either you have enough information already, or "+
								"you need a different approach (grep/search_content for specifics, "+
								"or delegate to explore).</convergence-nudge>")
					}
				}
			}
			toolStart := time.Now()
			if r.eventSink != nil {
				r.eventSink.Emit(event.Event{
					Type:       event.ToolStarted,
					TraceID:    traceID,
					SessionID:  r.scope.SessionID,
					AgentID:    r.scope.AgentID,
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
				})
			}
			var result tool.Result
			if r.profile.Role == "explore" && (tc.Name == "read_file" || tc.Name == "list_dir") {
				if filePath := extractFilePath(tc.Arguments); filePath != "" && r.readCounts[filePath] >= 3 {
					result = tool.Failed(fmt.Sprintf(
						"repeated %s of %q blocked; use the earlier result or search_content", tc.Name, filePath))
					results = append(results, result)
					out.ToolResults = append(out.ToolResults, ToolResultRecord{
						Name: tc.Name, Args: tc.Arguments, Status: result.Status, Output: result.Output,
					})
					messages = append(messages, llm.ToolMessage(result.ToToolMessage(), tc.ID))
					r.failures++
					turnFailCount++
					turnToolCount++
					if r.eventSink != nil {
						r.eventSink.Emit(event.Event{
							Type: event.ToolFinished, TraceID: traceID, SessionID: r.scope.SessionID,
							AgentID: r.scope.AgentID, ToolCallID: tc.ID, ToolName: tc.Name,
							Duration: time.Since(toolStart), Status: string(result.Status), Output: result.Output,
						})
					}
					continue
				}
			}
			switch {
			case r.toolCallCounts[key] > config.MaxRepeatedToolCalls:
				result = tool.Failed(fmt.Sprintf("repeated tool call blocked: %s. Use a different path, offset, limit, or query.", tc.Name))
			case tc.Name == "explore":
				r.exploreDelegations++
				if r.exploreDelegations > config.MaxExploreDelegations {
					result = tool.Failed("explore delegation budget exhausted for this turn; synthesize the findings already collected")
				} else if r.snapshot != nil && r.snapshot.ShouldWrap(tc.Name) {
					result = r.snapshot.WithSnapshot(tc.Name, func() tool.Result {
						return r.executor.Execute(ctx, r.scope, tc)
					})
				} else {
					result = r.executor.Execute(ctx, r.scope, tc)
				}
			default:
				if r.snapshot != nil && r.snapshot.ShouldWrap(tc.Name) {
					result = r.snapshot.WithSnapshot(tc.Name, func() tool.Result {
						return r.executor.Execute(ctx, r.scope, tc)
					})
				} else {
					result = r.executor.Execute(ctx, r.scope, tc)
				}
			}

			results = append(results, result)
			out.ToolResults = append(out.ToolResults, ToolResultRecord{
				Name:   tc.Name,
				Args:   tc.Arguments,
				Status: result.Status,
				Output: result.Output,
			})
			messages = append(messages, llm.ToolMessage(result.ToToolMessage(), tc.ID))

			if r.eventSink != nil {
				r.eventSink.Emit(event.Event{
					Type:       event.ToolFinished,
					TraceID:    traceID,
					SessionID:  r.scope.SessionID,
					AgentID:    r.scope.AgentID,
					ToolCallID: tc.ID,
					ToolName:   tc.Name,
					Duration:   time.Since(toolStart),
					Status:     string(result.Status),
					Output:     result.Output,
				})
			}

			if !result.Succeeded() {
				r.failures++
				turnFailCount++
				if tc.Name == r.lastFailedTool {
					r.consecutiveFails++
				} else {
					r.consecutiveFails = 1
					r.lastFailedTool = tc.Name
				}
			} else {
				r.consecutiveFails = 0
				r.lastFailedTool = ""
				r.roundsSinceComplete++
			}

			// Track todo interaction
			// --- Track planning/thinking flags ---
			switch tc.Name {
			case "think", "thinking", "reason":
				r.usedThink = true
			case "explore":
				r.usedExplore = true
			case "TodoWrite", "task_create", "task_list", "task_dag", "task_ready":
				r.usedPlanning = true
			case "compress":
				manualCompress = true
			}

			if tc.Name == "TodoWrite" {
				r.roundsWithoutTodo = 0
				r.hasOpenItems = false
				for _, item := range parseArgsItems(tc.Arguments) {
					if item["status"] != "completed" {
						r.hasOpenItems = true
						break
					}
				}
			} else {
				r.roundsWithoutTodo++
			}
			turnToolCount++
		}

		// Inject deferred nudges only after the full tool_call → tool_result
		// block is closed, preserving OpenAI/DeepSeek message ordering.
		for _, nudge := range pendingNudges {
			messages = append(messages, llm.UserMessage(nudge))
		}

		r.rounds++

		// Manual compress requested by LLM via the `compress` tool
		// (matching original).
		if manualCompress && r.compress != nil {
			messages = r.compress.AutoCompact(ctx, messages, r.profile.SystemPrompt)
			r.cachedTokens = 0
		}

		// Lesson stage budget: if the model was given a lesson prompt, limit
		// how many extra rounds it can use before we force a wrap-up.
		if r.lessonWritten {
			r.lessonRoundsRemaining--
			if r.lessonRoundsRemaining <= 0 {
				out.Rounds = r.rounds
				out.ToolFailures = r.failures
				out.Messages = messages
				if r.eventSink != nil {
					r.eventSink.Emit(event.Event{
						Type:      event.TurnComplete,
						TraceID:   traceID,
						SessionID: r.scope.SessionID,
						AgentID:   r.scope.AgentID,
						Payload:   map[string]string{"summary": fmt.Sprintf("rounds=%d", r.rounds), "note": "lesson budget exhausted"},
						Usage:     &r.turnUsage,
					})
				}
				return out
			}
		}

		// Per-turn summary (matches original finalizeTurn).
		if r.eventSink != nil && turnToolCount > 0 {
			r.eventSink.Emit(event.Event{
				Type:      event.TurnDecision,
				TraceID:   traceID,
				SessionID: r.scope.SessionID,
				AgentID:   r.scope.AgentID,
				Payload: map[string]string{
					"round":      fmt.Sprintf("%d", r.rounds),
					"tool_calls": fmt.Sprintf("%d", turnToolCount),
					"failures":   fmt.Sprintf("%d", turnFailCount),
				},
			})
		}
	}
}

// --- Lead / Explore / Teammate profiles ---

func NewLeadProfile(systemPrompt string) Profile {
	return Profile{
		Role:         "lead",
		Name:         "lead",
		SystemPrompt: systemPrompt,
		MaxTokens:    config.DefaultMaxOutputTokens,
		CanRead:      true,
		CanWrite:     true,
		CanExecute:   true,
		CanNetwork:   true,
		CanTeam:      true,
		CanMemory:    true,
	}
}

func NewExploreProfile() Profile {
	return Profile{
		Role:       "explore",
		Name:       "explore",
		MaxRounds:  config.SubagentMaxRounds,
		MaxTokens:  config.DefaultMaxOutputTokens,
		CanRead:    true,
		CanWrite:   false,
		CanExecute: true,
		CanNetwork: false,
		CanTeam:    false,
		CanMemory:  false,
	}
}

func NewTeammateProfile(name string) Profile {
	return Profile{
		Role:       "teammate",
		Name:       name,
		MaxRounds:  config.TeammateWorkMaxRounds,
		MaxTokens:  config.DefaultMaxOutputTokens,
		CanRead:    true,
		CanWrite:   true,
		CanExecute: true,
		CanNetwork: false,
		CanTeam:    true,
		CanMemory:  false,
	}
}

// --- Helpers ---

func lastUserMessage(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		c := strings.TrimSpace(msgs[i].Content)
		if c == "" || strings.HasPrefix(c, "<") {
			continue
		}
		return c
	}
	return ""
}

func parseArgsItems(rawArgs string) []map[string]string {
	if rawArgs == "" {
		return nil
	}
	var a struct {
		Items []map[string]string `json:"items"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &a); err != nil {
		return nil
	}
	return a.Items
}

var _ = utils.Truncate

// ephemeralNudgePrefixes marks one-time procedural instructions injected by
// the reflection / judge / plan-gate / convergence machinery. Once the model
// has produced an assistant turn in response to them they carry no durable
// context, so they are stripped from the running message list to avoid
// re-billing them on every subsequent round.
var ephemeralNudgePrefixes = []string{
	"<mini-reflect>",
	"<strategy-change>",
	"<investigation-stuck>",
	"<stuck>",
	"<reflect>",
	"<task-nag>",
	"<convergence-nudge>",
	"<budget-warn>",
	"<limit>",
	"<judge-critical>",
	"<verification-failed>",
	"<auto-lesson>",
	"<think-first>",
	"<planning-required>",
	"<system>",
}

// finalizeWithoutTools asks the model for one last plain-text response with
// tools disabled (max-rounds / prompt-budget / soft-deadline wrap-up).
// Mirrors master's subagent soft-deadline "turn in what you have" pattern.
func (r *Runner) finalizeWithoutTools(
	ctx context.Context,
	messages []llm.Message,
	modelID, traceID string,
	out *TurnOutcome,
	reason, limitMsg string,
) TurnOutcome {
	messages = append(messages, llm.UserMessage(limitMsg))
	sr, err := r.gateway.Stream(ctx, r.profile.Role, llm.CallParams{
		Model:    modelID,
		Messages: messages,
		Tools:    nil,
	}, newPrefixedSink(r.profile.Role))
	r.rounds++
	if err != nil || sr == nil {
		messages = append(messages, llm.AssistantMessage(
			fmt.Sprintf("[interrupted] Hit %s and the wrap-up call failed; please retry.", reason)))
		out.Error = fmt.Errorf("%s wrap-up failed", reason)
		out.StoppedReason = reason
	} else {
		if !sr.Usage.IsZero() {
			r.turnUsage.PromptTokens += sr.Usage.PromptTokens
			r.turnUsage.CompletionTokens += sr.Usage.CompletionTokens
			r.turnUsage.TotalTokens += sr.Usage.TotalTokens
			r.turnUsage.CachedReadTokens += sr.Usage.CachedReadTokens
			r.turnUsage.CacheMissTokens += sr.Usage.CacheMissTokens
			r.turnUsage.CacheCreateTokens += sr.Usage.CacheCreateTokens
		}
		messages = append(messages, sr.ToAssistantMessage())
		out.Completed = true
		out.StoppedReason = reason
	}
	out.Rounds = r.rounds
	out.ToolFailures = r.failures
	out.Messages = messages
	if r.eventSink != nil {
		r.eventSink.Emit(event.Event{
			Type:      event.TurnComplete,
			TraceID:   traceID,
			SessionID: r.scope.SessionID,
			AgentID:   r.scope.AgentID,
			Payload:   map[string]string{"note": reason, "rounds": fmt.Sprintf("%d", r.rounds)},
			Usage:     &r.turnUsage,
		})
	}
	return *out
}

func isEphemeralNudge(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	c := strings.TrimSpace(m.Content)
	for _, p := range ephemeralNudgePrefixes {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// ensureToolCallPairing walks the message list and, for every assistant
// message that declares tool_calls, makes sure each tool_call_id has a
// matching RoleTool response immediately afterwards (OpenAI/DeepSeek
// protocol). If a non-tool message previously interrupted the response
// block, matching tool results are pulled forward and the interrupter is
// deferred until after the complete tool block. Missing responses are
// filled with synthetic error tool messages. Returns the (possibly rebuilt)
// slice and the number of synthetic tool messages inserted.
func ensureToolCallPairing(msgs []llm.Message) ([]llm.Message, int) {
	if len(msgs) == 0 {
		return msgs, 0
	}
	out := make([]llm.Message, 0, len(msgs)+4)
	filled := 0
	changed := false
	for i := 0; i < len(msgs); {
		m := msgs[i]
		i++
		if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
			out = append(out, m)
			continue
		}
		out = append(out, m)

		needed := make(map[string]struct{}, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				needed[tc.ID] = struct{}{}
			}
		}
		found := make(map[string]llm.Message, len(needed))
		var deferred []llm.Message
		// Pull matching tool results forward even if a nudge interrupted
		// them; stop at the next assistant turn.
		for i < len(msgs) && len(found) < len(needed) {
			cur := msgs[i]
			if cur.Role == llm.RoleAssistant {
				break
			}
			i++
			if cur.Role == llm.RoleTool {
				if _, ok := needed[cur.ToolCallID]; ok {
					if _, seen := found[cur.ToolCallID]; !seen {
						found[cur.ToolCallID] = cur
						continue
					}
				}
			}
			deferred = append(deferred, cur)
			changed = true
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				continue
			}
			if tm, ok := found[tc.ID]; ok {
				out = append(out, tm)
				continue
			}
			name := tc.Name
			if name == "" {
				name = "unknown"
			}
			out = append(out, llm.ToolMessage(
				fmt.Sprintf("Error: missing tool result for '%s' (auto-repaired)", name), tc.ID))
			filled++
			changed = true
		}
		out = append(out, deferred...)
	}
	if !changed {
		return msgs, 0
	}
	return out, filled
}

// dropConsumedNudges removes ephemeral nudge messages that appear before the
// last assistant message (i.e. the model has already seen and responded to
// them). Nudges positioned after the last assistant message are kept so the
// model still sees not-yet-consumed instructions on the upcoming call. Returns
// the filtered slice (a fresh backing array when anything changed) and the
// number of messages removed.
func dropConsumedNudges(msgs []llm.Message) ([]llm.Message, int) {
	lastAsst := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			lastAsst = i
			break
		}
	}
	if lastAsst <= 0 {
		return msgs, 0
	}
	removed := 0
	for i := 0; i < lastAsst; i++ {
		if isEphemeralNudge(msgs[i]) {
			removed++
		}
	}
	if removed == 0 {
		return msgs, 0
	}
	out := make([]llm.Message, 0, len(msgs)-removed)
	for i, m := range msgs {
		if i < lastAsst && isEphemeralNudge(m) {
			continue
		}
		out = append(out, m)
	}
	return out, removed
}

func extractFilePath(rawArgs string) string {
	if rawArgs == "" {
		return ""
	}
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &a); err != nil || a.Path == "" {
		return ""
	}
	return a.Path
}
