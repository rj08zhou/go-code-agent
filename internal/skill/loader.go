// Package skill loads SKILL.md files from the project directory.
package skill

import (
	"os"
	"path/filepath"
	"strings"
)

// Loader reads skill definitions from {projectDir}/skills/.
// It recursively walks subdirectories looking for SKILL.md files,
// matching the original project's behaviour.
type Loader struct {
	skills       map[string]string
	descriptions map[string]string
	names        []string
}

func NewLoader(skillsDir string) *Loader {
	l := &Loader{
		skills:       make(map[string]string),
		descriptions: make(map[string]string),
	}
	_ = filepath.Walk(skillsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.EqualFold(info.Name(), "SKILL.md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// Skill name = parent directory name (matching original convention).
		name := filepath.Base(filepath.Dir(path))
		if name == "." || name == "skills" {
			name = strings.TrimSuffix(strings.ToLower(info.Name()), ".md")
		}
		if _, exists := l.skills[name]; !exists {
			l.names = append(l.names, name)
		}
		l.skills[name] = string(data)
		l.descriptions[name] = parseDescription(string(data))
		return nil
	})
	return l
}

// parseDescription extracts the `description` field from a SKILL.md YAML
// frontmatter block. It supports both inline (`description: text`) and YAML
// block scalars (`description: |` followed by indented lines). Returns "" if
// no frontmatter/description is present.
func parseDescription(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	// Locate frontmatter bounds (first line is the opening ---).
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		end = len(lines)
	}
	for i := 1; i < end; i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "description:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "description:"))
		// Inline form: `description: some text`.
		if value != "" && value != "|" && value != ">" && value != "|-" && value != ">-" {
			return value
		}
		// Block scalar: collect subsequent more-indented lines.
		var parts []string
		for j := i + 1; j < end; j++ {
			bl := lines[j]
			if strings.TrimSpace(bl) == "" {
				continue
			}
			// Block content must be indented; a non-indented line ends it.
			if bl[0] != ' ' && bl[0] != '\t' {
				break
			}
			parts = append(parts, strings.TrimSpace(bl))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func (l *Loader) Load(name string) string {
	if content, ok := l.skills[name]; ok {
		return content
	}
	return "Skill '" + name + "' not found. Available: " + l.Names()
}

func (l *Loader) Names() string {
	return strings.Join(l.names, ", ")
}

func (l *Loader) Len() int {
	return len(l.skills)
}

// All returns all skill contents concatenated, in stable order.
func (l *Loader) All() string {
	var b strings.Builder
	for _, name := range l.names {
		b.WriteString("\n## Skill: " + name + "\n")
		b.WriteString(l.skills[name])
		b.WriteString("\n")
	}
	return b.String()
}

// Summaries returns a compact catalog of available skills (name + one-line
// description) instead of their full contents. The full body of a skill is
// loaded on demand via the load_skill tool, keeping the static system prompt
// small. Returns "" when there are no skills.
func (l *Loader) Summaries() string {
	if len(l.names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Available Skills\n")
	b.WriteString("Load a skill's full instructions with load_skill(name) only when relevant:\n")
	for _, name := range l.names {
		desc := l.descriptions[name]
		if desc == "" {
			b.WriteString("- " + name + "\n")
			continue
		}
		b.WriteString("- " + name + ": " + desc + "\n")
	}
	return b.String()
}
