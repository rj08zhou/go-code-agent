package main

import (
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/log"
	"strings"
)

// System prompt assembly + memory auto-recall.

// buildSystemPrompt assembles the system prompt with dynamic sections:
// evergreen memory, DAG resume context, and auto-recalled memories.
func buildSystemPrompt(memoryContext string) string {
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

	// Inject auto-recalled memories.
	if memoryContext != "" {
		prompt += "\n\n## Recalled Memories\n\n" + memoryContext
	}
	return prompt
}

// memoryRecall searches memory for context relevant to the user's message.
func memoryRecall(userMessage string) string {
	results := memStore.HybridSearch(userMessage, 3)
	if len(results) == 0 {
		return ""
	}
	log.PrintSystem("[auto-recall] Found relevant memories")
	var lines []string
	for _, r := range results {
		lines = append(lines, fmt.Sprintf("- [%s] %s", r.Path, r.Snippet))
	}
	return strings.Join(lines, "\n")
}
