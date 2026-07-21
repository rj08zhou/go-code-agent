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
	skills map[string]string
	names  []string
}

func NewLoader(skillsDir string) *Loader {
	l := &Loader{skills: make(map[string]string)}
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
		return nil
	})
	return l
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
