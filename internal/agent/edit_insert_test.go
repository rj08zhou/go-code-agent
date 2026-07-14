package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/security"
)

// withAutoApproveAll turns on auto-approve for the duration of a test so
// that edit/insert tools run without interactive approval or diff
// preview (ShouldPreviewDiff() is false when AutoApproveAll is on).
func withAutoApproveAll(t *testing.T) (context.Context, string) {
	t.Helper()
	saved := security.GlobalApproval
	t.Cleanup(func() { security.GlobalApproval = saved })
	security.GlobalApproval.SetAutoApproveAll(true)

	dir := t.TempDir()
	App = &AppContext{Workdir: dir}
	return WithWorkdir(context.Background(), dir), dir
}

func TestSecureEditFile_ReplaceAllFalse_OnlyFirst(t *testing.T) {
	ctx, dir := withAutoApproveAll(t)
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\na\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := secureEditFile(ctx, "f.txt", "a", "X", false)
	if !strings.Contains(got, "Edited") {
		t.Fatalf("unexpected result: %s", got)
	}
	data, _ := os.ReadFile(p)
	want := "X\nb\na\nb\n"
	if string(data) != want {
		t.Errorf("replace_all=false: got %q, want %q", string(data), want)
	}
}

func TestSecureEditFile_ReplaceAllTrue_All(t *testing.T) {
	ctx, dir := withAutoApproveAll(t)
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\na\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := secureEditFile(ctx, "f.txt", "a", "X", true)
	if !strings.Contains(got, "Edited") {
		t.Fatalf("unexpected result: %s", got)
	}
	data, _ := os.ReadFile(p)
	want := "X\nb\nX\nb\n"
	if string(data) != want {
		t.Errorf("replace_all=true: got %q, want %q", string(data), want)
	}
}

func TestSecureEditFile_ReplaceAllWhitespaceTolerant(t *testing.T) {
	ctx, dir := withAutoApproveAll(t)
	p := filepath.Join(dir, "f.txt")
	// Two occurrences with different surrounding indentation; exact
	// match fails, whitespace-tolerant match finds both.
	if err := os.WriteFile(p, []byte("  foo\nbar\n\tfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := secureEditFile(ctx, "f.txt", "foo", "baz", true)
	if !strings.Contains(got, "Edited") {
		t.Fatalf("unexpected result: %s", got)
	}
	data, _ := os.ReadFile(p)
	want := "  baz\nbar\n\tbaz\n"
	if string(data) != want {
		t.Errorf("replace_all whitespace-tolerant: got %q, want %q", string(data), want)
	}
}

func TestSecureInsertFile_Middle(t *testing.T) {
	ctx, dir := withAutoApproveAll(t)
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := secureInsertFile(ctx, "f.txt", 1, "inserted")
	if !strings.Contains(got, "Inserted") {
		t.Fatalf("unexpected result: %s", got)
	}
	data, _ := os.ReadFile(p)
	want := "line1\ninserted\nline2\nline3\n"
	if string(data) != want {
		t.Errorf("insert after line 1: got %q, want %q", string(data), want)
	}
}

func TestSecureInsertFile_PrependAndAppend(t *testing.T) {
	ctx, dir := withAutoApproveAll(t)
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// after_line=0 -> prepend.
	if got := secureInsertFile(ctx, "f.txt", 0, "head"); !strings.Contains(got, "Inserted") {
		t.Fatalf("prepend failed: %s", got)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "head\na\nb\n" {
		t.Errorf("prepend: got %q", string(data))
	}
	// after_line >= line count -> append.
	if got := secureInsertFile(ctx, "f.txt", 99, "tail"); !strings.Contains(got, "Inserted") {
		t.Fatalf("append failed: %s", got)
	}
	data, _ = os.ReadFile(p)
	if string(data) != "head\na\nb\ntail\n" {
		t.Errorf("append: got %q", string(data))
	}
}

func TestSecureInsertFile_NewFile(t *testing.T) {
	ctx, dir := withAutoApproveAll(t)
	got := secureInsertFile(ctx, "new.txt", 0, "fresh")
	if !strings.Contains(got, "Inserted") {
		t.Fatalf("unexpected result: %s", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fresh\n" {
		t.Errorf("new file: got %q, want %q", string(data), "fresh\n")
	}
}
