package prompt

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestLoadEmptyDirReturnsEmpty(t *testing.T) {
	l := NewLoader("")
	if got := l.Load("system"); got != "" {
		t.Errorf("Load with empty dir = %q, want empty", got)
	}
}

func TestLoadNilReceiverReturnsEmpty(t *testing.T) {
	var l *Loader
	if got := l.Load("system"); got != "" {
		t.Errorf("Load on nil receiver = %q, want empty", got)
	}
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	l := NewLoader(dir)
	if got := l.Load("does-not-exist"); got != "" {
		t.Errorf("Load missing file = %q, want empty", got)
	}
}

func TestLoadReadsAndTrims(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sys.md"), []byte("  line one  \n\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	l := NewLoader(dir)
	if got := l.Load("sys"); got != "line one" {
		t.Errorf("Load = %q, want trimmed %q", got, "line one")
	}
}

// --- Loader.LoadOr ---------------------------------------------------------

func TestLoadOrUsesTemplateWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.md"), []byte("from-disk"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	l := NewLoader(dir)
	if got := l.LoadOr("p", "fallback"); got != "from-disk" {
		t.Errorf("LoadOr = %q, want template content", got)
	}
}

func TestLoadOrUsesFallbackWhenMissing(t *testing.T) {
	dir := t.TempDir()
	l := NewLoader(dir)
	if got := l.LoadOr("missing", "fallback-text"); got != "fallback-text" {
		t.Errorf("LoadOr = %q, want fallback", got)
	}
}

func TestLoadOrEmptyFallbackWhenMissing(t *testing.T) {
	dir := t.TempDir()
	l := NewLoader(dir)
	if got := l.LoadOr("missing", ""); got != "" {
		t.Errorf("LoadOr empty fallback = %q, want empty", got)
	}
}
