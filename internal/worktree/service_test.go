package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	return dir
}

func TestAcquireReleaseRoundTrip(t *testing.T) {
	workdir := initGitRepo(t)
	dataDir := t.TempDir()
	svc := New(workdir, dataDir)

	lease, err := svc.Acquire("worker")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.BranchName != "agent_worker" {
		t.Fatalf("branch = %q", lease.BranchName)
	}
	if _, err := os.Stat(lease.WorktreeDir); err != nil {
		t.Fatalf("worktree missing: %v", err)
	}

	// Second acquire returns the same lease.
	again, err := svc.Acquire("worker")
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if again.WorktreeDir != lease.WorktreeDir {
		t.Fatalf("expected cached lease")
	}

	if err := svc.Release("worker"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := svc.leases["worker"]; ok {
		t.Fatal("lease should be forgotten after successful Release")
	}
	if _, err := os.Stat(lease.WorktreeDir); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed, err=%v", err)
	}
}

func TestAcquireRollsBackOnCheckoutFailure(t *testing.T) {
	workdir := initGitRepo(t)
	dataDir := t.TempDir()
	svc := New(workdir, dataDir)

	// Pre-create the branch so checkout -b fails.
	cmd := exec.Command("git", "-C", workdir, "branch", "agent_dup")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create branch: %v\n%s", err, out)
	}

	_, err := svc.Acquire("dup")
	if err == nil {
		t.Fatal("expected Acquire to fail when branch already exists")
	}
	if !strings.Contains(err.Error(), "checkout") {
		t.Fatalf("error = %v, want checkout failure", err)
	}
	if _, ok := svc.leases["dup"]; ok {
		t.Fatal("failed Acquire must not record a lease")
	}
	worktreeDir := filepath.Join(dataDir, "worktrees", "dup")
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Fatalf("failed Acquire should remove worktree, err=%v", err)
	}
}

func TestRemoveAllKeepsFailedLeases(t *testing.T) {
	workdir := initGitRepo(t)
	dataDir := t.TempDir()
	svc := New(workdir, dataDir)

	lease, err := svc.Acquire("keep")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Corrupt the worktree path so remove fails; lease must remain.
	lease.WorktreeDir = filepath.Join(dataDir, "worktrees", "missing-path")

	err = svc.RemoveAll()
	if err == nil {
		t.Fatal("expected RemoveAll error for missing worktree")
	}
	if _, ok := svc.leases["keep"]; !ok {
		t.Fatal("failed remove should retain lease for retry")
	}
}

func TestRemoveAllClearsSuccessfulLeases(t *testing.T) {
	workdir := initGitRepo(t)
	dataDir := t.TempDir()
	svc := New(workdir, dataDir)

	if _, err := svc.Acquire("a"); err != nil {
		t.Fatalf("Acquire a: %v", err)
	}
	if _, err := svc.Acquire("b"); err != nil {
		t.Fatalf("Acquire b: %v", err)
	}
	if err := svc.RemoveAll(); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if len(svc.leases) != 0 {
		t.Fatalf("leases left: %d", len(svc.leases))
	}
}
