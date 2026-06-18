package main

import (
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/log"
	"strings"
)

// System prompt assembly + memory auto-recall.

// buildSystemPrompt assembles the system prompt with dynamic sections:
// evergreen memory and DAG resume context.
//
// Note: per-turn semantic recall was intentionally removed. Evergreen
// MEMORY.md is loaded once at session start (resident), and relevant
// daily memories are surfaced on-demand by the model via the
// `memory_search` tool — mirroring how Claude Code / Cursor / CodeBuddy
// avoid blindly re-recalling on every user turn.
func buildSystemPrompt() string {
	raw := app.PromptLoader.Load("system")
	if raw == "" {
		log.PrintSystem("ERROR: prompts/system.md not found, using minimal fallback")
		raw = "You are a coding agent. Use tools to solve tasks."
	}
	prompt := strings.Replace(strings.Replace(raw,
		"{{workdir}}", workdir, 1),
		"{{skills}}", skills.Descriptions(), 1)

	// Inject evergreen memory (truncated to prevent prompt bloat).
	if eg := memStore.LoadEvergreen(); eg != "" {
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
	if rc := app.DagSched().ResumeContext(); rc != "" {
		prompt += "\n\n" + rc
	}

	return prompt
}
