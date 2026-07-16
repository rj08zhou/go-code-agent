package prompt

import (
	"io/fs"
	"strings"

	"go-code-agent/prompts"
)

// Package prompt provides prompt template loading and rendering.
//
// Prompt templates are system-level assets embedded in the binary
// (see prompts/embed.go). They are available regardless of the
// working directory the agent is launched from.

// Loader reads prompt templates from the embedded filesystem.
type Loader struct {
	fsys fs.FS
}

// NewLoader creates a Loader backed by the embedded prompt files.
func NewLoader() *Loader {
	return &Loader{fsys: prompts.FS}
}

// Load reads a prompt template named "{name}.md" from the embedded
// prompts. Returns empty string if the file doesn't exist.
func (l *Loader) Load(name string) string {
	if l == nil || l.fsys == nil {
		return ""
	}
	data, err := fs.ReadFile(l.fsys, name+".md")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// LoadOr is Load with a hardcoded fallback for functionally important prompts.
func (l *Loader) LoadOr(name, fallback string) string {
	if got := l.Load(name); got != "" {
		return got
	}
	return fallback
}

// Render performs {{key}} substitution on a template string.
func Render(tmpl string, vars map[string]string) string {
	if len(vars) == 0 {
		return tmpl
	}
	out := tmpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}
