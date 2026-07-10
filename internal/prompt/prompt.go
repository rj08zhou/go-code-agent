package prompt

import (
	"os"
	"path/filepath"
	"strings"
)

// Package prompt provides prompt template loading and rendering.
//
// Prompt templates live in {workdir}/prompts/{name}.md. This package
// is dependency-free (no imports from internal/*) so any sub-package
// can use it without creating circular imports.

// Loader holds the directory path for prompt templates.
type Loader struct {
	dir string
}

// NewLoader creates a Loader rooted at the given directory.
func NewLoader(promptsDir string) *Loader {
	return &Loader{dir: promptsDir}
}

// Load reads a prompt template from {dir}/{name}.md.
// Returns empty string if the file doesn't exist.
func (l *Loader) Load(name string) string {
	if l == nil || l.dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(l.dir, name+".md"))
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
