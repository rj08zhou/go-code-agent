package infra

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// AppRootDirName is the runtime state directory name. Under the new
// layout the state directory is NOT nested inside the project workdir;
// instead it lives under the user-level config dir, keyed by a hash of
// the project path (mirrors Claude Code's ~/.claude/projects/<hash>/).
// This name is used as a subdirectory under the resolved data dir so
// that a future human-readable form (e.g. the project's basename) can
// coexist without risk of path collisions.
const AppRootDirName = ".go-code-agent"

// DataDirOverrideEnv is the environment variable that forces the state
// directory to a fixed location (e.g. a mounted volume in containers).
// The --data-dir CLI flag takes precedence when both are set.
const DataDirOverrideEnv = "GO_CODE_AGENT_DATA_DIR"

// projectKey builds a stable, filesystem-safe identifier for a project
// path. We hash the absolute path rather than use the basename so two
// projects with the same name in different locations map to distinct
// state dirs, and so paths that are not valid filenames (slashes,
// unicode) can never break the layout.
func projectKey(absWorkdir string) string {
	sum := sha256.Sum256([]byte(absWorkdir))
	return hex.EncodeToString(sum[:])[:16]
}

// ResolveDataDir returns the directory where the agent's persistent
// state lives for the given project workdir.
//
// Precedence (highest first):
//  1. override  — explicit --data-dir value from the CLI
//  2. GO_CODE_AGENT_DATA_DIR — environment override
//  3. default   — os.UserConfigDir()/go-code-agent/projects/<hash>
//
// The default keeps all runtime state (sessions, memory, usage, HITL
// audit, permissions) OUT of the project directory, so the code dir
// stays clean and the same project maps to the same state across
// launches regardless of where the binary runs from.
func ResolveDataDir(workdir, override string) string {
	if override != "" {
		return override
	}
	if env := os.Getenv(DataDirOverrideEnv); env != "" {
		return env
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		// Fall back to the raw workdir if abs fails (extremely rare).
		abs = workdir
	}
	base, err := os.UserConfigDir()
	if err != nil {
		// Last-resort fallback: keep state next to the binary's cwd.
		base = "."
	}
	return filepath.Join(base, "go-code-agent", "projects", projectKey(abs))
}
