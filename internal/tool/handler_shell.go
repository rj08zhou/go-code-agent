package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"go-code-agent/internal/config"
	"go-code-agent/internal/security"
)

func shellTools(d builtinDeps) []ToolDefinition {
	var defs []ToolDefinition

	defs = append(defs, ToolDefinition{
		Name:        "bash",
		Description: "Execute a shell command. Runs with a hard 120s timeout.",
		RiskLevel:   RiskDanger,
		Effects:     Effects(EffectExecuteProcess),
		Timeout:     config.BashTimeout,
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"command"},
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command to execute."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Command string `json:"command"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			// d.perms is captured from session wiring (may be nil); hard policy
			// still applies. Reload updates the same *Permissions in place.
			bashPolicy := security.NewDefaultBashPolicy()
			allowed, needConfirm, reason := bashPolicy.Validate(a.Command, d.perms)
			if !allowed {
				return Denied("bash blocked: " + reason)
			}
			// Confirmation is handled centrally by Executor's HITL adapter.
			// BashPolicy only blocks commands that are never permitted.
			_ = needConfirm
			_ = reason
			execCtx, cancel := context.WithTimeout(scopeParentContext(scope), config.BashTimeout)
			defer cancel()
			cmd := exec.CommandContext(execCtx, "sh", "-c", a.Command)
			cmd.Dir = scope.Workdir
			output, err := cmd.CombinedOutput()
			if execCtx.Err() == context.DeadlineExceeded {
				return Failed("command timed out after " + config.BashTimeout.String())
			}
			out := strings.TrimSpace(string(output))
			if err != nil && out == "" {
				out = fmt.Sprintf("Error: %v", err)
			}
			if out == "" {
				out = "(no output)"
			}
			return Succeeded(out)
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "background_run",
		Description: "Run a shell command in the background.",
		RiskLevel:   RiskDanger,
		Effects:     Effects(EffectExecuteProcess),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"command"},
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "Shell command to run in background."},
				"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds (0 = no timeout)."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			bashPolicy := security.NewDefaultBashPolicy()
			allowed, needConfirm, reason := bashPolicy.Validate(a.Command, d.perms)
			if !allowed {
				return Denied("background command blocked: " + reason)
			}
			// Executor's HITL adapter handles confirmation for this danger tool.
			_ = needConfirm
			_ = reason
			if d.bgSvc != nil {
				return Succeeded(d.bgSvc.Run(scope.SessionID, a.Command, a.Timeout))
			}
			return Failed("background supervisor unavailable")
		},
	})

	defs = append(defs, ToolDefinition{
		Name:        "check_background",
		Description: "Check background task status.",
		RiskLevel:   RiskAuto,
		Effects:     Effects(),
		Schema: MustMarshalJSON(map[string]any{
			"type": "object", "required": []string{"task_id"},
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "Background task ID returned by background_run."},
			},
		}),
		Handler: func(scope *ToolScope, args json.RawMessage) Result {
			var a struct {
				TaskID string `json:"task_id"`
			}
			if e := parseJSON(args, &a); e != "" {
				return Failed(e)
			}
			if d.bgSvc != nil {
				return Succeeded(d.bgSvc.Check(a.TaskID))
			}
			return Failed("background supervisor unavailable")
		},
	})

	return defs
}
