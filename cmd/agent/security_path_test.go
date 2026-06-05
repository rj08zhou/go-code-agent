package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test that securePath correctly handles both relative and absolute
// paths, and rejects escapes. Regression test for the bug where an
// absolute path inside the workdir was being joined again with the
// workdir, producing /workdir/Users/... and reporting "path does not
// exist" for files that were right there.
func TestSecurePath(t *testing.T) {
	wd := t.TempDir()
	// Resolve the temp dir to its real path because EvalSymlinks may
	// rewrite e.g. /var -> /private/var on macOS, and we want the test
	// expectations to match what securePath returns.
	realWd, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatalf("EvalSymlinks(wd): %v", err)
	}

	// Create a real file inside the workdir.
	subDir := filepath.Join(realWd, "cmd", "agent")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(subDir, "tool_registry.go")
	if err := os.WriteFile(target, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		input    string
		wantErr  bool
		wantPath string // when wantErr=false
	}{
		{
			name:     "relative path resolves under workdir",
			input:    "cmd/agent/tool_registry.go",
			wantErr:  false,
			wantPath: target,
		},
		{
			name:     "absolute path inside workdir is accepted as-is",
			input:    target,
			wantErr:  false,
			wantPath: target,
		},
		{
			name:    "absolute path outside workdir is rejected",
			input:   "/etc/passwd",
			wantErr: true,
		},
		{
			name:    "relative .. traversal is rejected",
			input:   "../etc/passwd",
			wantErr: true,
		},
		{
			name:    "relative path with embedded .. that stays inside is allowed",
			input:   "cmd/agent/../agent/tool_registry.go",
			wantErr: false, wantPath: target,
		},
		{
			name:    "empty path is rejected",
			input:   "",
			wantErr: true,
		},
		{
			name:     "non-existent file under workdir is allowed (write target)",
			input:    "cmd/agent/new_file.go",
			wantErr:  false,
			wantPath: filepath.Join(subDir, "new_file.go"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := securePath(realWd, tc.input, false)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantPath {
				t.Fatalf("got %q, want %q", got, tc.wantPath)
			}
		})
	}
}

// Verifies that absolute paths inside the workdir produce a path that
// actually exists on disk - this is the exact failure mode we saw in
// the agent log (read_file rejecting real files with "path does not
// exist").
func TestSecurePathAbsoluteFileExists(t *testing.T) {
	wd := t.TempDir()
	realWd, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realWd, "README.md")
	if err := os.WriteFile(target, []byte("# hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := securePath(realWd, target, false)
	if err != nil {
		t.Fatalf("securePath(abs) err: %v", err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("resolved path does not exist on disk: %v (got=%q)", err, got)
	}
	if !strings.HasPrefix(got, realWd) {
		t.Fatalf("resolved path %q escaped workdir %q", got, realWd)
	}
}

// Sanity-check: writes through allowWrite still reject sensitive files
// even when supplied as absolute paths inside workdir.
func TestSecurePathSensitiveBlocked(t *testing.T) {
	wd := t.TempDir()
	realWd, _ := filepath.EvalSymlinks(wd)
	if err := os.MkdirAll(filepath.Join(realWd, ".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(realWd, ".ssh", "id_rsa")
	if _, err := securePath(realWd, abs, true); err == nil {
		t.Fatalf("expected sensitive-path block for %s", abs)
	}
}
