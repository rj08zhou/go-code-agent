package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/logging"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrWorktreeDirty is returned by Remove when the worktree has
// uncommitted/untracked changes or commits not yet merged back to
// the main repo - i.e. deleting it would lose work. Use ForceRemove
// to delete anyway.
var ErrWorktreeDirty = errors.New("worktree has unmerged or uncommitted changes")

// ErrTooManyWorktrees is returned by Create when infra.MaxActiveWorktrees
// active worktrees already exist and teammateName isn't one of them
// (i.e. this would be a new one, not a reuse). Shut an idle teammate
// down first.
var ErrTooManyWorktrees = errors.New("too many active worktrees")

// WorktreeManager creates and removes git worktrees for teammates,
// giving each its own isolated working directory instead of sharing
// App.Workdir.
type WorktreeManager struct {
	root     string // parent dir for all worktrees ({sessionDir}/worktrees)
	registry string // {root}/registry.json
	mainRepo string // the main workspace root (App.Workdir)
	mu       sync.Mutex
	entries  map[string]wtEntry // teammateName → entry
}

type wtEntry struct {
	Branch    string `json:"branch"`
	Path      string `json:"path"`
	CreatedAt int64  `json:"created_at"`
}

func NewWorktreeManager(sessionDir, mainRepo string) *WorktreeManager {
	wm := &WorktreeManager{
		root:     filepath.Join(sessionDir, "worktrees"),
		mainRepo: mainRepo,
		registry: filepath.Join(sessionDir, "worktrees", "registry.json"),
		entries:  map[string]wtEntry{},
	}
	os.MkdirAll(wm.root, 0o755)
	wm.loadRegistry()
	return wm
}

func (wm *WorktreeManager) loadRegistry() {
	data, err := os.ReadFile(wm.registry)
	if err != nil {
		return
	}
	json.Unmarshal(data, &wm.entries)
}

func (wm *WorktreeManager) saveRegistry() {
	data, _ := json.MarshalIndent(wm.entries, "", "  ")
	os.WriteFile(wm.registry, data, 0o644)
}

// Create makes a new git worktree at a branch derived from teammateName.
// Returns the worktree's filesystem path.
func (wm *WorktreeManager) Create(teammateName string) (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	// Reuse if already exists (teammate respawned after idle).
	if e, ok := wm.entries[teammateName]; ok {
		if _, err := os.Stat(e.Path); err == nil {
			wm.warnIfBehindMain(teammateName, e)
			return e.Path, nil
		}
		// Path gone — fall through to recreate.
	}

	if len(wm.entries) >= infra.MaxActiveWorktrees {
		return "", ErrTooManyWorktrees
	}

	branch := fmt.Sprintf("teammate/%s", teammateName)
	wtPath := filepath.Join(wm.root, teammateName)

	// Remove stale dir if any.
	os.RemoveAll(wtPath)

	// git worktree add -b {branch} {path}  (from main repo root)
	cmd := exec.Command("git", "worktree", "add", "-b", branch, wtPath, "HEAD")
	cmd.Dir = wm.mainRepo
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %w (%s)", err, string(out))
	}

	wm.copyIncludedFiles(wtPath)

	wm.entries[teammateName] = wtEntry{
		Branch:    branch,
		Path:      wtPath,
		CreatedAt: nowUnix(),
	}
	wm.saveRegistry()
	return wtPath, nil
}

// warnIfBehindMain logs when a reused worktree's branch point has
// fallen behind the main repo's current HEAD - e.g. the main agent
// kept committing while this teammate was idle. Create never rebases
// automatically (that could turn a clean reuse into a merge conflict
// the teammate isn't expecting mid-task); this just makes the drift
// visible instead of the teammate silently working off stale code.
func (wm *WorktreeManager) warnIfBehindMain(teammateName string, e wtEntry) {
	cmd := exec.Command("git", "rev-list", "--count", e.Branch+"..HEAD")
	cmd.Dir = wm.mainRepo
	out, err := cmd.Output()
	if err != nil {
		return
	}
	if n := strings.TrimSpace(string(out)); n != "" && n != "0" {
		logging.PrintSystem(fmt.Sprintf("[team] reused worktree for '%s' is %s commit(s) behind main HEAD (rebase manually if needed): %s", teammateName, n, e.Path))
	}
}

// worktreeIncludeFile selects ignored files (.env, local secrets) to copy
// into every new worktree, since a fresh checkout only has tracked files.
const worktreeIncludeFile = ".worktreeinclude"

// copyIncludedFiles copies the standard-ignored files selected by
// .worktreeinclude into the new worktree. Uses git's own ignore resolver
// so patterns support full gitignore semantics (**, negation, etc).
func (wm *WorktreeManager) copyIncludedFiles(wtPath string) {
	includePath := filepath.Join(wm.mainRepo, worktreeIncludeFile)
	if _, err := os.Stat(includePath); err != nil {
		return
	}

	stdOut, err := exec.Command("git", "-C", wm.mainRepo, "ls-files", "--others", "--ignored", "--exclude-standard", "-z").Output()
	if err != nil {
		return
	}

	incOut, err := exec.Command("git", "-C", wm.mainRepo, "ls-files", "--others", "--ignored", "--exclude-from="+worktreeIncludeFile, "-z").Output()
	if err != nil {
		return
	}

	included := map[string]struct{}{}
	for path := range strings.SplitSeq(string(incOut), "\x00") {
		if path != "" {
			included[path] = struct{}{}
		}
	}

	for path := range strings.SplitSeq(string(stdOut), "\x00") {
		if path == "" {
			continue
		}
		if _, ok := included[path]; !ok {
			continue
		}
		if err := copyPath(filepath.Join(wm.mainRepo, path), filepath.Join(wtPath, path)); err != nil {
			logging.PrintSystem(fmt.Sprintf("[team] .worktreeinclude: failed to copy %s: %v", path, err))
		}
	}
}

// copyPath copies src to dst, recursing into directories. Used only
// for the small, explicitly-opted-in .worktreeinclude file set (env
// files, local config) - not a general-purpose bulk-copy utility.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// Remove deletes a teammate's worktree and its branch, but only if
// there is nothing that would be lost: no uncommitted/untracked
// changes in the working tree, and no commits on the branch that
// aren't already reachable from the main repo's HEAD. This mirrors
// Claude Code / Codex's worktree cleanup policy (delete only when
// clean; otherwise leave it for a human to look at) instead of the
// git-worktree-remove --force default, which would silently discard
// any in-progress work a teammate hadn't gotten around to committing
// or that never made it back to the main branch.
//
// Returns ErrWorktreeDirty (leaving the worktree and registry entry
// intact) if there's anything to lose. Use ForceRemove to override.
func (wm *WorktreeManager) Remove(teammateName string) error {
	wm.mu.Lock()
	entry, ok := wm.entries[teammateName]
	wm.mu.Unlock()
	if !ok {
		return nil // nothing to remove
	}

	if dirty, reason := wm.isUnsafeToRemove(entry); dirty {
		logging.PrintSystem(fmt.Sprintf("[team] keeping worktree for '%s' (%s): %s", teammateName, reason, entry.Path))
		return ErrWorktreeDirty
	}

	return wm.forceRemoveLocked(teammateName, entry)
}

// ForceRemove deletes a teammate's worktree and branch unconditionally,
// discarding any uncommitted or unmerged work. Used when the caller
// has already decided (or asked the user) that it's fine to lose it.
func (wm *WorktreeManager) ForceRemove(teammateName string) error {
	wm.mu.Lock()
	entry, ok := wm.entries[teammateName]
	wm.mu.Unlock()
	if !ok {
		return nil
	}
	return wm.forceRemoveLocked(teammateName, entry)
}

func (wm *WorktreeManager) forceRemoveLocked(teammateName string, entry wtEntry) error {
	cmd := exec.Command("git", "worktree", "remove", "--force", entry.Path)
	cmd.Dir = wm.mainRepo
	cmd.CombinedOutput() // best-effort

	cmd = exec.Command("git", "branch", "-D", entry.Branch)
	cmd.Dir = wm.mainRepo
	cmd.CombinedOutput()

	// Fallback: ensure dir is gone even if git refused.
	os.RemoveAll(entry.Path)

	wm.mu.Lock()
	delete(wm.entries, teammateName)
	wm.saveRegistry()
	wm.mu.Unlock()
	return nil
}

// isUnsafeToRemove reports whether entry's worktree has anything that
// would be lost by deleting it: an uncommitted/untracked change in
// the working tree, or a commit on its branch not reachable from the
// main repo's current HEAD (work that was committed but never merged
// back). Either check failing to run (e.g. git binary issues) is
// treated as "unsafe" - fail closed, since the cost of skipping a
// cleanup is far lower than the cost of silently discarding work.
func (wm *WorktreeManager) isUnsafeToRemove(entry wtEntry) (bool, string) {
	statusCmd := exec.Command("git", "status", "--porcelain")
	statusCmd.Dir = entry.Path
	out, err := statusCmd.Output()
	if err != nil {
		return true, "could not check working-tree status"
	}
	if strings.TrimSpace(string(out)) != "" {
		return true, "uncommitted or untracked changes"
	}

	revCmd := exec.Command("git", "rev-list", "--count", "HEAD.."+entry.Branch)
	revCmd.Dir = wm.mainRepo
	out, err = revCmd.Output()
	if err != nil {
		return true, "could not check for unmerged commits"
	}
	if strings.TrimSpace(string(out)) != "0" {
		return true, "unmerged commits on " + entry.Branch
	}

	return false, ""
}

// PathFor returns the worktree path for a teammate, or "" if none.
func (wm *WorktreeManager) PathFor(teammateName string) string {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	if e, ok := wm.entries[teammateName]; ok {
		return e.Path
	}
	return ""
}

// CleanupOrphans removes worktrees whose teammates no longer exist
// in the TeammateManager's member list. Called on session activation
// to recover from a crash that left stale worktrees behind.
func (wm *WorktreeManager) CleanupOrphans(activeTeammates []string) {
	active := map[string]bool{}
	for _, n := range activeTeammates {
		active[n] = true
	}

	wm.mu.Lock()
	names := make([]string, 0, len(wm.entries))
	for n := range wm.entries {
		names = append(names, n)
	}
	wm.mu.Unlock()

	for _, name := range names {
		if !active[name] {
			wm.Remove(name)
		}
	}
}

// RemoveAll removes all known worktrees. Called on session deactivation.
func (wm *WorktreeManager) RemoveAll() {
	wm.mu.Lock()
	names := make([]string, 0, len(wm.entries))
	for n := range wm.entries {
		names = append(names, n)
	}
	wm.mu.Unlock()

	for _, name := range names {
		wm.Remove(name)
	}
}

func nowUnix() int64 {
	return time.Now().Unix()
}
