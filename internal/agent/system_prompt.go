package agent

import (
	"go-code-agent/internal/memory"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/skill"
	"go-code-agent/internal/task"
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

	// Inject only a compact catalog (name + one-line description) of skills.
	// The full body of each skill is loaded on demand via the load_skill tool,
	// so the large skill contents stay out of the static system prompt that is
	// re-sent (and re-billed on cache misses) every turn.
	skillCtx := ""
	skillNames := ""
	if b.skillLoader != nil && b.skillLoader.Len() > 0 {
		skillCtx = b.skillLoader.Summaries()
		skillNames = b.skillLoader.Names()
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
		"{{workdir}}", workdir,
		"{{skills}}", skillNames,
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
