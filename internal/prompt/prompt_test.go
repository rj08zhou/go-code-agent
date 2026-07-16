package prompt

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// newTestLoader returns a Loader backed by an in-memory filesystem
// for testing, so individual tests can supply their own prompt content.
func newTestLoader(files map[string]string) *Loader {
	m := make(fstest.MapFS)
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return &Loader{fsys: m}
}

// --- Render ----------------------------------------------------------------

func TestRenderSubstitutesAllVars(t *testing.T) {
	tmpl := "Hello {{name}}, you are {{role}}."
	got := Render(tmpl, map[string]string{"name": "Yi", "role": "admin"})
	want := "Hello Yi, you are admin."
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestRenderNoVarsReturnsOriginal(t *testing.T) {
	tmpl := "static template with {{placeholder}}"
	got := Render(tmpl, nil)
	if got != tmpl {
		t.Errorf("Render with nil vars = %q, want %q", got, tmpl)
	}
}

func TestRenderEmptyVarsReturnsOriginal(t *testing.T) {
	tmpl := "static template with {{placeholder}}"
	got := Render(tmpl, map[string]string{})
	if got != tmpl {
		t.Errorf("Render with empty vars = %q, want %q", got, tmpl)
	}
}

func TestRenderRepeatsPlaceholder(t *testing.T) {
	tmpl := "{{x}} and {{x}} again"
	got := Render(tmpl, map[string]string{"x": "Z"})
	want := "Z and Z again"
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestRenderLeavesUnknownPlaceholders(t *testing.T) {
	tmpl := "known={{a}} unknown={{b}}"
	got := Render(tmpl, map[string]string{"a": "1"})
	want := "known=1 unknown={{b}}"
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

func TestRenderVarNameWithSpecialChars(t *testing.T) {
	tmpl := "{{key.name}}"
	got := Render(tmpl, map[string]string{"key.name": "v"})
	want := "v"
	if got != want {
		t.Errorf("Render = %q, want %q", got, want)
	}
}

// --- Loader.Load -----------------------------------------------------------

func TestLoadNilFSReturnsEmpty(t *testing.T) {
	l := &Loader{fsys: nil}
	if got := l.Load("system"); got != "" {
		t.Errorf("Load with nil fs = %q, want empty", got)
	}
}

func TestLoadNilReceiverReturnsEmpty(t *testing.T) {
	var l *Loader
	if got := l.Load("system"); got != "" {
		t.Errorf("Load on nil receiver = %q, want empty", got)
	}
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	l := NewLoader()
	if got := l.Load("does-not-exist"); got != "" {
		t.Errorf("Load missing file = %q, want empty", got)
	}
}

func TestLoadReadsAndTrims(t *testing.T) {
	l := newTestLoader(map[string]string{
		"sys.md": "  line one  \n\n",
	})
	if got := l.Load("sys"); got != "line one" {
		t.Errorf("Load = %q, want trimmed %q", got, "line one")
	}
}

func TestLoadEmbeddedSystemPrompt(t *testing.T) {
	l := NewLoader()
	got := l.Load("system")
	if got == "" {
		t.Fatal("system.md not found in embedded FS")
	}
	if !contains(got, "coding agent") {
		t.Errorf("system.md looks wrong: %s", got[:min(len(got), 80)])
	}
}

// --- Loader.LoadOr ---------------------------------------------------------

func TestLoadOrUsesTemplateWhenPresent(t *testing.T) {
	l := newTestLoader(map[string]string{
		"p.md": "from-test-fs",
	})
	if got := l.LoadOr("p", "fallback"); got != "from-test-fs" {
		t.Errorf("LoadOr = %q, want template content", got)
	}
}

func TestLoadOrUsesFallbackWhenMissing(t *testing.T) {
	l := NewLoader()
	if got := l.LoadOr("missing", "fallback-text"); got != "fallback-text" {
		t.Errorf("LoadOr = %q, want fallback", got)
	}
}

func TestLoadOrEmptyFallbackWhenMissing(t *testing.T) {
	l := NewLoader()
	if got := l.LoadOr("missing", ""); got != "" {
		t.Errorf("LoadOr empty fallback = %q, want empty", got)
	}
}

// helpers

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Check that the embedded FS actually contains the expected files.
func TestEmbeddedFSContainsAllPrompts(t *testing.T) {
	names := []string{
		"system.md",
		"think_required.md",
		"planning_required.md",
		"judge_system.md",
		"judge_critical.md",
		"teammate.md",
		"auto_lesson.md",
		"human_reject.md",
		"human_modify.md",
		"session_to_memory.md",
		"strategy_change.md",
		"todo_nag.md",
	}
	l := NewLoader()
	for _, name := range names {
		// strip .md suffix for Load
		key := name[:len(name)-3]
		if got := l.Load(key); got == "" {
			t.Errorf("embedded FS missing %s", name)
		}
	}
}

// Ensure fs.ReadFile is used (compile-time check via the fstest path).
var _ fs.FS = fstest.MapFS{}
