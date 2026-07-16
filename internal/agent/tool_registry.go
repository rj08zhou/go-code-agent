package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/security"
	"go-code-agent/internal/team"
	"strings"
)

// Tool registry: definitions (30+ built-in + N MCP) + handlers + security
// levels. Base tools (bash, read_file, write_file, edit_file, delete_file)
// live in tool_base.go (coreToolSpecs).
//
// Every tool below is registered through registerToolSpec (see
// tool_base.go), which is the ONLY thing that writes to ToolDefs,
// ToolHandlers and ToolSecurityMap. That keeps a tool's LLM-facing
// schema, its execution handler and its approval Level atomic — see
// the comment on ToolSpec for why this matters.

// InitTools (re)builds the global tool registry. Safe to call again
// (e.g. after /mcp connect|disconnect) — all three registries are
// reset to empty first, so repeated calls never accumulate duplicates.
func InitTools() {
	ToolDefs = nil
	ToolHandlers = make(map[string]ToolHandler)
	ToolSecurityMap = map[string]ToolSecurityMeta{}

	// Base tools (bash/read/write/edit/delete) from tool_base.go.
	registerToolSpecs(coreToolSpecs(true)...)

	// Reasoning
	registerToolSpecs(
		spec("compress", "Manually compress conversation context.", map[string]any{}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				return llm.MkOk("Compressing...")
			}),
	)

	// Planning
	registerToolSpecs(
		spec("TodoWrite", "Update task tracking list.", map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{
				"type":       "object",
				"properties": map[string]any{"content": strProp(), "status": enumProp("pending", "in_progress", "completed"), "activeForm": strProp()},
				"required":   []string{"content", "status", "activeForm"},
			}},
		}, []string{"items"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Items []map[string]string `json:"items"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				out, err := App.Todo().Update(a.Items)
				if err != nil {
					return llm.MkErr(fmt.Sprintf("%v", err))
				}
				return llm.MkOk(out)
			}),
	)

	// Subagent + skills
	registerToolSpecs(
		spec("explore", "Delegate multi-file investigation to a read-only subagent. The subagent reads files and runs safe shell commands in its own isolated context, then returns only a concise summary — you receive the findings without burning your context on raw file content. Ideal when you need to understand architecture, trace call chains, or find how a feature is implemented across multiple files. "+
			"For single-file lookups (one function, constant, or signature) use read_file directly instead. "+
			"The subagent has NO write/edit/delete tools — if it concludes a change is needed, it describes the change and you perform the write. "+
			"Large time budget; if it doesn't finish in time you get a partial-progress report rather than a bare failure.",
			map[string]any{"prompt": strProp(), "agent_type": enumProp("Explore", "general-purpose")}, []string{"prompt"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Prompt    string `json:"prompt"`
					AgentType string `json:"agent_type"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				if a.AgentType == "" {
					a.AgentType = "Explore"
				}
				return llm.MkOk(runSubagent(ctx, a.Prompt, a.AgentType))
			}),
		spec("load_skill", "Load specialized knowledge by name.", map[string]any{"name": strProp()}, []string{"name"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Name string `json:"name"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.Skills.Load(a.Name))
			}),
	)

	// Background tasks
	registerToolSpecs(
		spec("background_run", "Run a shell command in the background (non-blocking). "+
			"IMPORTANT: after 'timeout' seconds elapse (default 120s if omitted) the process is killed automatically, even if it is still healthy and useful - e.g. a dev server. "+
			"For long-running/indefinite processes that must keep running (dev servers, watch-mode builds, etc.), pass a much larger explicit timeout in seconds (e.g. 86400 for ~24h); "+
			"otherwise it will be silently killed once the default 120s elapses and later checks/requests against it will fail.",
			map[string]any{"command": strProp(), "timeout": intProp()}, []string{"command"}, security.ApproveDanger,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.BgMgr().Run(a.Command, a.Timeout))
			}),
		// Read-only status poll for a command background_run already
		// launched — no new side effects, so security.ApproveAuto (unlike its
		// launcher above).
		spec("check_background", "Check background task status.", map[string]any{"task_id": strProp()}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					TaskID string `json:"task_id"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.BgMgr().Check(a.TaskID))
			}),
	)

	// Persistent tasks
	registerToolSpecs(
		spec("task_create", "Create a persistent file task. Optionally specify depends_on to set DAG dependencies. "+
			"The response includes the new task's id (e.g. {\"id\": 4, ...}) — always parse it rather than guessing; call task_list if you lose track.",
			map[string]any{"subject": strProp(), "description": strProp(), "depends_on": intArrayProp()}, []string{"subject"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Subject     string `json:"subject"`
					Description string `json:"description"`
					DependsOn   []int  `json:"depends_on"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.TaskMgr().Create(a.Subject, a.Description, a.DependsOn))
			}),
		spec("task_get", "Get task details by ID.", map[string]any{"task_id": intProp()}, []string{"task_id"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					TaskID int `json:"task_id"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.TaskMgr().Get(a.TaskID))
			}),
		spec("task_update", "Update task STATUS only (pending/in_progress/completed/deleted). Cannot modify description or subject - use task_create for new tasks with different descriptions. "+
			"Verify the task id exists (task_list) before updating, and use task_dag first if unsure about dependency ordering.",
			map[string]any{"task_id": intProp(), "status": enumProp("pending", "in_progress", "completed", "deleted")}, []string{"task_id"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					TaskID int    `json:"task_id"`
					Status string `json:"status"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				result := App.TaskMgr().Update(a.TaskID, a.Status)
				if a.Status == "completed" {
					if ready := App.DagSched().OnComplete(a.TaskID); ready != "" {
						result += "\n" + ready
					}
				}
				return llm.MkOk(result)
			}),
		spec("task_list", "List all tasks.", map[string]any{}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				return llm.MkOk(App.TaskMgr().ListAll())
			}),
	)

	// DAG dependency tools
	registerToolSpecs(
		spec("task_add_dep", "Add a DAG dependency: `from` must complete before `to` can start. Both task ids must already exist (check task_list if unsure).", map[string]any{"from": intProp(), "to": intProp()}, []string{"from", "to"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					From int `json:"from"`
					To   int `json:"to"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.DagSched().AddEdge(a.From, a.To))
			}),
		spec("task_remove_dep", "Remove a DAG dependency edge.", map[string]any{"from": intProp(), "to": intProp()}, []string{"from", "to"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					From int `json:"from"`
					To   int `json:"to"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.DagSched().RemoveEdge(a.From, a.To))
			}),
		spec("task_ready", "List tasks whose DAG predecessors are all completed (ready to start).", map[string]any{}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				return llm.MkOk(App.DagSched().ReadyTasks())
			}),
		spec("task_dag", "Show topological execution order and dependency edges.", map[string]any{}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				return llm.MkOk(App.DagSched().TopoView())
			}),
	)

	// Team management. spawn_teammate is Safe (mirrors `task`: it
	// starts an autonomous agent, but unlike `task` that agent keeps
	// running/polling after this call returns). The pure messaging
	// tools (send_message/read_inbox/broadcast/list_teammates) don't
	// touch the filesystem or shell, so security.ApproveAuto.
	registerToolSpecs(
		spec("spawn_teammate", "Spawn a persistent autonomous teammate.", map[string]any{"name": strProp(), "role": strProp(), "prompt": strProp()}, []string{"name", "role", "prompt"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Name   string `json:"name"`
					Role   string `json:"role"`
					Prompt string `json:"prompt"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.TeamMgr().Spawn(ctx, a.Name, a.Role, a.Prompt))
			}),
		spec("list_teammates", "List all teammates.", map[string]any{}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				return llm.MkOk(App.TeamMgr().ListAll())
			}),
		spec("send_message", "Send a message to a teammate.", map[string]any{"to": strProp(), "content": strProp(), "msg_type": strProp()}, []string{"to", "content"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					To      string `json:"to"`
					Content string `json:"content"`
					MsgType string `json:"msg_type"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				if a.MsgType == "" {
					a.MsgType = "message"
				}
				return llm.MkOk(App.Bus().Send("lead", a.To, a.Content, a.MsgType, nil))
			}),
		spec("read_inbox", "Read and drain the lead's inbox.", map[string]any{}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				data, _ := json.MarshalIndent(App.Bus().ReadInbox("lead"), "", "  ")
				return llm.MkOk(string(data))
			}),
		spec("broadcast", "Send message to all teammates.", map[string]any{"content": strProp()}, []string{"content"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Content string `json:"content"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.Bus().Broadcast("lead", a.Content, App.TeamMgr().MemberNames()))
			}),
	)

	// Protocols. shutdown_request stops another running agent and
	// claim_task takes ownership of shared work, so both are Safe
	// rather than Auto; plan_approval is the lead granting/denying a
	// teammate's write plan — an oversight action, not itself a write,
	// so Auto (requiring a second confirmation to approve an approval
	// would just be confirmation fatigue).
	registerToolSpecs(
		spec("shutdown_request", "Request a teammate to shut down.", map[string]any{"teammate": strProp()}, []string{"teammate"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Teammate string `json:"teammate"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(team.HandleShutdownReq(App.Protocols(), App.Bus(), a.Teammate))
			}),
		spec("plan_approval", "Approve or reject a teammate's plan.", map[string]any{"request_id": strProp(), "approve": boolProp(), "feedback": strProp()}, []string{"request_id", "approve"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					RequestID string `json:"request_id"`
					Approve   bool   `json:"approve"`
					Feedback  string `json:"feedback"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(team.HandlePlanReview(App.Protocols(), App.Bus(), a.RequestID, a.Approve, a.Feedback))
			}),
		spec("claim_task", "Claim a task from the board.", map[string]any{"task_id": intProp()}, []string{"task_id"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					TaskID int `json:"task_id"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				msg, ok := App.TaskMgr().Claim(a.TaskID, "lead")
				if !ok {
					return llm.MkErr(msg)
				}
				return llm.MkOk(msg)
			}),
	)

	// Memory tools
	registerToolSpecs(
		spec("memory_write", "Save an important fact or observation to long-term memory. Categories: preference (user settings, highest recall priority), "+
			"lesson (bugs/gotchas, high priority), change_log (a code modification + its rationale + risk reasoning — used later to spot emergent bugs "+
			"from combined changes, mid-high priority), fact (project facts/architecture, standard), context (temporary, decays fast).",
			map[string]any{
				"content":  strProp(),
				"category": enumProp("preference", "fact", "lesson", "context", "change_log"),
			}, []string{"content", "category"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Content  string `json:"content"`
					Category string `json:"category"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				if a.Category == "" {
					a.Category = "fact"
				}
				return llm.MkOk(App.MemStore.WriteMemory(a.Content, a.Category))
			}),
		spec("memory_search", "Search stored memories for relevant information, ranked by similarity. Optional within_days filters to memories from recent N days (key for code-review: spot emergent bugs from recent changes). Optional category narrows to one class.",
			map[string]any{
				"query":       strProp(),
				"top_k":       intProp(),
				"within_days": intProp(),
				"category":    enumProp("preference", "fact", "lesson", "context", "change_log"),
			}, []string{"query"}, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Query      string `json:"query"`
					TopK       int    `json:"top_k"`
					WithinDays int    `json:"within_days"`
					Category   string `json:"category"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				if a.TopK <= 0 {
					a.TopK = 5
				}
				results := App.MemStore.HybridSearchFiltered(a.Query, a.TopK, a.WithinDays, a.Category)
				if len(results) == 0 {
					return llm.MkOk("No relevant memories found.")
				}
				var lines []string
				for _, res := range results {
					lines = append(lines, fmt.Sprintf("[%s] (score: %.4f) %s", res.Path, res.Score, res.Snippet))
				}
				return llm.MkOk(strings.Join(lines, "\n"))
			}),
		spec("memory_delete", "Delete a memory entry by matching content. Finds the most similar entry and removes it.",
			map[string]any{
				"query":    strProp(),
				"category": enumProp("preference", "fact", "lesson", "context", "change_log"),
			}, []string{"query"}, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				var a struct {
					Query    string `json:"query"`
					Category string `json:"category"`
				}
				if e := llm.ParseArgs(r, &a); e != "" {
					return llm.MkErr(e)
				}
				return llm.MkOk(App.MemStore.DeleteMemory(a.Query, a.Category))
			}),
		spec("memory_stats", "Show memory store statistics (evergreen chars, daily files, entry count).",
			map[string]any{}, nil, security.ApproveAuto,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				ec, df, de := App.MemStore.GetStats()
				return llm.MkOk(fmt.Sprintf("evergreen: %d chars, daily files: %d, entries: %d", ec, df, de))
			}),
		spec("session_save_memory", "Extract insights from the current session's conversation history and save them to long-term memory. Called automatically when the session ends; use this to trigger it manually.",
			map[string]any{}, nil, security.ApproveSafe,
			func(ctx context.Context, r json.RawMessage) ToolResult {
				if App.SessionManager == nil || App.SessionManager.Active() == nil {
					return llm.MkErr("no active session")
				}
				result, err := App.SessionManager.SaveToMemory(ctx, App.SessionManager.Active())
				if err != nil {
					return llm.MkErr(fmt.Sprintf("%v", err))
				}
				return llm.MkOk(result)
			}),
	)

	// Web access (web_fetch/web_search) - see web_tools.go.
	registerWebTools()

	// Append MCP tools from connected servers. Their approval Level is
	// resolved dynamically in security.go's checkToolApproval (they
	// don't have a static ToolSpec since the set is only known at
	// runtime, after each server's tools/list handshake).
	if App.MCPMgr != nil {
		ToolDefs = append(ToolDefs, App.MCPMgr.ToolDefs()...)
	}
}
