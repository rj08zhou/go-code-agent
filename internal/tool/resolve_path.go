package tool

import "go-code-agent/internal/security"

// resolveFSPath sandboxes path under scope.Workdir, remapping absolute paths
// that point at SourceWorkdir (host repo) into the active worktree first.
func resolveFSPath(scope *ToolScope, path string, allowWrite bool) (string, error) {
	workdir, source := "", ""
	if scope != nil {
		workdir = scope.Workdir
		source = scope.SourceWorkdir
	}
	return security.SecurePathMapped(workdir, source, path, allowWrite)
}
