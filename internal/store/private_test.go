package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestAtomicWritePrivateCreatesOwnerOnlyFileAndDir(t *testing.T) {
	// A permissive umask must not widen private state permissions.
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	dir := filepath.Join(t.TempDir(), "nested", "state")
	path := filepath.Join(dir, "meta.json")
	if err := AtomicWritePrivate(path, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}
}

func TestAtomicWritePrivateTightensExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(path, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWritePrivate(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Fatalf("rewritten file mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("content = %q err = %v", data, err)
	}
}

func TestEnsurePrivateDirTightensLegacyDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Fatalf("legacy dir mode = %o, want 700", got)
	}
}

func TestOpenPrivateAppendCreatesAndTightens(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	path := filepath.Join(t.TempDir(), "logs", "history.jsonl")
	f, err := OpenPrivateAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line1\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := mode(t, path); got != 0o600 {
		t.Fatalf("new file mode = %o, want 600", got)
	}

	// Pre-existing permissive file gets tightened on reopen, content appended.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = OpenPrivateAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line2\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := mode(t, path); got != 0o600 {
		t.Fatalf("reopened file mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "line1\nline2\n" {
		t.Fatalf("append lost data: %q err=%v", data, err)
	}
}

func TestAtomicWriteKeepsWorkspaceDefaults(t *testing.T) {
	// Workspace-facing writes must NOT be tightened: user files stay 0644.
	path := filepath.Join(t.TempDir(), "user.txt")
	if err := AtomicWrite(path, []byte("workspace file")); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, path); got != 0o644 {
		t.Fatalf("workspace file mode = %o, want 644", got)
	}
}
