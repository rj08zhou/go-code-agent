package agent

import (
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/skill"
)

// SystemPromptVars are the placeholders filled into system.md.
// EnvContext is usually produced by buildEnvContext; tests pass a fixed value
// for golden snapshots.
type SystemPromptVars struct {
	Workdir      string
	Skills       string
	EnvContext   string
	SkillContext string
}

// SystemPromptBuilder constructs the static system prompt.
// Dynamic state (evergreen memory, tasks, MCP) is injected per Run via
// BuildSessionContext — not interpolated here — so the system prefix stays
// stable for prompt caching.
type SystemPromptBuilder struct {
	promptLoader *prompt.Loader
	skillLoader  *skill.Loader
	embedded     []byte
}

func NewSystemPromptBuilder(
	pl *prompt.Loader,
	sl *skill.Loader,
	embedded []byte,
) *SystemPromptBuilder {
	return &SystemPromptBuilder{
		promptLoader: pl,
		skillLoader:  sl,
		embedded:     embedded,
	}
}

func (b *SystemPromptBuilder) Build(workdir string) string {
	skillCtx := ""
	skillNames := ""
	if b.skillLoader != nil && b.skillLoader.Len() > 0 {
		skillCtx = b.skillLoader.Summaries()
		skillNames = b.skillLoader.Names()
	}
	return b.BuildWith(SystemPromptVars{
		Workdir:      workdir,
		Skills:       skillNames,
		EnvContext:   buildEnvContext(workdir),
		SkillContext: skillCtx,
	})
}

// BuildWith renders the system template from explicit vars (used by golden tests).
func (b *SystemPromptBuilder) BuildWith(v SystemPromptVars) string {
	result := prompt.Render(b.promptLoader.MustLoad("system"), map[string]string{
		"workdir":       v.Workdir,
		"skills":        v.Skills,
		"env_context":   v.EnvContext,
		"skill_context": v.SkillContext,
	})
	if len(b.embedded) > 0 {
		result += "\n\n## Project Documentation\n" + string(b.embedded)
	}
	return result
}
