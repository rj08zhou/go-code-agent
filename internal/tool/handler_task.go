package tool

import (
	"encoding/json"
)

func taskTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name: "TodoWrite",
		Description: `Update the session todo checklist. Pass items: an array of {content, status} where status is pending|in_progress|completed. Advance work by updating status — do not invent task_id or sort by id (this tool has no task_id).

When to use: short linear checklists (about 3+ concrete steps you will execute); user gave a list of items; local work that benefits from progress tracking.
When NOT to use: a single trivial step (just do it); pure Q&A/analysis with no edits; work with real dependencies or that must survive restart → use task_create + DAG; do not use todos as an investigation plan ("read A, read B") → use explore.

Example — rename a symbol in one file + update its test: TodoWrite with 2–3 items, then edit. Incorrect: task_create a 5-node DAG for a local rename.
Example — README typo: edit directly (or one TodoWrite item). Incorrect: task_create planning ceremony for a one-line change.`,
		RiskLevel: RiskSafe,
		Effects:     Effects(EffectSessionMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"items"},
			"properties": map[string]any{
				"items": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object", "required": []string{"content", "status"},
						"properties": map[string]any{
							"content": map[string]any{"type": "string", "description": "Task description."},
							"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}, "description": "Task status."},
						},
					},
				},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Items []map[string]string `json:"items"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if d.todoSvc == nil {
				return Failed("todo service unavailable")
			}
			output, err := d.todoSvc.Update(a.Items)
			if err != nil {
				return Failed(err.Error())
			}
			return Succeeded(output)
		},
	})

	defs = append(defs,
		makeTaskTool("task_create", d.taskSvc),
		makeTaskTool("task_list", d.taskSvc),
		makeTaskTool("task_update", d.taskSvc),
		makeTaskTool("task_get", d.taskSvc),
		makeTaskTool("task_add_dep", d.taskSvc),
		makeTaskTool("task_remove_dep", d.taskSvc),
		makeTaskTool("task_ready", d.taskSvc),
		makeTaskTool("task_dag", d.taskSvc),
		makeTaskTool("claim_task", d.taskSvc),
	)

	return defs
}

func makeTaskTool(name string, taskSvc TaskService) ToolDefinition {
	switch name {
	case "task_create":
		return ToolDefinition{
			Name: "task_create",
			Description: `Create a persistent task (optional depends_on: array of numeric task IDs). The returned numeric ID is authoritative — use it in task_update/task_get/task_add_dep; never use 0 or invent IDs.

When to use: multi-step work where order/deps matter, or the plan should survive restart/handoff.
When NOT to use: local rename / one-file fix → TodoWrite or just edit; read-only investigation → explore.

If you create multiple related tasks, set depends_on (or call task_add_dep) and review with task_dag before executing. Runtime stops you if multiple tasks have no edges.

Example — auth middleware + wire into server + migrate handlers + integration tests (later steps depend on earlier): task_create with depends_on / task_add_dep, then task_dag, then execute in order. Incorrect: TodoWrite with unordered bullets and hope sequencing works.`,
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"subject"},
				"properties": map[string]any{
					"subject":     map[string]any{"type": "string", "description": "Short task title."},
					"description": map[string]any{"type": "string"},
					"depends_on":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				},
			}),
			RiskLevel: RiskSafe,
			Effects:   Effects(EffectSessionMutation),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct {
					Subject     string `json:"subject"`
					Description string `json:"description"`
					DependsOn   []int  `json:"depends_on"`
				}
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if taskSvc != nil {
					return Succeeded(taskSvc.Create(a.Subject, a.Description, a.DependsOn))
				}
				return Failed("task service unavailable")
			},
		}
	case "task_get":
		return ToolDefinition{
			Name: "task_get",
			Description: `Get task details by numeric task_id from task_create (e.g. 1). Never pass 0 or omit task_id. See task_create for when to use the DAG task system vs TodoWrite.`,
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"task_id"},
				"properties": map[string]any{"task_id": map[string]any{"type": "integer", "minimum": 1}},
			}),
			RiskLevel: RiskAuto,
			Effects:   Effects(),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct {
					TaskID int `json:"task_id"`
				}
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if taskSvc != nil {
					return Succeeded(taskSvc.Get(a.TaskID))
				}
				return Failed("task service unavailable")
			},
		}
	case "task_update":
		return ToolDefinition{
			Name: "task_update",
			Description: `Update a task's status only (pending|in_progress|completed|deleted) — does not change subject/description. Pass the numeric task_id returned by task_create (e.g. 1); never use 0, omit it, or invent an ID. If unsure of IDs, call task_list first.`,
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"task_id", "status"},
				"properties": map[string]any{
					"task_id": map[string]any{"type": "integer", "minimum": 1, "description": "Numeric ID from task_create, e.g. 1 (not 0)."},
					"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed", "deleted"}},
				},
			}),
			RiskLevel: RiskSafe,
			Effects:   Effects(EffectSessionMutation),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct {
					TaskID int    `json:"task_id"`
					ID     int    `json:"id"` // compatibility with older task output
					Status string `json:"status"`
				}
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if a.TaskID == 0 {
					a.TaskID = a.ID
				}
				if a.TaskID <= 0 {
					return Failed("task_id is required and must be a positive integer; use the ID returned by task_create")
				}
				if taskSvc != nil {
					return Succeeded(taskSvc.Update(a.TaskID, a.Status))
				}
				return Failed("task service unavailable")
			},
		}
	case "task_list":
		return ToolDefinition{
			Name:        "task_list",
			Description: "List all persistent tasks and their numeric IDs. Use before task_update/task_get if you are unsure of an ID. See task_create for when to use this task system vs TodoWrite.",
			RiskLevel:   RiskAuto,
			Effects:     Effects(),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				if taskSvc != nil {
					return Succeeded(taskSvc.ListAll())
				}
				return Failed("task service unavailable")
			},
		}
	case "task_add_dep":
		return ToolDefinition{
			Name:        "task_add_dep",
			Description: "Add a DAG edge from→to using numeric IDs from task_create. Prefer depends_on at create time when known; use this to fix missing edges. See task_create for when DAG planning is appropriate.",
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"from", "to"},
				"properties": map[string]any{
					"from": map[string]any{"type": "integer", "minimum": 1},
					"to":   map[string]any{"type": "integer", "minimum": 1},
				},
			}),
			RiskLevel: RiskSafe,
			Effects:   Effects(EffectSessionMutation),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct{ From, To int }
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if taskSvc == nil {
					return Failed("task service unavailable")
				}
				return Succeeded(taskSvc.AddEdge(a.From, a.To))
			},
		}
	case "task_remove_dep":
		return ToolDefinition{
			Name:        "task_remove_dep",
			Description: "Remove a DAG edge. Use numeric task IDs from task_create (never 0).",
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"from", "to"},
				"properties": map[string]any{
					"from": map[string]any{"type": "integer", "minimum": 1},
					"to":   map[string]any{"type": "integer", "minimum": 1},
				},
			}),
			RiskLevel: RiskSafe,
			Effects:   Effects(EffectSessionMutation),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct{ From, To int }
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if taskSvc == nil {
					return Failed("task service unavailable")
				}
				return Succeeded(taskSvc.RemoveEdge(a.From, a.To))
			},
		}
	case "task_ready":
		return ToolDefinition{
			Name:        "task_ready",
			Description: "List tasks whose DAG predecessors are completed (ready to execute). Pair with task_dag after task_create. See task_create for planning guidance.",
			RiskLevel:   RiskAuto,
			Effects:     Effects(),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				if taskSvc == nil {
					return Failed("task service unavailable")
				}
				return Succeeded(taskSvc.ReadyTasks())
			},
		}
	case "task_dag":
		return ToolDefinition{
			Name:        "task_dag",
			Description: "Show topological execution order for the task DAG. Call after creating multiple tasks/deps before executing. See task_create for when to use the DAG family.",
			RiskLevel:   RiskAuto,
			Effects:     Effects(),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				if taskSvc == nil {
					return Failed("task service unavailable")
				}
				return Succeeded(taskSvc.TopoView())
			},
		}
	case "claim_task":
		return ToolDefinition{
			Name:        "claim_task",
			Description: "Claim a board task by numeric task_id from task_create (never 0). See task_create for ID conventions.",
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"task_id"},
				"properties": map[string]any{"task_id": map[string]any{"type": "integer", "minimum": 1}},
			}),
			RiskLevel: RiskSafe,
			Effects:   Effects(EffectSessionMutation),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct {
					TaskID int `json:"task_id"`
				}
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if taskSvc != nil {
					msg, _ := taskSvc.Claim(a.TaskID, scope.AgentID)
					return Succeeded(msg)
				}
				return Failed("task service unavailable")
			},
		}
	}
	return ToolDefinition{}
}
