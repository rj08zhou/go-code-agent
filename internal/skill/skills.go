package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SkillLoader - Layer 1 metadata in prompt, Layer 2 on demand
//
// SKILL.md files use YAML frontmatter with a simplified parser that
// supports the `|` multi-line string syntax:
//
//   description: |
//     line 1
//     line 2
//
// The parser folds continuation lines (indented by ≥2 spaces) into
// the current key until a line with less indentation is encountered.

type skillEntry struct {
	Meta map[string]string
	Body string
}

type SkillLoader struct {
	skills map[string]skillEntry
}

var frontmatterRe = regexp.MustCompile(`(?s)^---\n(.*?)\n---\n(.*)`)

// parseFrontmatter parses a YAML-style frontmatter block.
// It supports single-line `key: value` and multi-line `key: |` syntax.
// Continuation lines must be indented by at least 2 spaces.
func parseFrontmatter(raw string) map[string]string {
	meta := map[string]string{}
	var curKey string
	var curVal []string
	flush := func() {
		if curKey != "" {
			meta[curKey] = strings.TrimSpace(strings.Join(curVal, "\n"))
		}
	}

	for _, line := range strings.Split(raw, "\n") {
		// Check if this is a new key: value line.
		idx := strings.Index(line, ":")
		if idx >= 0 {
			// Make sure the part before ":" doesn't contain spaces (looks like a key).
			keyPart := strings.TrimSpace(line[:idx])
			if !strings.ContainsAny(keyPart, " \t") {
				flush()
				curKey = keyPart
				val := strings.TrimSpace(line[idx+1:])
				if val == "|" {
					// Multi-line string: start collecting continuation lines.
					curVal = []string{}
				} else {
					curVal = []string{val}
					curKey = "" // single-line: commit immediately
					meta[keyPart] = val
				}
				continue
			}
		}
		// Continuation line: append to current multi-line value.
		if curKey != "" && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			curVal = append(curVal, strings.TrimSpace(line))
		}
	}
	flush()
	return meta
}

func NewSkillLoader(dir string) *SkillLoader {
	sl := &SkillLoader{skills: make(map[string]skillEntry)}
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "SKILL.md" {
			return nil
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		meta := map[string]string{}
		body := string(text)
		if m := frontmatterRe.FindStringSubmatch(string(text)); m != nil {
			meta = parseFrontmatter(m[1])
			body = strings.TrimSpace(m[2])
		}
		name := meta["name"]
		if name == "" {
			name = filepath.Base(filepath.Dir(path))
		}
		sl.skills[name] = skillEntry{Meta: meta, Body: body}
		return nil
	})
	return sl
}

// Len returns the number of loaded skills. Useful for diagnostics.
func (sl *SkillLoader) Len() int { return len(sl.skills) }

func (sl *SkillLoader) Descriptions() string {
	if len(sl.skills) == 0 {
		return "(no skills)"
	}
	names := make([]string, 0, len(sl.skills))
	for n := range sl.skills {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	for _, n := range names {
		desc := sl.skills[n].Meta["description"]
		// Normalize: replace newlines with spaces for the one-line summary.
		oneLine := strings.Join(strings.Fields(desc), " ")
		if len(oneLine) > 300 {
			oneLine = oneLine[:297] + "..."
		}
		lines = append(lines, fmt.Sprintf("  - %s: %s", n, oneLine))
	}
	return strings.Join(lines, "\n")
}

func (sl *SkillLoader) Load(name string) string {
	s, ok := sl.skills[name]
	if !ok {
		return fmt.Sprintf("Error: Unknown skill '%s'", name)
	}
	return fmt.Sprintf("<skill name=%q>\n%s\n</skill>", name, s.Body)
}
