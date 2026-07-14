package agent

import (
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/logging"
	"strings"
)

// System prompt assembly + memory auto-recall.

// BuildSystemPrompt assembles the system prompt with dynamic sections:
// evergreen memory and DAG resume context.
func BuildSystemPrompt() string {
	raw := App.PromptLoader.Load("system")
	if raw == "" {
		logging.PrintSystem("ERROR: prompts/system.md not found, using minimal fallback")
		raw = "You are a coding agent. Use tools to solve tasks."
	}
	prompt := strings.Replace(strings.Replace(raw,
		"{{workdir}}", App.Workdir, 1),
		"{{skills}}", App.Skills.Descriptions(), 1)

	// Inject evergreen memory (truncated to prevent prompt bloat).
	if eg := App.MemStore.LoadEvergreen(); eg != "" {
		if len(eg) > infra.MaxEvergreenChars {
			cut := strings.LastIndex(eg[:infra.MaxEvergreenChars], "\n")
			if cut <= 0 {
				cut = infra.MaxEvergreenChars
			}
			eg = eg[:cut] + fmt.Sprintf("\n\n[... truncated, %d/%d chars shown. Consider cleaning up MEMORY.md.]", cut, len(eg))
		}
		prompt += "\n\n## Evergreen Memory\n\n" + eg
	}

	// Inject task resume context (unfinished DAG tasks from previous session).
	if rc := App.DagSched().ResumeContext(); rc != "" {
		prompt += "\n\n" + rc
	}

	// Inject MCP server instructions so the model knows what each server does.
	if App.MCPMgr != nil {
		if instructions := App.MCPMgr.ServerInstructions(); instructions != "" {
			prompt += "\n\n## MCP Server Instructions\n\n" + instructions
		}
	}

	return prompt
}
