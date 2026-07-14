package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestWorkdirFromCtx verifies the override/fallback contract that the
// whole worktree-isolation feature rests on: a ctx set via WithWorkdir
// wins, and an unset ctx falls back to App.Workdir.
func TestWorkdirFromCtx(t *testing.T) {
	// Save and restore the global App so the test is self-contained.
	saved := App
	t.Cleanup(func() { App = saved })
	App = &AppContext{Workdir: "/fallback/workdir"}

	// No override -> App.Workdir.
	if got := workdirFromCtx(context.Background()); got != "/fallback/workdir" {
		t.Errorf("no override: got %q, want /fallback/workdir", got)
	}
	// nil ctx -> App.Workdir (defensive).
	if got := workdirFromCtx(nil); got != "/fallback/workdir" {
		t.Errorf("nil ctx: got %q, want /fallback/workdir", got)
	}
	// Override wins.
	ctx := WithWorkdir(context.Background(), "/team/worktree")
	if got := workdirFromCtx(ctx); got != "/team/worktree" {
		t.Errorf("override: got %q, want /team/worktree", got)
	}
	// Empty override is a no-op (returns same ctx, falls back).
	ctx2 := WithWorkdir(context.Background(), "")
	if got := workdirFromCtx(ctx2); got != "/fallback/workdir" {
		t.Errorf("empty override: got %q, want /fallback/workdir", got)
	}
}

// TestSecureReadFile_HonorsCtxWorkdir is an end-to-end check that a
// secure* function actually confines itself to the ctx-provided
// workdir: the same relative path resolves into whichever directory the
// ctx names.
func TestSecureReadFile_HonorsCtxWorkdir(t *testing.T) {
	saved := App
	t.Cleanup(func() { App = saved })

	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "f.txt"), []byte("from-A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "f.txt"), []byte("from-B"), 0o644); err != nil {
		t.Fatal(err)
	}

	// App.Workdir points at dirA; a ctx override points at dirB.
	App = &AppContext{Workdir: dirA}

	if got := secureReadFile(context.Background(), "f.txt", 0, 0); got != "from-A" {
		t.Errorf("fallback workdir: got %q, want from-A", got)
	}
	ctx := WithWorkdir(context.Background(), dirB)
	if got := secureReadFile(ctx, "f.txt", 0, 0); got != "from-B" {
		t.Errorf("ctx workdir: got %q, want from-B", got)
	}
}
