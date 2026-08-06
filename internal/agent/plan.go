package agent

import (
	"fmt"
	"go-code-agent/internal/config"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/security"
	"go-code-agent/internal/task"
	"go-code-agent/internal/tool"
	"strings"
)

// PlanGate nudges non-trivial runs to establish an explicit plan.
// It is injected into the runner and evaluated at specific rounds.
type PlanGate struct {
	promptLoader *prompt.Loader
	taskSvc      *task.Service
}

func NewPlanGate(pl *prompt.Loader, ts *task.Service) *PlanGate {
	return &PlanGate{promptLoader: pl, taskSvc: ts}
}

// Eval returns a nudge message to inject and a policy action for observability
// ("" / "" if nothing). Called by the runner every turn with the latest
// state snapshot.
func (g *PlanGate) Eval(toolRounds int, planEstablished bool, originalTask string) (nudge, action string) {
	// --- Phase 1: round-0 planning nudge ---
	if toolRounds == 0 && needsPlan(originalTask, planEstablished) {
		return g.promptLoader.MustLoad("planning_required"), "require_plan"
	}

	// --- Phase 2: round-1 DAG nudge ---
	if msg := g.checkDAGDependency(toolRounds); msg != "" {
		return msg, "require_dag_edges"
	}
	return "", ""
}

func needsPlan(originalTask string, planEstablished bool) bool {
	return !planEstablished && !isTrivialQuery(originalTask)
}

func unplannedToolBlock(toolName string, definition tool.ToolDefinition, known bool, arguments string) (classification string, blocked bool) {
	switch {
	case !known:
		return "unknown_tool", true
	case !definition.Effects.Declared(), definition.HasEffect(tool.EffectUnclassified):
		return "unclassified_effects", true
	case definition.HasEffect(tool.EffectWriteFile), definition.HasEffect(tool.EffectDeleteFile):
		return "file_mutation", true
	case definition.HasEffect(tool.EffectExecuteProcess):
		// A shell tool can execute anything, but the gate exists to stop
		// unplanned *side effects*. Inspection-only commands (ls, grep,
		// git status) produce none, and forcing a plan before the agent may
		// even look around defeats the purpose of planning.
		if isReadOnlyShellCall(toolName, definition, arguments) {
			return "", false
		}
		return "process_execution", true
	case definition.HasEffect(tool.EffectDelegation):
		// Spawning a teammate or approving its plan hands side-effect
		// capability to another agent; that must not bypass the lead's
		// own planning gate.
		return "delegation", true
	default:
		return "", false
	}
}

// isReadOnlyShellCall reports whether a shell tool call is inspection-only.
// It fails closed: anything other than a recognised shell tool carrying a
// command that security.ClassifyCommand rates VerdictSafe is not read-only.
func isReadOnlyShellCall(toolName string, definition tool.ToolDefinition, arguments string) bool {
	if !security.IsShellTool(toolName) {
		return false
	}
	// A shell tool that also declares mutating effects is judged by those
	// effects, not by the command text.
	for _, e := range []tool.Effect{
		tool.EffectWriteFile, tool.EffectDeleteFile,
		tool.EffectNetworkAccess, tool.EffectDelegation,
	} {
		if definition.HasEffect(e) {
			return false
		}
	}
	command := strings.TrimSpace(extractBashCommand(arguments))
	if command == "" {
		return false
	}
	return security.ClassifyCommand(command).Verdict == security.VerdictSafe
}

func (g *PlanGate) checkDAGDependency(toolRounds int) string {
	if toolRounds != 1 {
		return ""
	}
	if g.taskSvc == nil {
		return ""
	}
	taskCount := g.taskSvc.TaskCount()
	edgeCount := g.taskSvc.EdgeCount()
	if taskCount > 1 && edgeCount == 0 {
		return prompt.Render(g.promptLoader.MustLoad("dag_required"), map[string]string{
			"count": fmt.Sprintf("%d", taskCount),
		})
	}
	return ""
}

func isTrivialQuery(task string) bool {
	t := strings.TrimSpace(task)
	if t == "" {
		return true
	}
	if len(t) >= config.PlanningGateMinTaskChars {
		return false
	}
	if strings.Contains(t, "\n") {
		return false
	}
	lower := strings.ToLower(t)
	for _, k := range []string{
		"implement", "refactor", "build", "design", "fix", "write", "edit",
		"modify", "update", "delete", "remove", "rename", "create", "install",
		"deploy", "migrate", "rewrite", "patch", "purge", "clear", "add ",
		"change ", "move ", "重构", "实现", "修复", "设计", "修改", "编辑",
		"新增", "添加", "创建", "删除", "移除", "重命名", "移动", "安装",
		"清空", "清除", "写", "改",
	} {
		if strings.Contains(lower, k) {
			return false
		}
	}
	return true
}
