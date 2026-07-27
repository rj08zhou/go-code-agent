// Package prompt loads and renders prompt templates.
package prompt

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

//go:embed templates/*.md
var templateFS embed.FS

// placeholderRE matches {{key}} tokens (letters, digits, underscore).
var placeholderRE = regexp.MustCompile(`\{\{[a-zA-Z0-9_]+\}\}`)

// Loader renders prompt templates loaded from the embedded templates directory.
type Loader struct {
	templates map[string]string
}

// NewLoader loads every *.md file under templates/ into a name→content map.
// The template name is the file basename without the .md suffix.
func NewLoader() *Loader {
	l := &Loader{templates: make(map[string]string)}
	entries, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		panic(fmt.Sprintf("prompt: read embedded templates: %v", err))
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		data, err := templateFS.ReadFile(path.Join("templates", e.Name()))
		if err != nil {
			panic(fmt.Sprintf("prompt: read template %s: %v", e.Name(), err))
		}
		l.templates[name] = string(data)
	}
	return l
}

// Load returns the template body for name, or "" if unknown.
func (l *Loader) Load(name string) string {
	if l == nil {
		return ""
	}
	return l.templates[name]
}

// MustLoad returns the template body or panics if missing/empty.
func (l *Loader) MustLoad(name string) string {
	t := l.Load(name)
	if strings.TrimSpace(t) == "" {
		panic(fmt.Sprintf("prompt: required template %q is missing or empty", name))
	}
	return t
}

// Names returns sorted template names (for tests and diagnostics).
func (l *Loader) Names() []string {
	names := make([]string, 0, len(l.templates))
	for n := range l.templates {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Count returns how many templates are loaded.
func (l *Loader) Count() int {
	if l == nil {
		return 0
	}
	return len(l.templates)
}

// EmbeddedCount returns the number of *.md files in the embed FS.
func EmbeddedCount() int {
	entries, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
}

// UnreplacedPlaceholders returns distinct {{key}} tokens still present in s.
func UnreplacedPlaceholders(s string) []string {
	matches := placeholderRE.FindAllString(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	var out []string
	for _, m := range matches {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// Render replaces {{key}} placeholders with values in sorted key order.
//
// Validation runs against the template's own placeholder set BEFORE
// substitution: it panics if the template declares a placeholder with no
// matching data key, so template typos cannot silently reach the model.
// Runtime data values are never scanned, so user-supplied text containing
// "{{...}}" (e.g. HITL feedback) can never trigger a panic.
func Render(tmpl string, data map[string]string) string {
	var missing []string
	for _, ph := range UnreplacedPlaceholders(tmpl) {
		key := strings.TrimSuffix(strings.TrimPrefix(ph, "{{"), "}}")
		if _, ok := data[key]; !ok {
			missing = append(missing, ph)
		}
	}
	if len(missing) > 0 {
		panic(fmt.Sprintf("prompt: missing data for placeholders: %s", strings.Join(missing, ", ")))
	}

	// Single-pass substitution: only tokens present in the original template
	// are replaced, so values containing "{{...}}" are emitted verbatim and
	// never re-substituted.
	return placeholderRE.ReplaceAllStringFunc(tmpl, func(ph string) string {
		key := strings.TrimSuffix(strings.TrimPrefix(ph, "{{"), "}}")
		return data[key]
	})
}
