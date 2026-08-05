package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/config"
	"go-code-agent/internal/prompt"
)

func TestLoadProjectDocumentationPrefersAGENT(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("from claude"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte("from agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadProjectDocumentation(dir)
	if string(got) != "from agent" {
		t.Fatalf("got %q, want AGENT.md content", got)
	}
}

func TestLoadProjectDocumentationFallsBackToCLAUDE(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("from claude"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadProjectDocumentation(dir)
	if string(got) != "from claude" {
		t.Fatalf("got %q, want CLAUDE.md content", got)
	}
}

func TestLoadProjectDocumentationMissing(t *testing.T) {
	if got := LoadProjectDocumentation(t.TempDir()); got != nil {
		t.Fatalf("got %q, want nil", got)
	}
	if got := LoadProjectDocumentation(""); got != nil {
		t.Fatalf("empty workdir: got %q, want nil", got)
	}
}

func TestLoadProjectDocumentationTruncates(t *testing.T) {
	dir := t.TempDir()
	// Use multi-byte runes so byte length ≠ rune length.
	body := strings.Repeat("项", config.MaxProjectDocChars+10)
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadProjectDocumentation(dir)
	if !strings.HasSuffix(string(got), "…(truncated)") {
		t.Fatalf("expected truncation marker, got len=%d", len(got))
	}
	without := strings.TrimSuffix(string(got), "\n\n…(truncated)")
	if n := len([]rune(without)); n != config.MaxProjectDocChars {
		t.Fatalf("rune count %d, want %d", n, config.MaxProjectDocChars)
	}
}

func TestSystemPromptIncludesProjectInstructions(t *testing.T) {
	b := NewSystemPromptBuilder(prompt.NewLoader(), nil, []byte("use go test ./..."))
	got := b.BuildWith(SystemPromptVars{
		Workdir:    "/tmp/ws",
		Skills:     "none",
		EnvContext: "<env>\nok\n</env>",
	})
	if !strings.Contains(got, "## Project Instructions\nuse go test ./...") {
		t.Fatalf("project instructions missing:\n%s", got)
	}
}
