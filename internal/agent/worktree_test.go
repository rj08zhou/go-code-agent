package agent

import (
	"errors"
	"fmt"
	"go-code-agent/infra"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initTestRepo creates a real git repo with one commit in a temp dir.
func initTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	return dir
}

func TestWorktreeCreateAndRemove(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "README")); err != nil {
		t.Errorf("worktree should contain README from HEAD: %v", err)
	}
	if got := wm.PathFor("alice"); got != path {
		t.Errorf("PathFor = %q, want %q", got, path)
	}

	if err := wm.Remove("alice"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone after Remove")
	}
	if got := wm.PathFor("alice"); got != "" {
		t.Errorf("PathFor after Remove = %q, want empty", got)
	}

	// Branch should be deleted too.
	cmd := exec.Command("git", "branch", "--list", "teammate/alice")
	cmd.Dir = repo
	out, _ := cmd.CombinedOutput()
	if len(out) > 0 {
		t.Errorf("branch should be deleted, got: %s", string(out))
	}
}

func TestWorktreeCreateReuseExisting(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	p1, err := wm.Create("bob")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Second Create on same name should reuse, not error.
	p2, err := wm.Create("bob")
	if err != nil {
		t.Fatalf("reuse Create: %v", err)
	}
	if p1 != p2 {
		t.Errorf("expected reuse, got p1=%q p2=%q", p1, p2)
	}
	wm.Remove("bob")
}

// Reusing a worktree whose branch point has fallen behind the main
// repo's HEAD must still succeed (Create never auto-rebases) - this
// only exercises warnIfBehindMain's logging path without crashing or
// blocking the reuse.
func TestWorktreeCreate_ReuseBehindMainStillSucceeds(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	p1, err := wm.Create("carol")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Advance main repo's HEAD past the worktree's branch point.
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "advance main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	p2, err := wm.Create("carol")
	if err != nil {
		t.Fatalf("reuse while behind main: %v", err)
	}
	if p1 != p2 {
		t.Errorf("expected reuse, got p1=%q p2=%q", p1, p2)
	}
	wm.ForceRemove("carol")
}

func TestWorktreeCleanupOrphans(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	wm.Create("alice")
	wm.Create("bob")

	// Only alice is active — bob is an orphan.
	wm.CleanupOrphans([]string{"alice"})

	if wm.PathFor("alice") == "" {
		t.Error("alice should survive cleanup")
	}
	if wm.PathFor("bob") != "" {
		t.Error("bob should be cleaned up as orphan")
	}
	wm.Remove("alice")
}

func TestWorktreeRemoveAll(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	wm.Create("alice")
	wm.Create("bob")
	wm.RemoveAll()

	for _, name := range []string{"alice", "bob"} {
		if wm.PathFor(name) != "" {
			t.Errorf("%s should be gone after RemoveAll", name)
		}
	}
}

func TestWorktreeRegistryPersistsAcrossRestart(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()

	wm1 := NewWorktreeManager(sessionDir, repo)
	wm1.Create("alice")

	// Simulate restart: new manager reads same registry.
	wm2 := NewWorktreeManager(sessionDir, repo)
	if got := wm2.PathFor("alice"); got == "" {
		t.Error("registry should survive restart")
	}
	wm2.Remove("alice")
}

// .worktreeinclude support: gitignored files (e.g. .env) should be
// copied into a new worktree, mirroring Claude Code's mechanism for
// the same problem (a fresh checkout lacks untracked local config).
func TestWorktreeCreate_CopiesWorktreeInclude(t *testing.T) {
	repo := initTestRepo(t)

	// .env is gitignored in the main repo (never committed).
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreeinclude"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)
	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(path, ".env"))
	if err != nil {
		t.Fatalf(".env should have been copied into worktree: %v", err)
	}
	if string(got) != "SECRET=1\n" {
		t.Errorf(".env content = %q, want %q", string(got), "SECRET=1\n")
	}
	wm.ForceRemove("alice")
}

func TestWorktreeCreate_NoWorktreeIncludeIsNoOp(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree should still be created without .worktreeinclude: %v", err)
	}
	wm.ForceRemove("alice")
}

// Regression for the old filepath.Glob implementation: ** should be
// recursive, not just a single-directory glob.
func TestWorktreeCreate_WorktreeIncludeRecursivePattern(t *testing.T) {
	repo := initTestRepo(t)

	if err := os.MkdirAll(filepath.Join(repo, "config", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".gitignore", ".worktreeinclude"} {
		if err := os.WriteFile(filepath.Join(repo, p), []byte("config/**/*.env\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "config", "nested", "local.env"), []byte("LOCAL=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)
	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(path, "config", "nested", "local.env"))
	if err != nil {
		t.Fatalf("recursive .worktreeinclude pattern should copy nested file: %v", err)
	}
	if string(got) != "LOCAL=1\n" {
		t.Errorf("local.env content = %q, want %q", string(got), "LOCAL=1\n")
	}
	wm.ForceRemove("alice")
}

// .worktreeinclude negation should be respected.
func TestWorktreeCreate_WorktreeIncludeNegation(t *testing.T) {
	repo := initTestRepo(t)

	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.tmp"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "keep.tmp"), []byte("KEEP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreeinclude"), []byte("*.tmp\n!keep.tmp\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)
	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := os.ReadFile(filepath.Join(path, "a.tmp")); err != nil {
		t.Errorf("a.tmp should have been copied: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(path, "keep.tmp")); err == nil {
		t.Errorf("keep.tmp should NOT have been copied (negated)")
	}
	wm.ForceRemove("alice")
}

// Only ignored, untracked files are eligible for copying — a plain
// untracked file listed in .worktreeinclude but not gitignored is
// left alone.
func TestWorktreeCreate_WorktreeIncludeSkipsNonIgnoredFiles(t *testing.T) {
	repo := initTestRepo(t)

	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ignored.txt"), []byte("I\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "plain.txt"), []byte("P\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".worktreeinclude"), []byte("*.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)
	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := os.ReadFile(filepath.Join(path, "ignored.txt")); err != nil {
		t.Errorf("ignored.txt should have been copied: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(path, "plain.txt")); err == nil {
		t.Errorf("plain.txt should NOT have been copied (not gitignored)")
	}
	wm.ForceRemove("alice")
}

// Regression: Create must refuse beyond infra.MaxActiveWorktrees
// rather than letting an unbounded number of full working-tree
// checkouts pile up.
func TestWorktreeCreate_EnforcesMaxActiveWorktrees(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	for i := range infra.MaxActiveWorktrees {
		if _, err := wm.Create(fmt.Sprintf("teammate-%d", i)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	_, err := wm.Create("one-too-many")
	if !errors.Is(err, ErrTooManyWorktrees) {
		t.Fatalf("Create beyond cap = %v, want ErrTooManyWorktrees", err)
	}

	for i := range infra.MaxActiveWorktrees {
		wm.ForceRemove(fmt.Sprintf("teammate-%d", i))
	}
}

// Reusing an existing worktree must not be blocked by the cap, and
// must not require a fresh git-worktree-add call.
func TestWorktreeCreate_ReuseNotBlockedByCap(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	for i := range infra.MaxActiveWorktrees {
		if _, err := wm.Create(fmt.Sprintf("teammate-%d", i)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	// Reusing teammate-0 (already counted in entries) must succeed
	// even though we're at the cap.
	if _, err := wm.Create("teammate-0"); err != nil {
		t.Fatalf("reuse at cap should succeed, got: %v", err)
	}

	for i := range infra.MaxActiveWorktrees {
		wm.ForceRemove(fmt.Sprintf("teammate-%d", i))
	}
}

// Regression: Remove must not discard uncommitted/untracked work.
// git worktree remove --force alone would silently blow this away.
func TestWorktreeRemove_KeepsUncommittedChanges(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := wm.Remove("alice"); err != ErrWorktreeDirty {
		t.Fatalf("Remove = %v, want ErrWorktreeDirty", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("dirty worktree should survive Remove: %v", err)
	}
	if wm.PathFor("alice") == "" {
		t.Error("dirty worktree's registry entry should survive Remove")
	}

	wm.ForceRemove("alice") // cleanup
}

// Regression: Remove must not discard commits made on the teammate's
// branch that were never merged back into the main repo.
func TestWorktreeRemove_KeepsUnmergedCommits(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "wip"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	if err := wm.Remove("alice"); err != ErrWorktreeDirty {
		t.Fatalf("Remove = %v, want ErrWorktreeDirty", err)
	}
	if wm.PathFor("alice") == "" {
		t.Error("worktree with unmerged commits should survive Remove")
	}

	wm.ForceRemove("alice") // cleanup
}

func TestWorktreeForceRemove_DiscardsDirtyState(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := wm.ForceRemove("alice"); err != nil {
		t.Fatalf("ForceRemove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("ForceRemove should delete the worktree despite dirty state")
	}
	if wm.PathFor("alice") != "" {
		t.Error("ForceRemove should clear the registry entry")
	}
}

// A merged branch (its commits are reachable from the main repo's
// HEAD via a normal merge) should be treated as safe to remove -
// isUnsafeToRemove must not flag legitimately-integrated work as
// unmerged just because it lived on a different branch name.
func TestWorktreeRemove_MergedBranchIsSafe(t *testing.T) {
	repo := initTestRepo(t)
	sessionDir := t.TempDir()
	wm := NewWorktreeManager(sessionDir, repo)

	path, err := wm.Create("alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "feature.txt"), []byte("work"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-m", "wip"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	// Merge the teammate's branch back into the main repo's HEAD.
	cmd := exec.Command("git", "merge", "teammate/alice", "--no-edit")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git merge: %v (%s)", err, string(out))
	}

	if err := wm.Remove("alice"); err != nil {
		t.Fatalf("Remove after merge = %v, want nil", err)
	}
	if wm.PathFor("alice") != "" {
		t.Error("merged worktree should be removed")
	}
}
