// Snapshot-and-rollback (Saga) for risky write tools.
//
// Before each risky tool call, snapshot via `git stash create`;
// on failure, restore via `git read-tree`. Opt-in via SNAPSHOT_ENABLED=1.
// No-op outside a git repo. Untracked files are not snapshotted.
package main

import (
	"context"
	"fmt"
	"go-code-agent/internal/log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var riskySnapshotTools = map[string]bool{
	"write_file":      true,
	"edit_file":       true,
	"delete_file":     true,
	"bash":            true,
	"execute_command": true,
	"background_run":  true,
}

type snapshotState struct {
	mu      sync.Mutex
	enabled bool
}

var globalSnapshot = &snapshotState{}

func (s *snapshotState) Enable()  { s.mu.Lock(); defer s.mu.Unlock(); s.enabled = true }
func (s *snapshotState) Disable() { s.mu.Lock(); defer s.mu.Unlock(); s.enabled = false }
func (s *snapshotState) IsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

func snapshotShouldWrap(toolName string) bool {
	if !globalSnapshot.IsEnabled() {
		return false
	}
	if !riskySnapshotTools[toolName] {
		return false
	}
	return isGitRepo()
}

func isGitRepo() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func takeSnapshot() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "stash", "create")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git stash create: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func restoreSnapshot(sha string) error {
	if sha == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "read-tree", "-u", "--reset", sha)
	cmd.Dir = workdir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("read-tree: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// withSnapshot wraps a tool invocation with snapshot/rollback.
func withSnapshot(toolName string, run func() ToolResult) ToolResult {
	if !snapshotShouldWrap(toolName) {
		return run()
	}

	sha, err := takeSnapshot()
	if err != nil {
		// If snapshot itself fails, proceed without it — never block
		// the agent on infra issues.
		log.PrintSystem(fmt.Sprintf("[snapshot] skip (%v)", err))
		return run()
	}

	result := run()
	if result.OK {
		return result
	}

	// Failed — try rollback.
	if rerr := restoreSnapshot(sha); rerr != nil {
		log.PrintSystem(fmt.Sprintf("[snapshot] rollback failed: %v", rerr))
		result.Output += fmt.Sprintf("\n[snapshot] rollback FAILED: %v (workspace may be in partial state)", rerr)
		return result
	}
	if sha != "" {
		log.PrintSystem(fmt.Sprintf("[snapshot] rolled back '%s' to %s", toolName, sha[:8]))
		result.Output += fmt.Sprintf("\n[snapshot] working tree restored to pre-call state (%s)", sha[:8])
	}
	return result
}
