package agent

import (
	"context"
	"fmt"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/tool"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// riskySnapshotTools is read-only after init; safe for concurrent access.
func isRiskySnapshotTool(name string) bool {
	_, ok := riskySnapshotTools[name]
	return ok
}

var riskySnapshotTools = map[string]bool{
	"write_file":     true,
	"edit_file":      true,
	"delete_file":    true,
	"bash":           true,
	"background_run": true,
}

// SnapshotManager handles git stash-based snapshot and rollback.
type SnapshotManager struct {
	mu      sync.Mutex
	enabled bool
	workdir string
}

func NewSnapshotManager(enabled bool, workdir string) *SnapshotManager {
	return &SnapshotManager{enabled: enabled, workdir: workdir}
}

func (s *SnapshotManager) Enable()  { s.mu.Lock(); defer s.mu.Unlock(); s.enabled = true }
func (s *SnapshotManager) Disable() { s.mu.Lock(); defer s.mu.Unlock(); s.enabled = false }
func (s *SnapshotManager) IsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// ShouldWrap reports whether a tool call should be snapshotted.
func (s *SnapshotManager) ShouldWrap(toolName string) bool {
	if !s.IsEnabled() || !isRiskySnapshotTool(toolName) {
		return false
	}
	return isGitRepo(s.workdir)
}

// WithSnapshot wraps a tool invocation with snapshot/rollback.
func (s *SnapshotManager) WithSnapshot(toolName string, run func() tool.Result) tool.Result {
	if !s.ShouldWrap(toolName) {
		return run()
	}
	sha, err := takeSnapshot(s.workdir)
	if err != nil {
		logging.Default().Warn(fmt.Sprintf("snapshot skipped for %s: %v", toolName, err))
		result := run()
		if result.Output != "" {
			result.Output += "\n"
		}
		result.Output += "[snapshot] unavailable; tool ran without rollback protection"
		return result
	}
	result := run()
	if result.Succeeded() {
		return result
	}
	if rerr := restoreSnapshot(s.workdir, sha); rerr != nil {
		logging.Default().Warn(fmt.Sprintf("snapshot rollback failed for %s: %v", toolName, rerr))
		result.Output += fmt.Sprintf("\n[snapshot] rollback FAILED: %v", rerr)
		return result
	}
	if sha != "" {
		result.Output += fmt.Sprintf("\n[snapshot] working tree restored to pre-call state (%s)", sha[:8])
	}
	return result
}

func isGitRepo(dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = dir
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func takeSnapshot(dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "stash", "create", "--include-untracked")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git stash create: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func restoreSnapshot(dir, sha string) error {
	if sha == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "read-tree", "-u", "--reset", sha)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("read-tree: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
