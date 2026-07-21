package agent

import (
	"go-code-agent-refactor/internal/memory"
	"go-code-agent-refactor/internal/prompt"
	"go-code-agent-refactor/internal/skill"
	"go-code-agent-refactor/internal/task"
	"strings"
)

// SystemPromptBuilder constructs the system prompt from context.
type SystemPromptBuilder struct {
	promptLoader *prompt.Loader
	skillLoader  *skill.Loader
	memStore     *memory.Store
	taskSvc      *task.Service
	mcpListFn    func() string
	embedded     []byte
}

func NewSystemPromptBuilder(
	pl *prompt.Loader,
	sl *skill.Loader,
	ms *memory.Store,
	ts *task.Service,
	mcpFn func() string,
	embedded []byte,
) *SystemPromptBuilder {
	return &SystemPromptBuilder{
		promptLoader: pl,
		skillLoader:  sl,
		memStore:     ms,
		taskSvc:      ts,
		mcpListFn:    mcpFn,
		embedded:     embedded,
	}
}

func (b *SystemPromptBuilder) Build(workdir string) string {
	tmpl := b.promptLoader.Load("system")

	memoryCtx := ""
	if b.memStore != nil {
		memoryCtx = b.memStore.GetEvergreen()
	}

	skillCtx := ""
	if b.skillLoader != nil && b.skillLoader.Len() > 0 {
		skillCtx = b.skillLoader.All()
	}

	taskCtx := ""
	if b.taskSvc != nil {
		taskCtx = b.taskSvc.ProgressSummary()
	}

	mcpCtx := ""
	if b.mcpListFn != nil {
		mcpCtx = b.mcpListFn()
	}

	result := strings.NewReplacer(
		"{{memory_context}}", memoryCtx,
		"{{skill_context}}", skillCtx,
		"{{task_context}}", taskCtx,
		"{{mcp_context}}", mcpCtx,
	).Replace(tmpl)

	if len(b.embedded) > 0 {
		result += "\n\n## Project Documentation\n" + string(b.embedded)
	}

	return result
}
