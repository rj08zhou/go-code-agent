// Package prompt loads and renders prompt templates.
package prompt

import (
	_ "embed"
	"strings"
)

//go:embed templates/system.md
var systemTemplate string

//go:embed templates/auto_lesson.md
var autoLessonTemplate string

//go:embed templates/session_to_memory.md
var sessionToMemoryTemplate string

//go:embed templates/judge_critical.md
var judgeCriticalTemplate string

// Loader renders prompt templates.
type Loader struct {
	templates map[string]string
}

func NewLoader() *Loader {
	l := &Loader{
		templates: map[string]string{
			"system":            systemTemplate,
			"auto_lesson":       autoLessonTemplate,
			"session_to_memory": sessionToMemoryTemplate,
			"judge_critical":    judgeCriticalTemplate,
		},
	}
	return l
}

func (l *Loader) Load(name string) string {
	if t, ok := l.templates[name]; ok {
		return t
	}
	return ""
}

// Render replaces {{key}} placeholders with values.
func Render(tmpl string, data map[string]string) string {
	result := tmpl
	for k, v := range data {
		result = strings.ReplaceAll(result, "{{"+k+"}}", v)
	}
	return result
}
