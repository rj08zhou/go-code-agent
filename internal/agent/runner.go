package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/gateway"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/tool"
	"strings"
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
	gateway  *gateway.Gateway
	executor *tool.Executor
	scope    *tool.ToolScope

	// Integrated modules (session-lifetime collaborators)
	compress       *Compression
	reflection     *Reflection
	judge          *Judge
	snapshot       *SnapshotManager
	subagent       *SubagentRunner
	planGate       *PlanGate
	promptLoader   *prompt.Loader
	lessonWriter   LessonWriter
	memoryRecall   func(string) string
	todoState      func() (bool, string)
	taskProgress   func() string
	dynamicContext func() string // evergreen / tasks / MCP snapshot per Run
	eventSink      event.Sink
	cfg            *config.Config // process config; nil → safe defaults in Run

	// Ephemeral per-Run state
	turn turnState
}

func reasoningRequestFromConfig(cfg *config.Config) *llm.ReasoningRequest {
	if cfg == nil || !cfg.ReasoningEnabled {
		return nil
	}
	return &llm.ReasoningRequest{
		Enabled: true,
		Effort:  strings.TrimSpace(cfg.ReasoningEffort),
	}
}

func NewRunner(
	profile Profile,
	gateway *gateway.Gateway,
	executor *tool.Executor,
	scope *tool.ToolScope,
	cfg *config.Config,
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
		profile:      profile,
		gateway:      gateway,
		executor:     executor,
		scope:        scope,
		cfg:          cfg,
		lessonWriter: nopLessonWriter{},
		turn:         newTurnState(),
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
func (r *Runner) Rounds() int                  { return r.turn.rounds }
func (r *Runner) Failures() int                { return r.turn.failures }
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

// taskBatchID is the DAG batch this run has landed on, empty until the first
// task_create resolves one.
func (r *Runner) taskBatchID() string {
	if r.scope == nil {
		return ""
	}
	return r.scope.TaskBatch.ID()
}

// SetPromptLoader wires the template loader used for inline prompts
// (response-truncation message, post-explore nudge).
func (r *Runner) SetPromptLoader(pl *prompt.Loader) { r.promptLoader = pl }

// loader returns the injected prompt loader, or a default one if none was set.
// This keeps backward compatibility with code paths that don't wire a loader
// (e.g. integration tests). Same nil-fallback pattern as LLMLessonWriter.
func (r *Runner) loader() *prompt.Loader {
	if r.promptLoader != nil {
		return r.promptLoader
	}
	return prompt.NewLoader()
}

// SetMemoryRecall wires dynamic relevant-memory retrieval for each user turn.
func (r *Runner) SetMemoryRecall(fn func(string) string) { r.memoryRecall = fn }

// SetTodoState supplies the current checklist state so TodoWrite nagging
// continues across REPL turns instead of resetting at every Run call.
func (r *Runner) SetTodoState(fn func() (bool, string)) { r.todoState = fn }

// SetTaskProgress supplies the latest persistent DAG summary for each loop.
func (r *Runner) SetTaskProgress(fn func() string) { r.taskProgress = fn }

// SetDynamicContext supplies a per-Run snapshot (evergreen memory, open tasks,
// MCP catalog) injected as a user <session-context> message so the system
// prompt remains static and cacheable.
func (r *Runner) SetDynamicContext(fn func() string) { r.dynamicContext = fn }

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
// Stage details live in runner_loop.go; this method is the state-machine skeleton.
// The DAG batch is left unresolved: the first task_create picks up whichever
// batch the request is already using, so a multi-turn request keeps one DAG.
func (r *Runner) Run(ctx context.Context, thread []llm.Message, traceID string) (result TurnOutcome) {
	return r.RunWithTaskBatch(ctx, thread, traceID, "")
}

// RunWithTaskBatch pins the run to an explicit DAG batch, for callers whose
// task ownership is known up front (subagents and teammates own theirs). The
// batch ID is deliberately separate from traceID, which is observability only.
func (r *Runner) RunWithTaskBatch(
	ctx context.Context,
	thread []llm.Message,
	traceID, taskBatchID string,
) (result TurnOutcome) {
	defer func() {
		result.Messages = stripInjectedTurnContext(result.Messages)
	}()

	// Runner instances are reused across REPL turns; loop counters and gates
	// are per turn, so reset them before starting a new execution.
	r.resetTurnState()
	if r.scope != nil {
		// A fresh reference per run: the previous run's resolution must not
		// leak into this one, or a finished batch would keep absorbing tasks.
		r.scope.TaskBatch = tool.NewTaskBatch(strings.TrimSpace(taskBatchID))
	}

	ctx = gateway.WithTraceID(ctx, traceID)
	// Capture original task for plan gate and inject dynamic context once per
	// turn as UserMessage so the system block stays stable for prompt caching.
	messages := r.injectTurnContext(append([]llm.Message{}, thread...))

	out := TurnOutcome{}
	maxRounds := r.profile.MaxRounds
	if maxRounds <= 0 {
		maxRounds = config.MaxRounds
	}
	modelID := "default"
	ctxWindowTokens := config.DefaultContextTokens
	if r.cfg != nil {
		modelID = r.cfg.ModelID
		ctxWindowTokens = r.cfg.ContextWindowTokens(r.cfg.ModelID)
	}

	r.emit(event.Event{Type: event.AgentStarted, TraceID: traceID})

	for {
		if err := ctx.Err(); err != nil {
			out.Error = err
			out.Messages = messages
			return out
		}

		var toolDefs []llm.ToolDef
		var early *TurnOutcome
		messages, toolDefs, early = r.prepareRound(ctx, messages, modelID, traceID, maxRounds, ctxWindowTokens, &out)
		if early != nil {
			return *early
		}

		var sr *llm.StreamResult
		messages, sr, early = r.callModel(ctx, messages, toolDefs, modelID, traceID, &out)
		if early != nil {
			return *early
		}

		if sr.FinishReason == "length" {
			messages = r.handleTruncation(messages, sr, traceID)
			continue
		}

		if len(sr.ToolCalls) == 0 {
			var cont bool
			var done TurnOutcome
			messages, cont, done = r.handleNoToolCalls(ctx, messages, modelID, traceID, &out)
			if cont {
				continue
			}
			return done
		}

		batch := r.executeToolBatch(ctx, messages, sr.ToolCalls, traceID, &out)
		messages, early = r.afterTools(ctx, batch.messages, traceID, batch, &out)
		if early != nil {
			return *early
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

// --- Helpers ---

func lastUserMessage(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		c := strings.TrimSpace(msgs[i].Content)
		if c == "" || isEphemeralNudge(msgs[i]) || isInjectedTurnContext(msgs[i]) {
			continue
		}
		return c
	}
	return ""
}

func stripInjectedTurnContext(messages []llm.Message) []llm.Message {
	removed := false
	filtered := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		if isInjectedTurnContext(message) {
			removed = true
			continue
		}
		filtered = append(filtered, message)
	}
	if !removed {
		return messages
	}
	return filtered
}

func isInjectedTurnContext(message llm.Message) bool {
	if message.Role != llm.RoleUser {
		return false
	}
	content := strings.TrimSpace(message.Content)
	return strings.HasPrefix(content, "<session-context>") ||
		strings.HasPrefix(content, "<memory-recall>")
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
	"<post-explore>",
	"<budget-warn>",
	"<limit>",
	"<judge-critical>",
	"<verification-failed>",
	"<auto-lesson>",
	"<planning-required>",
	"<session-context>",
	"<response-truncated>",
}

// postExploreNudge is loaded per-session via defaultToolInterceptors
// from the injected prompt loader — see tool_interceptors.go.

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
		Model:     modelID,
		Messages:  messages,
		Tools:     nil,
		Reasoning: reasoningRequestFromConfig(r.cfg),
	}, newPrefixedSink(r.profile.Role))
	r.turn.rounds++
	if err != nil || sr == nil {
		messages = append(messages, llm.AssistantMessage(
			fmt.Sprintf("[interrupted] Hit %s and the wrap-up call failed; please retry.", reason)))
		out.Error = fmt.Errorf("%s wrap-up failed", reason)
		out.StoppedReason = reason
	} else {
		r.turn.usage.Add(sr.Usage)
		messages = append(messages, sr.ToAssistantMessage())
		out.Completed = true
		out.StoppedReason = reason
	}
	out.Rounds = r.turn.rounds
	out.ToolFailures = r.turn.failures
	out.Messages = messages
	r.emit(event.Event{
		Type:    event.TurnComplete,
		TraceID: traceID,
		Payload: map[string]string{"note": reason, "rounds": fmt.Sprintf("%d", r.turn.rounds)},
		Usage:   &r.turn.usage,
	})
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

func extractBashCommand(rawArgs string) string {
	if rawArgs == "" {
		return ""
	}
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &a); err != nil {
		return ""
	}
	return a.Command
}

// isRepoWalkBash reports whether a bash command is a broad tree walk
// (find / recursive ls / tree) that lead should not use after explore.
func isRepoWalkBash(command string) bool {
	c := strings.ToLower(strings.TrimSpace(command))
	if c == "" {
		return false
	}
	if c == "find" || strings.HasPrefix(c, "find ") || strings.HasPrefix(c, "find\t") ||
		strings.Contains(c, " find ") || strings.Contains(c, ";find ") ||
		strings.Contains(c, "|find ") || strings.Contains(c, "&& find ") ||
		strings.Contains(c, "|| find ") {
		return true
	}
	// Recursive ls: require capital R in short flags (ls -R / ls -laR). Do not
	// treat ls -r (reverse) as a repo walk.
	for _, part := range strings.Fields(command) { // preserve case for -R
		if part == "--recursive" {
			return true
		}
		if strings.HasPrefix(part, "-") && !strings.HasPrefix(part, "--") && strings.Contains(part, "R") {
			return true
		}
	}
	if c == "tree" || strings.HasPrefix(c, "tree ") || strings.Contains(c, " tree ") ||
		strings.Contains(c, ";tree ") || strings.Contains(c, "|tree ") {
		return true
	}
	return false
}

// postExploreBlock decides whether a lead tool call should be refused after a
// successful explore. Explore-role runners are never gated here.
func (r *Runner) postExploreBlock(tc llm.ToolCall) (blocked bool, reason string) {
	return postExploreBlockTurn(&r.turn, r.profile.Role, tc)
}
