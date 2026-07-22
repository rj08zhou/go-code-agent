package tool

import (
	"context"
	"encoding/json"
	"strings"
)

// parseTeamMemberNames extracts member names from the TeammateManager list output.
// The list format is: "Team: <name>\n  Alice (role): working\n  Bob (role): idle"
func parseTeamMemberNames(listOutput string) []string {
	lines := strings.Split(strings.TrimSpace(listOutput), "\n")
	var names []string
	for _, line := range lines {
		// Skip header line starting with "Team:"
		if strings.HasPrefix(strings.TrimSpace(line), "Team:") {
			continue
		}
		// Extract name: "  Alice (role): working" → "Alice"
		trimmed := strings.TrimSpace(line)
		if idx := strings.Index(trimmed, " ("); idx > 0 {
			names = append(names, trimmed[:idx])
		}
	}
	return names
}

func teamTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:        "spawn_teammate",
		Description: "Spawn a persistent autonomous teammate that runs in its own worktree. For code changes that need isolation. For read-only investigation, use explore instead.",
		RiskLevel:   RiskSafe,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"name", "prompt"},
			"properties": map[string]any{
				"name":   map[string]any{"type": "string", "description": "Unique name for this teammate."},
				"role":   map[string]any{"type": "string", "description": "Optional role hint (e.g. 'researcher', 'coder')."},
				"prompt": map[string]any{"type": "string", "description": "Task description for the teammate."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Name   string `json:"name"`
				Role   string `json:"role"`
				Prompt string `json:"prompt"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Name == "" || a.Prompt == "" {
				return Failed("name and prompt are required")
			}
			if d.teamSvc != nil {
				// Teammate lifetime is session-scoped: do not bind to the
				// current tool call context (Ctrl-C on one turn must not kill it).
				return Succeeded(d.teamSvc.Spawn(context.Background(), a.Name, a.Role, a.Prompt))
			}
			return Failed("team spawn unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "list_teammates",
		Description: "List all teammates and their statuses.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			if d.teamSvc != nil {
				return Succeeded(d.teamSvc.ListAll())
			}
			return Failed("team list unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "send_message",
		Description: "Send a message to another agent via their inbox.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"to", "content"},
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Recipient agent name."},
				"content": map[string]any{"type": "string", "description": "Message body."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				To      string `json:"to"`
				Content string `json:"content"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.To == "" || a.Content == "" {
				return Failed("to and content are required")
			}
			if d.bus != nil {
				return Succeeded(d.bus.Send(scope.AgentID, a.To, a.Content, "message", nil))
			}
			return Failed("message d.bus unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "read_inbox",
		Description: "Read and drain all messages from your inbox.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			if d.bus == nil {
				return Failed("message d.bus unavailable")
			}
			msgs := d.bus.ReadInbox(scope.AgentID)
			if len(msgs) == 0 {
				return Succeeded("[]")
			}
			data, _ := json.Marshal(msgs)
			return Succeeded(string(data))
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "broadcast",
		Description: "Send a message to all active teammates.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(EffectTeamMutation),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"content"},
			"properties": map[string]any{
				"content": map[string]any{"type": "string", "description": "Message to broadcast."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Content string `json:"content"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if a.Content == "" {
				return Failed("content is required")
			}
			if d.bus == nil || d.teamSvc == nil {
				return Failed("broadcast unavailable")
			}
			// Parse member names from list output
			recipients := parseTeamMemberNames(d.teamSvc.ListAll())
			if len(recipients) == 0 {
				return Succeeded("No teammates to broadcast to.")
			}
			return Succeeded(d.bus.Broadcast(scope.AgentID, a.Content, recipients))
		},
	})

	return defs
}
