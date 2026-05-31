package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/team"
	"strings"
)

// Tool registry: definitions (30+ built-in + N MCP) + handlers.
// Base tools (bash, read_file, write_file, edit_file, delete_file) live in tool_base.go.

func initTools() {
	// Base tools from tool_base.go.
	toolDefs = coreToolDefs(true)
	toolHandlers = coreToolHandlers()

	// Higher-level tools.

	// Reasoning
	toolDefs = append(toolDefs,
		toolDef("think", "Record your reasoning before taking action. Does NOT run anything or fetch data - it only logs structured thought into the conversation. Use this to: restate the user's request, list assumptions, decide scope, or plan an approach before calling planning/action tools.", map[string]any{
			"thought": strProp(),
		}, []string{"thought"}),
	)
	toolHandlers["think"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Thought string `json:"thought"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		if strings.TrimSpace(a.Thought) == "" {
			return mkErr("empty thought - provide meaningful reasoning")
		}
		return mkOk("Thought recorded. Proceed with your next action.")
	}

	// Planning
	toolDefs = append(toolDefs,
		toolDef("TodoWrite", "Update task tracking list.", map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"content": strProp(), "status": enumProp("pending", "in_progress", "completed"), "activeForm": strProp()},
				"required":   []string{"content", "status", "activeForm"},
			}},
		}, []string{"items"}),
	)
	toolHandlers["TodoWrite"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Items []map[string]string `json:"items"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		out, err := app.Todo().Update(a.Items)
		if err != nil {
			return mkErr(fmt.Sprintf("%v", err))
		}
		return mkOk(out)
	}

	// Subagent + skills
	toolDefs = append(toolDefs,
		toolDef("task", "Spawn a read-only subagent to investigate code, search files, or summarize findings. The subagent has NO write/edit/delete tools — if it concludes a change is needed, it returns a summary describing the change and you (the parent) perform the write yourself.", map[string]any{"prompt": strProp(), "agent_type": enumProp("Explore", "general-purpose")}, []string{"prompt"}),
		toolDef("load_skill", "Load specialized knowledge by name.", map[string]any{"name": strProp()}, []string{"name"}),
		toolDef("compress", "Manually compress conversation context.", map[string]any{}, nil),
	)
	toolHandlers["task"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Prompt    string `json:"prompt"`
			AgentType string `json:"agent_type"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		if a.AgentType == "" {
			a.AgentType = "Explore"
		}
		return mkOk(runSubagent(ctx, a.Prompt, a.AgentType))
	}
	toolHandlers["load_skill"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Name string `json:"name"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(skills.Load(a.Name))
	}
	toolHandlers["compress"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		return mkOk("Compressing...")
	}

	// Background tasks
	toolDefs = append(toolDefs,
		toolDef("background_run", "Run command in background.", map[string]any{"command": strProp(), "timeout": intProp()}, []string{"command"}),
		toolDef("check_background", "Check background task status.", map[string]any{"task_id": strProp()}, nil),
	)
	toolHandlers["background_run"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(app.BgMgr().Run(a.Command, a.Timeout))
	}
	toolHandlers["check_background"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			TaskID string `json:"task_id"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(app.BgMgr().Check(a.TaskID))
	}

	// Persistent tasks
	toolDefs = append(toolDefs,
		toolDef("task_create", "Create a persistent file task. Optionally specify depends_on to set DAG dependencies.", map[string]any{"subject": strProp(), "description": strProp(), "depends_on": intArrayProp()}, []string{"subject"}),
		toolDef("task_get", "Get task details by ID.", map[string]any{"task_id": intProp()}, []string{"task_id"}),
		toolDef("task_update", "Update task status. Use task_add_dep/task_remove_dep for dependencies.", map[string]any{"task_id": intProp(), "status": enumProp("pending", "in_progress", "completed", "deleted")}, []string{"task_id"}),
		toolDef("task_list", "List all tasks.", map[string]any{}, nil),
	)
	toolHandlers["task_create"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Subject     string `json:"subject"`
			Description string `json:"description"`
			DependsOn   []int  `json:"depends_on"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(app.TaskMgr().Create(a.Subject, a.Description, a.DependsOn))
	}
	toolHandlers["task_get"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			TaskID int `json:"task_id"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(app.TaskMgr().Get(a.TaskID))
	}
	toolHandlers["task_update"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			TaskID int    `json:"task_id"`
			Status string `json:"status"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		result := app.TaskMgr().Update(a.TaskID, a.Status)
		if a.Status == "completed" {
			if ready := app.DagSched().OnComplete(a.TaskID); ready != "" {
				result += "\n" + ready
			}
		}
		return mkOk(result)
	}
	toolHandlers["task_list"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		return mkOk(app.TaskMgr().ListAll())
	}

	// DAG dependency tools
	toolDefs = append(toolDefs,
		toolDef("task_add_dep", "Add a DAG dependency: `from` must complete before `to` can start.", map[string]any{"from": intProp(), "to": intProp()}, []string{"from", "to"}),
		toolDef("task_remove_dep", "Remove a DAG dependency edge.", map[string]any{"from": intProp(), "to": intProp()}, []string{"from", "to"}),
		toolDef("task_ready", "List tasks whose DAG predecessors are all completed (ready to start).", map[string]any{}, nil),
		toolDef("task_dag", "Show topological execution order and dependency edges.", map[string]any{}, nil),
	)
	toolHandlers["task_add_dep"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			From int `json:"from"`
			To   int `json:"to"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(app.DagSched().AddEdge(a.From, a.To))
	}
	toolHandlers["task_remove_dep"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			From int `json:"from"`
			To   int `json:"to"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(app.DagSched().RemoveEdge(a.From, a.To))
	}
	toolHandlers["task_ready"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		return mkOk(app.DagSched().ReadyTasks())
	}
	toolHandlers["task_dag"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		return mkOk(app.DagSched().TopoView())
	}

	// Team management
	toolDefs = append(toolDefs,
		toolDef("spawn_teammate", "Spawn a persistent autonomous teammate.", map[string]any{"name": strProp(), "role": strProp(), "prompt": strProp()}, []string{"name", "role", "prompt"}),
		toolDef("list_teammates", "List all teammates.", map[string]any{}, nil),
		toolDef("send_message", "Send a message to a teammate.", map[string]any{"to": strProp(), "content": strProp(), "msg_type": strProp()}, []string{"to", "content"}),
		toolDef("read_inbox", "Read and drain the lead's inbox.", map[string]any{}, nil),
		toolDef("broadcast", "Send message to all teammates.", map[string]any{"content": strProp()}, []string{"content"}),
	)
	toolHandlers["spawn_teammate"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Name   string `json:"name"`
			Role   string `json:"role"`
			Prompt string `json:"prompt"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(app.TeamMgr().Spawn(ctx, a.Name, a.Role, a.Prompt))
	}
	toolHandlers["list_teammates"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		return mkOk(app.TeamMgr().ListAll())
	}
	toolHandlers["send_message"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			To      string `json:"to"`
			Content string `json:"content"`
			MsgType string `json:"msg_type"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		if a.MsgType == "" {
			a.MsgType = "message"
		}
		return mkOk(app.Bus().Send("lead", a.To, a.Content, a.MsgType, nil))
	}
	toolHandlers["read_inbox"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		data, _ := json.MarshalIndent(app.Bus().ReadInbox("lead"), "", "  ")
		return mkOk(string(data))
	}
	toolHandlers["broadcast"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Content string `json:"content"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(app.Bus().Broadcast("lead", a.Content, app.TeamMgr().MemberNames()))
	}

	// Protocols
	toolDefs = append(toolDefs,
		toolDef("shutdown_request", "Request a teammate to shut down.", map[string]any{"teammate": strProp()}, []string{"teammate"}),
		toolDef("plan_approval", "Approve or reject a teammate's plan.", map[string]any{"request_id": strProp(), "approve": boolProp(), "feedback": strProp()}, []string{"request_id", "approve"}),
		toolDef("claim_task", "Claim a task from the board.", map[string]any{"task_id": intProp()}, []string{"task_id"}),
	)
	toolHandlers["shutdown_request"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Teammate string `json:"teammate"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(team.HandleShutdownReq(app.Protocols(), app.Bus(), a.Teammate))
	}
	toolHandlers["plan_approval"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			RequestID string `json:"request_id"`
			Approve   bool   `json:"approve"`
			Feedback  string `json:"feedback"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(team.HandlePlanReview(app.Protocols(), app.Bus(), a.RequestID, a.Approve, a.Feedback))
	}
	toolHandlers["claim_task"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			TaskID int `json:"task_id"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		msg, ok := app.TaskMgr().Claim(a.TaskID, "lead")
		if !ok {
			return mkErr(msg)
		}
		return mkOk(msg)
	}

	// Memory tools
	toolDefs = append(toolDefs,
		toolDef("memory_write", "Save an important fact or observation to long-term memory. Use category=change_log to record a code modification with its rationale - critical for later spotting emergent bugs from combined changes.", map[string]any{
			"content":  strProp(),
			"category": enumProp("preference", "fact", "lesson", "context", "change_log"),
		}, []string{"content", "category"}),
		toolDef("memory_search", "Search stored memories for relevant information, ranked by similarity. Optional within_days filters to memories from recent N days (key for code-review: spot emergent bugs from recent changes). Optional category narrows to one class.", map[string]any{
			"query":       strProp(),
			"top_k":       intProp(),
			"within_days": intProp(),
			"category":    enumProp("preference", "fact", "lesson", "context", "change_log"),
		}, []string{"query"}),
		toolDef("memory_delete", "Delete a memory entry by matching content. Finds the most similar entry and removes it.", map[string]any{
			"query":    strProp(),
			"category": enumProp("preference", "fact", "lesson", "context", "change_log"),
		}, []string{"query"}),
		toolDef("session_save_memory", "Extract insights from the current session's conversation history and save them to long-term memory. Called automatically when the session ends; use this to trigger it manually.", map[string]any{}, nil),
	)
	toolHandlers["memory_write"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Content  string `json:"content"`
			Category string `json:"category"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		if a.Category == "" {
			a.Category = "fact"
		}
		return mkOk(memStore.WriteMemory(a.Content, a.Category))
	}
	toolHandlers["memory_search"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Query      string `json:"query"`
			TopK       int    `json:"top_k"`
			WithinDays int    `json:"within_days"`
			Category   string `json:"category"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		if a.TopK <= 0 {
			a.TopK = 5
		}
		results := memStore.HybridSearchFiltered(a.Query, a.TopK, a.WithinDays, a.Category)
		if len(results) == 0 {
			return mkOk("No relevant memories found.")
		}
		var lines []string
		for _, res := range results {
			lines = append(lines, fmt.Sprintf("[%s] (score: %.4f) %s", res.Path, res.Score, res.Snippet))
		}
		return mkOk(strings.Join(lines, "\n"))
	}
	toolHandlers["memory_delete"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		var a struct {
			Query    string `json:"query"`
			Category string `json:"category"`
		}
		if e := parseArgs(r, &a); e != "" {
			return mkErr(e)
		}
		return mkOk(memStore.DeleteMemory(a.Query, a.Category))
	}
	toolHandlers["session_save_memory"] = func(ctx context.Context, r json.RawMessage) ToolResult {
		if app.SessionManager == nil || app.SessionManager.Active() == nil {
			return mkErr("no active session")
		}
		result, err := app.SessionManager.SaveToMemory(ctx, app.SessionManager.Active())
		if err != nil {
			return mkErr(fmt.Sprintf("%v", err))
		}
		return mkOk(result)
	}

	// Append MCP tools from connected servers.
	if mcpMgr != nil {
		toolDefs = append(toolDefs, mcpMgr.ToolDefs()...)
	}
}
