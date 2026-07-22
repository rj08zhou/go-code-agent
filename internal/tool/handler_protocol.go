package tool

import (
	"encoding/json"
)

func protocolTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:        "shutdown_request",
		Description: "Request a teammate to stop its autonomous work loop.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"teammate"},
			"properties": map[string]any{
				"teammate": map[string]any{"type": "string", "description": "Teammate name to shut down."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Teammate string `json:"teammate"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Teammate == "" {
				return Failed("teammate is required")
			}
			if d.protocolSvc == nil {
				return Failed("team protocol unavailable")
			}
			return Succeeded(d.protocolSvc.ShutdownRequest(a.Teammate))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "plan_approval",
		Description: "Approve or reject a teammate plan by request ID.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"request_id", "approve"},
			"properties": map[string]any{
				"request_id": map[string]any{"type": "string", "description": "Plan request ID from inbox."},
				"approve":    map[string]any{"type": "boolean", "description": "true = approve, false = reject."},
				"feedback":   map[string]any{"type": "string", "description": "Optional feedback for the teammate."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				RequestID string `json:"request_id"`
				Approve   bool   `json:"approve"`
				Feedback  string `json:"feedback"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.RequestID == "" {
				return Failed("request_id is required")
			}
			if scope == nil || scope.Role != "lead" {
				return Denied("only the lead may approve plans")
			}
			if d.protocolSvc == nil {
				return Failed("team protocol unavailable")
			}
			return Succeeded(d.protocolSvc.ReviewPlan(a.RequestID, a.Approve, a.Feedback))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "submit_plan",
		Description: "Submit a teammate plan for lead approval before mutations.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"plan"},
			"properties": map[string]any{
				"plan": map[string]any{"type": "string", "description": "Plan description for lead review."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Plan string `json:"plan"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Plan == "" {
				return Failed("plan is required")
			}
			if d.protocolSvc == nil {
				return Failed("team protocol unavailable")
			}
			return Succeeded(d.protocolSvc.SubmitPlan(scope.AgentID, a.Plan))
		},
	})

	return defs
}
