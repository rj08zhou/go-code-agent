package tool

import "encoding/json"

func memoryTools(d builtinDeps) []ToolDefinition {
	return []ToolDefinition{
		makeMemoryTool("memory_write", d.memorySvc),
		makeMemoryTool("memory_search", d.memorySvc),
		makeMemoryTool("memory_delete", d.memorySvc),
		makeMemoryTool("memory_stats", d.memorySvc),
		makeMemoryTool("session_save_memory", d.memorySvc),
	}
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
