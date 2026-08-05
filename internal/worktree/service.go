package worktree

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Lease represents an agent's exclusive git worktree.
type Lease struct {
	AgentID     string
	WorktreeDir string
	BranchName  string
}

// Service manages worktree creation, cleanup, and merge.
type Service struct {
	workdir string
	dataDir string
	mu      sync.Mutex
	leases  map[string]*Lease
}

func New(workdir, dataDir string) *Service {
	return &Service{
		workdir: workdir,
		dataDir: dataDir,
		leases:  make(map[string]*Lease),
	}
}

func (s *Service) runGit(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func (s *Service) Acquire(agentID string) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.leases[agentID]; ok {
		return l, nil
	}
	branchName := fmt.Sprintf("agent_%s", agentID)
	worktreeDir := filepath.Join(s.dataDir, "worktrees", agentID)

	if err := s.runGit(s.workdir, "worktree", "add", "--detach", worktreeDir, "HEAD"); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}

	if err := s.runGit(worktreeDir, "checkout", "-b", branchName); err != nil {
		_ = s.runGit(s.workdir, "worktree", "remove", "--force", worktreeDir)
		return nil, fmt.Errorf("git checkout -b %s: %w", branchName, err)
	}

	l := &Lease{AgentID: agentID, WorktreeDir: worktreeDir, BranchName: branchName}
	s.leases[agentID] = l
	return l, nil
}

func (s *Service) Release(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.leases[agentID]
	if !ok {
		return nil
	}
	if err := s.runGit(s.workdir, "worktree", "remove", "--force", l.WorktreeDir); err != nil {
		return fmt.Errorf("git worktree remove %s: %w", agentID, err)
	}
	delete(s.leases, agentID)
	return nil
}

// RemoveAll removes every tracked worktree. Leases that fail to remove are
// retained so a later attempt can retry; successful removals are forgotten.
func (s *Service) RemoveAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for id, l := range s.leases {
		if err := s.runGit(s.workdir, "worktree", "remove", "--force", l.WorktreeDir); err != nil {
			errs = append(errs, fmt.Errorf("git worktree remove %s: %w", id, err))
			continue
		}
		delete(s.leases, id)
	}
	return errors.Join(errs...)
}
