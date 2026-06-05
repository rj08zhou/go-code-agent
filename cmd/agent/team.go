package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/log"
	"go-code-agent/internal/task"
	"go-code-agent/internal/team"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// TeammateManager - persistent named agents with WORK/IDLE cycle.
// Lives in cmd/agent/ (not internal/) to directly use package-level symbols.

type memberInfo struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type teamConfig struct {
	TeamName string       `json:"team_name"`
	Members  []memberInfo `json:"members"`
}

type TeammateManager struct {
	dir        string
	configPath string
	config     teamConfig
	mu         sync.Mutex
	bus        *team.MessageBus
	taskMgr    *task.TaskManager
	dagSched   *task.DAGScheduler
	tasksDir   string
	protocols  *team.ProtocolStore

	// spawnMu serializes Spawn calls and lastSpawn enforces a minimum
	// gap between consecutive spawns. A reflect step that decides to
	// fan out 3 subagents would otherwise launch their goroutines (and
	// their first LLM hits) within microseconds of each other,
	// defeating the LLM token-bucket and inviting 429s. Staggering by
	// SpawnMinInterval lets the upstream gateway's bucket refill
	// between hits.
	spawnMu   sync.Mutex
	lastSpawn time.Time
}

func NewTeamMgr(dir string, bus *team.MessageBus, taskMgr *task.TaskManager, dagSched *task.DAGScheduler, tasksDir string, protocols *team.ProtocolStore) *TeammateManager {
	os.MkdirAll(dir, 0o755)
	tm := &TeammateManager{
		dir: dir, configPath: filepath.Join(dir, "config.json"),
		bus: bus, taskMgr: taskMgr, dagSched: dagSched, tasksDir: tasksDir,
		protocols: protocols,
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

func (tm *TeammateManager) Spawn(ctx context.Context, name, role, prompt string) string {
	// Stagger consecutive spawns. Cheap per-process serialization: even
	// when callers issue several Spawn() back-to-back from one reflect
	// step, the goroutines start at least SpawnMinInterval apart so
	// their first LLM calls don't all hit the gateway at the same
	// instant. The wait is bounded by ctx.
	tm.spawnMu.Lock()
	if !tm.lastSpawn.IsZero() {
		gap := time.Since(tm.lastSpawn)
		if wait := infra.SpawnMinInterval - gap; wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				tm.spawnMu.Unlock()
				return fmt.Sprintf("Error: spawn cancelled: %v", ctx.Err())
			}
		}
	}
	tm.lastSpawn = time.Now()
	tm.spawnMu.Unlock()

	tm.mu.Lock()
	idx := tm.findIndex(name)
	if idx >= 0 {
		if s := tm.config.Members[idx].Status; s != "idle" && s != "shutdown" {
			tm.mu.Unlock()
			return fmt.Sprintf("Error: '%s' is currently %s", name, s)
		}
		tm.config.Members[idx].Status = "working"
		tm.config.Members[idx].Role = role
	} else {
		tm.config.Members = append(tm.config.Members, memberInfo{name, role, "working"})
	}
	tm.save()
	tm.mu.Unlock()

	// Use a background context so the teammate's lifetime is independent
	// of the spawning tool call's timeout (PerToolTimeout = 5min).
	go tm.autonomousLoop(context.Background(), name, role, prompt)
	return fmt.Sprintf("Spawned '%s' (role: %s)", name, role)
}

// autonomousLoop runs a WORK -> IDLE -> WORK cycle until timeout or shutdown.
func (tm *TeammateManager) autonomousLoop(ctx context.Context, name, role, prompt string) {
	teamName := tm.config.TeamName
	tmpl := app.PromptLoader.Load("teammate")
	sys := strings.NewReplacer(
		"{{name}}", name, "{{role}}", role,
		"{{team}}", teamName, "{{workdir}}", workdir,
	).Replace(tmpl)
	msgs := []llm.Message{llm.SystemMessage(sys), llm.UserMessage(prompt)}

	tools := coreToolDefs(true)
	tools = append(tools,
		toolDef("send_message", "Send message.", map[string]any{"to": strProp(), "content": strProp()}, []string{"to", "content"}),
		toolDef("idle", "Signal no more work.", map[string]any{}, nil),
		toolDef("claim_task", "Claim task by ID.", map[string]any{"task_id": intProp()}, []string{"task_id"}),
		toolDef("submit_plan", "Submit a plan to lead for approval before executing.", map[string]any{"plan": strProp()}, []string{"plan"}),
	)

	// writeTools require an approved plan before execution.
	// Note: `bash` is conditional — read-only inspection commands
	// (ls/cat/find/grep/...) are exempted via IsReadOnlyBash so that
	// read-only verifier subagents can run them without first going
	// through submit_plan. See cmd/agent/security.go.
	writeTools := map[string]bool{"bash": true, "write_file": true, "edit_file": true}
	baseHandlers := coreToolHandlers()

	execTool := func(toolName string, raw json.RawMessage) llm.ToolResult {
		// Gate: block write operations until plan is approved.
		if writeTools[toolName] && !team.HasApprovedPlan(tm.protocols, name) {
			// Carve out read-only bash invocations.
			if toolName == "bash" {
				var args struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(raw, &args); err == nil && IsReadOnlyBash(args.Command) {
					// Fall through; treat as a non-write tool call.
				} else {
					return llm.MkErr("You must submit_plan and get lead approval before executing write operations.")
				}
			} else {
				return llm.MkErr("You must submit_plan and get lead approval before executing write operations.")
			}
		}
		// Try shared base handlers first.
		if h, ok := baseHandlers[toolName]; ok {
			return h(ctx, raw)
		}
		// Team-specific handlers.
		switch toolName {
		case "send_message":
			var a struct {
				To      string `json:"to"`
				Content string `json:"content"`
			}
			if e := llm.ParseArgs(raw, &a); e != "" {
				return llm.MkErr(e)
			}
			return llm.MkOk(tm.bus.Send(name, a.To, a.Content, "message", nil))
		case "claim_task":
			var a struct {
				TaskID int `json:"task_id"`
			}
			if e := llm.ParseArgs(raw, &a); e != "" {
				return llm.MkErr(e)
			}
			msg, ok := tm.taskMgr.Claim(a.TaskID, name)
			if !ok {
				return llm.MkErr(msg)
			}
			return llm.MkOk(msg)
		case "submit_plan":
			var a struct {
				Plan string `json:"plan"`
			}
			if e := llm.ParseArgs(raw, &a); e != "" {
				return llm.MkErr(e)
			}
			return llm.MkOk(team.SubmitPlan(tm.protocols, tm.bus, name, a.Plan))
		default:
			return llm.MkErr(fmt.Sprintf("Unknown tool: %s", toolName))
		}
	}

	for { // Outer: alternates WORK and IDLE phases.
		if tm.workPhase(ctx, name, &msgs, tools, execTool) == "shutdown" {
			return
		}
		if !tm.idlePhase(name, role, teamName, &msgs) {
			return
		}
	}
}

// workPhase runs the inner agent loop. Returns "shutdown" or "idle".
func (tm *TeammateManager) workPhase(ctx context.Context, name string, msgs *[]llm.Message, tools []llm.ToolDef, execTool func(string, json.RawMessage) llm.ToolResult) string {
	for range infra.TeammateWorkMaxRounds {
		// Compress old tool results to prevent token overflow.
		microCompact(*msgs)

		for _, m := range tm.bus.ReadInbox(name) {
			if t, _ := m["type"].(string); t == "shutdown_request" {
				tm.setStatus(name, "shutdown")
				return "shutdown"
			}
			data, _ := json.Marshal(m)
			*msgs = append(*msgs, llm.UserMessage(string(data)))
		}

		sr, err := llm.NewClient(nil).StreamWithRetrySink(ctx, "team", llm.CallParams{Model: model, Messages: *msgs, Tools: tools, MaxTokens: infra.DefaultMaxOutputTokens},
			&llm.PrefixedStreamSink{Prefix: "[" + name + "]", Color: log.ColorDim})
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
			// Skip tool calls with truncated JSON arguments.
			if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
				out := fmt.Sprintf("[SKIPPED] tool call '%s' has truncated arguments", tc.Name)
				log.PrintTeamTool(name, tc.Name, out)
				*msgs = append(*msgs, llm.ToolMessage(out, tc.ID))
				continue
			}
			var result llm.ToolResult
			if tc.Name == "idle" {
				idleRequested = true
				result = llm.MkOk("Entering idle phase.")
			} else {
				result = execTool(tc.Name, json.RawMessage(tc.Arguments))
			}
			log.PrintTeamTool(name, tc.Name, result.Output)
			*msgs = append(*msgs, llm.ToolMessage(result.Output, tc.ID))
		}
		if idleRequested {
			break
		}
	}
	return "idle"
}

// idlePhase polls inbox and task board. Returns true if work was found, false on timeout/shutdown.
func (tm *TeammateManager) idlePhase(name, role, teamName string, msgs *[]llm.Message) bool {
	tm.setStatus(name, "idle")

	for range int(infra.IdleTimeout / infra.PollInterval) {
		time.Sleep(infra.PollInterval)

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

		entries, _ := filepath.Glob(filepath.Join(tm.tasksDir, "task_*.json"))
		sort.Strings(entries)
		for _, e := range entries {
			data, _ := os.ReadFile(e)
			var t map[string]any
			json.Unmarshal(data, &t)

			if t["status"] != "pending" || (t["owner"] != nil && t["owner"] != "") {
				continue
			}

			id := int(t["id"].(float64))

			// Check DAG readiness: all predecessors must be completed.
			if !tm.dagSched.IsReady(id) {
				continue
			}
			subject, _ := t["subject"].(string)
			desc, _ := t["description"].(string)
			if _, ok := tm.taskMgr.Claim(id, name); !ok {
				continue
			}

			if len(*msgs) <= 3 {
				*msgs = append(
					[]llm.Message{
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

// shutdownTeammates sends shutdown requests to all active teammates (best-effort).
func shutdownTeammates() {
	if app == nil {
		return
	}
	tm := app.TeamMgr()
	if tm == nil {
		return
	}
	s := app.SessionManager.Active()
	if s == nil || s.Protocols == nil || s.Bus == nil {
		return
	}
	for _, name := range tm.MemberNames() {
		_ = team.PostShutdownRequest(s.Protocols, s.Bus, name)
	}
}
