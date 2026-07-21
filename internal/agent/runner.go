package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent-refactor/internal/config"
	"go-code-agent-refactor/internal/event"
	"go-code-agent-refactor/internal/llm"
	"go-code-agent-refactor/internal/model"
	"go-code-agent-refactor/internal/tool"
	"go-code-agent-refactor/internal/utils"
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
		if r.rounds >= maxRounds {
			if r.eventSink != nil {
				r.eventSink.Emit(event.Event{
					Type:      event.TurnComplete,
					TraceID:   traceID,
					SessionID: r.scope.SessionID,
					AgentID:   r.scope.AgentID,
					Payload:   map[string]string{"note": "max_rounds", "rounds": fmt.Sprintf("%d", r.rounds)},
					Usage:     &r.turnUsage,
				})
			}
			// Final wrap-up: give the model one last chance to respond
			// without tools, matching original finalizeMaxRounds.
			messages = append(messages, llm.UserMessage(
				"<limit>Maximum tool rounds reached. Wrap up and respond now in plain text.</limit>"))
			sr, err := r.gateway.Stream(ctx, r.profile.Role, llm.CallParams{
				Model:    modelID,
				Messages: messages,
				Tools:    nil,
			}, newPrefixedSink(r.profile.Role))
			r.rounds++
			if err != nil || sr == nil {
				messages = append(messages, llm.AssistantMessage(
					"[interrupted] Hit max-rounds and the wrap-up call failed; please retry."))
				out.Error = fmt.Errorf("max rounds (%d) wrap-up failed", maxRounds)
				out.StoppedReason = "max_rounds"
			} else {
				messages = append(messages, sr.ToAssistantMessage())
				out.Completed = true
				out.StoppedReason = "max_rounds"
			}
			out.Rounds = r.rounds
			out.ToolFailures = r.failures
			out.Messages = messages
			return out
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

		// --- MicroCompact every 6 rounds (matching original) ---
		const microCompactInterval = 6
		if r.rounds > 0 && r.rounds%microCompactInterval == 0 {
			if cleared := MicroCompact(messages); cleared > 0 {
				if r.eventSink != nil {
					r.eventSink.Emit(event.Event{
						Type:      event.ContextDecision,
						TraceID:   traceID,
						SessionID: r.scope.SessionID,
						AgentID:   r.scope.AgentID,
						Payload: map[string]string{
							"action":  "micro_compact",
							"cleared": fmt.Sprintf("%d", cleared),
							"rounds":  fmt.Sprintf("%d", r.rounds),
						},
					})
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
		if r.profile.Role == "explore" {
			estimatedPrompt := int64(llm.EstimateRequestTokens(messages, toolDefs))
			if r.promptTokensUsed > 0 && r.promptTokensUsed+estimatedPrompt > config.SubagentPromptTokenBudget {
				out.Rounds = r.rounds
				out.ToolFailures = r.failures
				out.StoppedReason = "prompt_budget"
				out.Messages = messages
				return out
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
			switch tc.Name {
			case "read_file", "list_dir":
				filePath := extractFilePath(tc.Arguments)
				if filePath != "" {
					r.readCounts[filePath]++
					if r.readCounts[filePath] == 3 {
						messages = append(messages, llm.UserMessage(
							"<convergence-nudge>You have read/list-dir '"+filePath+
								"' 3 times. STOP re-reading it. "+
								"Either you have enough information already, or "+
								"you need a different approach (grep/search_content for specifics, "+
								"or delegate to explore).</convergence-nudge>"))
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
