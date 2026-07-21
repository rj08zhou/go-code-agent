package tool

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

func parseJSON(raw json.RawMessage, v any) string {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, v); err == nil {
		return ""
	}
	// Lenient fallback: LLMs often emit numeric fields as strings
	// (e.g. {"task_id":"1"}). Coerce numeric-looking string values to numbers
	// and retry.
	if relaxed, relaxErr := coerceNumericStrings(raw); relaxErr == nil {
		if err2 := json.Unmarshal(relaxed, v); err2 == nil {
			return ""
		}
	}
	return "invalid arguments"
}

func coerceNumericStrings(raw json.RawMessage) (json.RawMessage, error) {
	var m any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	coerced := walkAndCoerce(m)
	out, err := json.Marshal(coerced)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

func walkAndCoerce(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = walkAndCoerce(vv)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, vv := range val {
			out[i] = walkAndCoerce(vv)
		}
		return out
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return i
		}
		if f, err := val.Float64(); err == nil {
			return f
		}
		return val.String()
	case string:
		// LLMs often replicate "#1" notation from task_create output.
		c := strings.TrimLeft(val, "#")
		if i, err := strconv.ParseInt(c, 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(c, 64); err == nil {
			return f
		}
		return val
	default:
		return val
	}
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

func makeMemoryTool(name string, memSvc MemoryService) ToolDefinition {
	switch name {
	case "memory_write":
		return ToolDefinition{
			Name:        "memory_write",
			Description: "Save a fact to long-term memory.",
			RiskLevel:   RiskSafe,
			Effects:     Effects(EffectMemoryMutation),
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"content"},
				"properties": map[string]any{
					"content":  map[string]any{"type": "string", "description": "Fact or knowledge to store."},
					"category": map[string]any{"type": "string", "description": "Optional category (default: 'fact')."},
				},
			}),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct {
					Content  string `json:"content"`
					Category string `json:"category"`
				}
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if a.Category == "" {
					a.Category = "fact"
				}
				if memSvc != nil {
					return Succeeded(memSvc.Write(a.Content, a.Category))
				}
				return Failed("memory service unavailable")
			},
		}
	case "memory_search":
		return ToolDefinition{
			Name:        "memory_search",
			Description: "Search stored memories.",
			RiskLevel:   RiskAuto,
			Effects:     Effects(),
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"query"},
				"properties": map[string]any{
					"query":       map[string]any{"type": "string", "description": "Search query."},
					"top_k":       map[string]any{"type": "integer", "description": "Max results."},
					"within_days": map[string]any{"type": "integer", "description": "Days to look back."},
				},
			}),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct {
					Query      string `json:"query"`
					TopK       int    `json:"top_k"`
					WithinDays int    `json:"within_days"`
					Category   string `json:"category"`
				}
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if a.TopK <= 0 {
					a.TopK = 5
				}
				if memSvc != nil {
					return Succeeded(memSvc.Search(a.Query, a.TopK, a.WithinDays, a.Category))
				}
				return Failed("memory service unavailable")
			},
		}
	case "memory_delete":
		return ToolDefinition{
			Name:        "memory_delete",
			Description: "Delete a memory entry.",
			RiskLevel:   RiskSafe,
			Effects:     Effects(EffectMemoryMutation),
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"query"},
				"properties": map[string]any{
					"query":    map[string]any{"type": "string", "description": "Search query to match memories to delete."},
					"category": map[string]any{"type": "string", "description": "Optional category filter."},
				},
			}),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct {
					Query    string `json:"query"`
					Category string `json:"category"`
				}
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if memSvc != nil {
					return Succeeded(memSvc.Delete(a.Query, a.Category))
				}
				return Failed("memory service unavailable")
			},
		}
	case "memory_stats":
		return ToolDefinition{
			Name:        "memory_stats",
			Description: "Show memory store statistics.",
			RiskLevel:   RiskAuto,
			Effects:     Effects(),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				if memSvc != nil {
					return Succeeded(memSvc.Stats())
				}
				return Failed("memory service unavailable")
			},
		}
	case "session_save_memory":
		return ToolDefinition{
			Name: "session_save_memory", Description: "Save a session summary to long-term memory.",
			RiskLevel: RiskSafe, Effects: Effects(EffectMemoryMutation),
			Schema: MustMarshalJSON(map[string]any{
				"type": "object", "required": []string{"summary"},
				"properties": map[string]any{
					"summary": map[string]any{"type": "string", "description": "Session summary text to persist."},
				},
			}),
			Handler: func(scope *ToolScope, args json.RawMessage) Result {
				var a struct {
					Summary string `json:"summary"`
				}
				if e := parseJSON(args, &a); e != "" {
					return Failed(e)
				}
				if a.Summary == "" {
					return Failed("summary is required")
				}
				if memSvc == nil {
					return Failed("memory service unavailable")
				}
				return Succeeded(memSvc.SaveSessionMemory(scope.SessionID, a.Summary))
			},
		}
	}
	return ToolDefinition{}
}
