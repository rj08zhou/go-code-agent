package tool

import (
	"encoding/json"
)

func taskTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:        "TodoWrite",
		Description: "Update task tracking list.",
		RiskLevel:   RiskSafe,
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
			Name:        "task_create",
			Description: "Create a persistent task with optional DAG dependencies. The returned task ID is authoritative; use it in later task_update calls.",
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
			Name:        "task_get",
			Description: "Get task details by ID. Use the numeric ID returned by task_create.",
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
			Name:        "task_update",
			Description: "Update a task's status. You MUST pass the numeric task_id returned by task_create; do not use 0 or omit it.",
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
			Description: "List all tasks.",
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
			Description: "Add a DAG dependency edge. Use numeric task IDs returned by task_create.",
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
			Description: "Remove a DAG dependency edge. Use numeric task IDs returned by task_create.",
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
			Description: "List tasks whose DAG predecessors are completed.",
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
			Description: "Show topological execution order.",
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
			Description: "Claim a task from the board. Use the numeric task ID returned by task_create.",
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
