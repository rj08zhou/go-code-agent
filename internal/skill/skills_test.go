package skill

import (
	"os"
	"path/filepath"
	"testing"
)

// --- parseFrontmatter ------------------------------------------------------

func TestParseFrontmatterSingleLine(t *testing.T) {
	m := parseFrontmatter("name: foo\ndescription: does a thing")
	if m["name"] != "foo" {
		t.Errorf("name = %q, want foo", m["name"])
	}
	if m["description"] != "does a thing" {
		t.Errorf("description = %q, want 'does a thing'", m["description"])
	}
}

func TestParseFrontmatterMultilineFold(t *testing.T) {
	raw := "name: foo\ndescription: |\n  line 1\n  line 2\n  line 3"
	m := parseFrontmatter(raw)
	want := "line 1\nline 2\nline 3"
	if m["description"] != want {
		t.Errorf("description = %q, want %q", m["description"], want)
	}
}

func TestParseFrontmatterMultilineThenNewKey(t *testing.T) {
	raw := "name: foo\ndescription: |\n  multi\n  line\nversion: 2"
	m := parseFrontmatter(raw)
	if m["description"] != "multi\nline" {
		t.Errorf("description = %q, want 'multi\\nline'", m["description"])
	}
	if m["version"] != "2" {
		t.Errorf("version = %q, want 2", m["version"])
	}
}

func TestParseFrontmatterKeyWithSpaceIgnored(t *testing.T) {
	// "key with space:" is not a valid key (contains space), so the colon
	// inside shouldn't start a new field.
	raw := "name: foo\nfoo bar: baz"
	m := parseFrontmatter(raw)
	if _, ok := m["foo bar"]; ok {
		t.Errorf("key with space should not be treated as a field, got %v", m)
	}
	if m["name"] != "foo" {
		t.Errorf("name = %q, want foo", m["name"])
	}
}

func TestParseFrontmatterEmpty(t *testing.T) {
	if m := parseFrontmatter(""); len(m) != 0 {
		t.Errorf("empty input should yield empty map, got %v", m)
	}
}

// --- SkillLoader -----------------------------------------------------------

func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	skDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skDir, 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

func TestSkillLoaderDiscoversAndDescribes(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "---\nname: alpha\ndescription: first skill\n---\nbody alpha")
	writeSkill(t, dir, "beta", "---\nname: beta\ndescription: second skill\n---\nbody beta")

	sl := NewSkillLoader(dir)
	if sl.Len() != 2 {
		t.Fatalf("Len = %d, want 2", sl.Len())
	}
	desc := sl.Descriptions()
	// Names sorted: alpha must appear before beta.
	aIdx := indexOf(desc, "alpha")
	bIdx := indexOf(desc, "beta")
	if aIdx < 0 || bIdx < 0 || aIdx > bIdx {
		t.Errorf("Descriptions not sorted as expected: %q", desc)
	}
	if !contains(desc, "first skill") || !contains(desc, "second skill") {
		t.Errorf("Descriptions missing skill text: %q", desc)
	}
}

func TestSkillLoaderLoadKnown(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "agent-builder", "---\nname: agent-builder\ndescription: x\n---\nTHIS IS THE BODY")
	sl := NewSkillLoader(dir)
	got := sl.Load("agent-builder")
	if got != `<skill name="agent-builder">`+"\nTHIS IS THE BODY\n</skill>" {
		t.Errorf("Load = %q", got)
	}
}

func TestSkillLoaderLoadUnknown(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "known", "---\nname: known\ndescription: x\n---\nbody")
	sl := NewSkillLoader(dir)
	got := sl.Load("nonexistent")
	if got != "Error: Unknown skill 'nonexistent'" {
		t.Errorf("Load unknown = %q", got)
	}
}

func TestSkillLoaderEmptyDescriptions(t *testing.T) {
	sl := NewSkillLoader(t.TempDir())
	if got := sl.Descriptions(); got != "(no skills)" {
		t.Errorf("empty Descriptions = %q, want '(no skills)'", got)
	}
}

// --- helpers ---------------------------------------------------------------

func contains(s, sub string) bool {
	return indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
