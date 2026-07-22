package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/task"
	"go-code-agent/internal/team"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/worktree"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// memberInfo captures persistent per-member state.
type memberInfo struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

// teamConfig is persisted to disk.
type teamConfig struct {
	TeamName string       `json:"team_name"`
	Members  []memberInfo `json:"members"`
}

// TeammateManager manages persistent named agents with WORK/IDLE cycle.
type TeammateManager struct {
	dir        string
	configPath string
	config     teamConfig
	mu         sync.Mutex

	gateway   *model.Gateway
	bus       *team.MessageBus
	taskSvc   *task.Service
	protocols *team.ProtocolStore
	worktrees *worktree.Service

	catalog     *tool.ToolCatalog
	modelID     string
	diffPreview tool.DiffPreview
	approval    tool.ApprovalChecker
	eventSink   event.Sink

	spawnMu   sync.Mutex
	lastSpawn time.Time

	sessCtxMu sync.RWMutex
	sessCtx   context.Context
	wg        sync.WaitGroup
}

// SetDiffPreview makes teammate file mutations go through the same preview gate as lead mutations.
func (tm *TeammateManager) SetDiffPreview(preview tool.DiffPreview) { tm.diffPreview = preview }

// SetApproval wires the session HITL adapter so teammate tools are gated
// the same way as lead tools (plan gate still controls CanWrite).
func (tm *TeammateManager) SetApproval(a tool.ApprovalChecker) { tm.approval = a }

func (tm *TeammateManager) SetEventSink(sink event.Sink) { tm.eventSink = sink }

func NewTeammateManager(
	dir string,
	gw *model.Gateway,
	bus *team.MessageBus,
	taskSvc *task.Service,
	protocols *team.ProtocolStore,
	worktrees *worktree.Service,
	catalog *tool.ToolCatalog,
	modelID string,
) *TeammateManager {
	os.MkdirAll(dir, 0o755)
	tm := &TeammateManager{
		dir:        dir,
		configPath: filepath.Join(dir, "config.json"),
		gateway:    gw,
		bus:        bus,
		taskSvc:    taskSvc,
		protocols:  protocols,
		worktrees:  worktrees,
		catalog:    catalog,
		modelID:    modelID,
	}
	if data, err := os.ReadFile(tm.configPath); err == nil {
		json.Unmarshal(data, &tm.config)
	} else {
		tm.config.TeamName = "default"
	}
	return tm
}

func (tm *TeammateManager) save() {
	data, _ := json.MarshalIndent(tm.config, "", "  ")
	os.WriteFile(tm.configPath, data, 0o644)
}

func (tm *TeammateManager) findIndex(name string) int {
	for i, m := range tm.config.Members {
		if m.Name == name {
			return i
		}
	}
	return -1
}

func (tm *TeammateManager) setStatus(name, status string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if i := tm.findIndex(name); i >= 0 {
		tm.config.Members[i].Status = status
		tm.save()
	}
}

func (tm *TeammateManager) SetSessionCtx(ctx context.Context) {
	tm.sessCtxMu.Lock()
	tm.sessCtx = ctx
	tm.sessCtxMu.Unlock()
}

func (tm *TeammateManager) getSessionCtx() context.Context {
	tm.sessCtxMu.RLock()
	defer tm.sessCtxMu.RUnlock()
	return tm.sessCtx
}

func (tm *TeammateManager) Wait() { tm.wg.Wait() }

// Spawn starts a persistent autonomous teammate with worktree isolation.
// If worktree creation fails, fail-closed: no teammate starts and error is returned.
func (tm *TeammateManager) Spawn(ctx context.Context, name, role, prompt string) string {
	tm.spawnMu.Lock()
	defer tm.spawnMu.Unlock()
	if !tm.lastSpawn.IsZero() {
		if wait := config.SpawnMinInterval - time.Since(tm.lastSpawn); wait > 0 {
			time.Sleep(wait)
		}
	}
	tm.lastSpawn = time.Now()

	tm.mu.Lock()
	idx := tm.findIndex(name)
	if idx >= 0 {
		if s := tm.config.Members[idx].Status; s != "idle" && s != "shutdown" {
			tm.mu.Unlock()
			return fmt.Sprintf("Error: '%s' is currently %s", name, s)
		}
	}

	if tm.worktrees == nil {
		tm.mu.Unlock()
		return fmt.Sprintf("Error: cannot spawn '%s': worktree service unavailable", name)
	}
	lease, err := tm.worktrees.Acquire(name)
	if err != nil {
		tm.mu.Unlock()
		return fmt.Sprintf("Error: cannot spawn '%s': worktree isolation failed: %v", name, err)
	}

	if idx >= 0 {
		tm.config.Members[idx].Status = "working"
		tm.config.Members[idx].Role = role
	} else {
		tm.config.Members = append(tm.config.Members, memberInfo{name, role, "working"})
	}
	tm.save()
	tm.mu.Unlock()

	lifetimeCtx := tm.getSessionCtx()
	if lifetimeCtx == nil {
		lifetimeCtx = context.Background()
	}
	tm.wg.Add(1)
	go func() {
		defer tm.wg.Done()
		tm.autonomousLoop(lifetimeCtx, name, role, prompt, lease.WorktreeDir)
	}()
	return fmt.Sprintf("Spawned '%s' (role: %s, workdir: %s)", name, role, lease.WorktreeDir)
}

// autonomousLoop runs a WORK → IDLE → WORK cycle within the assigned worktree.
func (tm *TeammateManager) autonomousLoop(ctx context.Context, name, role, prompt, worktreePath string) {
	teamName := tm.config.TeamName

	sys := fmt.Sprintf(
		"You are '%s' (role: %s) on team '%s'. "+
			"Use tools to complete your task. Send messages to coordinate with the team. "+
			"Use `idle` when you have completed your work. "+
			"Use `claim_task` to pick up tasks. "+
			"For write operations, submit a plan first using `submit_plan`.", name, role, teamName)

	msgs := []llm.Message{llm.SystemMessage(sys), llm.UserMessage(prompt)}

	for {
		if tm.workPhase(ctx, name, worktreePath, &msgs) == "shutdown" {
			return
		}
		if !tm.idlePhase(name, role, teamName, &msgs) {
			return
		}
	}
}

// workPhase runs the inner agent loop within the isolated worktree. Returns "shutdown" or "idle".
func (tm *TeammateManager) workPhase(ctx context.Context, name, worktreePath string, msgs *[]llm.Message) string {
	scope := &tool.ToolScope{
		Role:        "teammate",
		AgentID:     name,
		Workdir:     worktreePath,
		CanRead:     true,
		CanWrite:    team.HasApprovedPlan(tm.protocols, name),
		CanExecute:  true,
		CanNetwork:  true,
		CanTeam:     true,
		CanMemory:   true,
		DiffPreview: tm.diffPreview,
	}
	executor := tool.NewExecutor(tm.catalog, tm.approval, nil)

	traceID := "team-" + name
	if tm.eventSink != nil {
		tm.eventSink.Emit(event.Event{Type: event.AgentStarted, TraceID: traceID, AgentID: name})
	}
	for range config.TeammateWorkMaxRounds {
		// clear_at_least guard: only clear old tool results when it frees a
		// worthwhile number of bytes, so we don't bust the cache prefix for a
		// negligible saving.
		MicroCompact(*msgs, config.MicroCompactMinClearBytes)

		for _, m := range tm.bus.ReadInbox(name) {
			if t, _ := m["type"].(string); t == "shutdown_request" {
				tm.setStatus(name, "shutdown")
				return "shutdown"
			}
			data, _ := json.Marshal(m)
			*msgs = append(*msgs, llm.UserMessage(string(data)))
		}

		toolDefs := tool.NewExecutor(tm.catalog, tm.approval, nil).ToolDefs()
		modelStart := time.Now()
		sr, err := tm.gateway.Stream(ctx, "teammate", llm.CallParams{
			Model:     tm.modelID,
			Messages:  *msgs,
			Tools:     toolDefs,
			MaxTokens: config.DefaultMaxOutputTokens,
		}, newPrefixedSink(name))
		if tm.eventSink != nil {
			tm.eventSink.Emit(event.Event{Type: event.ModelCalled, TraceID: traceID, AgentID: name, Duration: time.Since(modelStart)})
		}
		if err != nil {
			tm.setStatus(name, "shutdown")
			return "shutdown"
		}
		if sr == nil {
			tm.setStatus(name, "shutdown")
			return "shutdown"
		}
		*msgs = append(*msgs, sr.ToAssistantMessage())
		if sr.FinishReason != "tool_calls" {
			break
		}

		idleRequested := false
		for _, tc := range sr.ToolCalls {
			if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
				out := fmt.Sprintf("[SKIPPED] tool call '%s' has truncated arguments", tc.Name)
				*msgs = append(*msgs, llm.ToolMessage(out, tc.ID))
				continue
			}
			if tc.Name == "idle" {
				idleRequested = true
				*msgs = append(*msgs, llm.ToolMessage("Entering idle phase.", tc.ID))
				continue
			}
			if tc.Name == "send_message" {
				result := tm.handleSendMessage(name, tc.Arguments)
				*msgs = append(*msgs, llm.ToolMessage(result.Output, tc.ID))
				continue
			}
			if tc.Name == "claim_task" {
				result := tm.handleClaimTask(name, tc.Arguments)
				*msgs = append(*msgs, llm.ToolMessage(result.Output, tc.ID))
				continue
			}
			if tc.Name == "submit_plan" {
				result := tm.handleSubmitPlan(name, tc.Arguments)
				*msgs = append(*msgs, llm.ToolMessage(result.Output, tc.ID))
				continue
			}
			// Regular tool execution
			toolStart := time.Now()
			if tm.eventSink != nil {
				tm.eventSink.Emit(event.Event{Type: event.ToolStarted, TraceID: traceID, AgentID: name, ToolCallID: tc.ID, ToolName: tc.Name})
			}
			// Gate write tools by plan approval
			if isWriteTool(tc.Name) && !team.HasApprovedPlan(tm.protocols, name) {
				*msgs = append(*msgs, llm.ToolMessage(
					"[DENIED] submit_plan approval required before write operations", tc.ID))
				continue
			}
			result := executor.Execute(ctx, scope, tc)
			if tm.eventSink != nil {
				tm.eventSink.Emit(event.Event{Type: event.ToolFinished, TraceID: traceID, AgentID: name, ToolCallID: tc.ID, ToolName: tc.Name, Duration: time.Since(toolStart), Status: string(result.Status), Output: result.Output})
			}
			*msgs = append(*msgs, llm.ToolMessage(result.ToToolMessage(), tc.ID))
		}
		if idleRequested {
			break
		}
	}
	return "idle"
}

// idlePhase polls inbox and task board. Returns true if work found.
func (tm *TeammateManager) idlePhase(name, role, teamName string, msgs *[]llm.Message) bool {
	tm.setStatus(name, "idle")

	for range int(config.IdleTimeout / config.PollInterval) {
		time.Sleep(config.PollInterval)

		if inbox := tm.bus.ReadInbox(name); len(inbox) > 0 {
			for _, m := range inbox {
				if t, _ := m["type"].(string); t == "shutdown_request" {
					tm.setStatus(name, "shutdown")
					return false
				}
				data, _ := json.Marshal(m)
				*msgs = append(*msgs, llm.UserMessage(string(data)))
			}
			tm.setStatus(name, "working")
			return true
		}

		// Poll task board for pending items
		tasksDir := filepath.Join(tm.dir, "..", "tasks")
		entries, _ := filepath.Glob(filepath.Join(tasksDir, "task_*.json"))
		sort.Strings(entries)
		for _, e := range entries {
			data, _ := os.ReadFile(e)
			var t map[string]any
			json.Unmarshal(data, &t)
			if t["status"] != "pending" || (t["owner"] != nil && t["owner"] != "") {
				continue
			}
			id := int(t["id"].(float64))
			subject, _ := t["subject"].(string)
			desc, _ := t["description"].(string)

			msg, ok := tm.taskSvc.Claim(id, name)
			if !ok {
				continue
			}
			_ = msg

			if len(*msgs) <= 3 {
				*msgs = append([]llm.Message{
					llm.UserMessage(fmt.Sprintf("<identity>You are '%s', role: %s, team: %s.</identity>", name, role, teamName)),
					llm.AssistantMessage("I am " + name + ". Continuing."),
				}, *msgs...)
			}
			*msgs = append(*msgs,
				llm.UserMessage(fmt.Sprintf("<auto-claimed>Task #%d: %s\n%s</auto-claimed>", id, subject, desc)),
				llm.AssistantMessage(fmt.Sprintf("Claimed task #%d. Working on it.", id)),
			)
			tm.setStatus(name, "working")
			return true
		}
	}

	tm.setStatus(name, "shutdown")
	return false
}

// --- Token-level tool handlers for teammates ---

func (tm *TeammateManager) handleSendMessage(from string, rawArgs string) tool.Result {
	var a struct {
		To      string `json:"to"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &a); err != nil {
		return tool.Failed(fmt.Sprintf("invalid args: %v", err))
	}
	return tool.Succeeded(tm.bus.Send(from, a.To, a.Content, "message", nil))
}

func (tm *TeammateManager) handleClaimTask(name string, rawArgs string) tool.Result {
	var a struct {
		TaskID int `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &a); err != nil {
		return tool.Failed(fmt.Sprintf("invalid args: %v", err))
	}
	msg, ok := tm.taskSvc.Claim(a.TaskID, name)
	if !ok {
		return tool.Failed(msg)
	}
	return tool.Succeeded(msg)
}

func (tm *TeammateManager) handleSubmitPlan(name string, rawArgs string) tool.Result {
	var a struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &a); err != nil {
		return tool.Failed(fmt.Sprintf("invalid args: %v", err))
	}
	return tool.Succeeded(team.SubmitPlan(tm.protocols, tm.bus, name, a.Plan))
}

// ListAll returns all teammates.
func (tm *TeammateManager) ListAll() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if len(tm.config.Members) == 0 {
		return "No teammates."
	}
	lines := []string{fmt.Sprintf("Team: %s", tm.config.TeamName)}
	for _, m := range tm.config.Members {
		lines = append(lines, fmt.Sprintf("  %s (%s): %s", m.Name, m.Role, m.Status))
	}
	return strings.Join(lines, "\n")
}

func (tm *TeammateManager) MemberNames() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	names := make([]string, 0, len(tm.config.Members))
	for _, m := range tm.config.Members {
		names = append(names, m.Name)
	}
	return names
}

func (tm *TeammateManager) ShutdownByName(name string) string {
	if tm.findIndex(name) < 0 {
		return fmt.Sprintf("Error: no teammate named '%s'", name)
	}
	team.PostShutdownRequest(tm.protocols, tm.bus, name)
	if tm.worktrees != nil {
		_ = tm.worktrees.Release(name)
	}
	return fmt.Sprintf("Shutdown request sent to '%s'", name)
}

func (tm *TeammateManager) ShutdownAll() {
	for _, name := range tm.MemberNames() {
		_ = team.PostShutdownRequest(tm.protocols, tm.bus, name)
		if tm.worktrees != nil {
			_ = tm.worktrees.Release(name)
		}
	}
}

func isWriteTool(name string) bool {
	return name == "bash" || name == "write_file" || name == "edit_file" || name == "delete_file"
}
