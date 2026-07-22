package tool

import (
	"encoding/json"
)

func metaTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:        "compress",
		Description: "Manually compress conversation context.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			return Succeeded("Compressing...")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "load_skill",
		Description: "Load specialized knowledge by name.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Skill name to load."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Name string `json:"name"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if d.skillLoader != nil {
				return Succeeded(d.skillLoader.Load(a.Name))
			}
			return Failed("no skills loaded")
		},
	})

	return defs
}
