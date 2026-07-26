package worktree

import (
	"fmt"
	"os/exec"
	"path/filepath"
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

func (s *Service) Acquire(agentID string) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.leases[agentID]; ok {
		return l, nil
	}
	branchName := fmt.Sprintf("agent_%s", agentID)
	worktreeDir := filepath.Join(s.dataDir, "worktrees", agentID)

	err := exec.Command("git",
		"-C", s.workdir,
		"worktree", "add", "--detach", worktreeDir,
		"HEAD",
	).Run()
	if err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}

	exec.Command("git",
		"-C", worktreeDir,
		"checkout", "-b", branchName,
	).Run()

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
	delete(s.leases, agentID)
	exec.Command("git",
		"-C", s.workdir,
		"worktree", "remove", "--force", l.WorktreeDir,
	).Run()
	return nil
}

func (s *Service) RemoveAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.leases {
		exec.Command("git",
			"-C", s.workdir,
			"worktree", "remove", "--force", l.WorktreeDir,
		).Run()
		delete(s.leases, id)
	}
}
