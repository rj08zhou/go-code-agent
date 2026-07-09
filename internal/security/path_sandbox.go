package security

import (
	"fmt"
	"path/filepath"
	"strings"
)

// P0-2: Secure Path Sandbox - symlink escape prevention

// sensitivePathPatterns lists paths that should never be written to
// (even inside workdir).
var sensitivePathPatterns = []string{
	string(filepath.Separator) + ".ssh" + string(filepath.Separator),
	string(filepath.Separator) + ".aws" + string(filepath.Separator),
	string(filepath.Separator) + ".gnupg" + string(filepath.Separator),
	".pem",
	".id_rsa",
	".id_ed25519",
	".id_ecdsa",
	string(filepath.Separator) + ".env.local",
	string(filepath.Separator) + ".env.production",
	string(filepath.Separator) + ".credentials",
	string(filepath.Separator) + ".netrc",
}

// SecurePath validates and resolves a user-supplied path within the workdir.
// It prevents:
//   - ".." traversal escape (relative paths)
//   - Absolute paths outside the workdir
//   - Symlink escape (resolves symlinks before checking bounds)
//   - Writes to sensitive files (when allowWrite=true)
//
// Both relative paths (resolved against workdir) and absolute paths
// (must already point inside workdir) are accepted.
//
// It returns the resolved absolute path or an error.
func SecurePath(workdirPath, userPath string, allowWrite bool) (string, error) {
	// 1. Reject empty path
	if strings.TrimSpace(userPath) == "" {
		return "", fmt.Errorf("path is empty")
	}

	// 2. Resolve workdir to its real (symlink-evaluated) absolute form
	//    once, so the bounds check below compares apples to apples even
	//    when /var, /tmp, etc. are symlinks (common on macOS).
	absWorkdir, err := filepath.Abs(workdirPath)
	if err != nil {
		return "", fmt.Errorf("invalid workdir: %w", err)
	}
	if real, err := filepath.EvalSymlinks(absWorkdir); err == nil {
		absWorkdir = real
	}

	// 3. Build a candidate absolute path:
	//    - absolute input  -> use as-is (must still be inside workdir, checked in step 6)
	//    - relative input  -> reject ".." traversal, then join with workdir
	cleanUser := filepath.Clean(userPath)
	var resolved string
	if filepath.IsAbs(cleanUser) {
		resolved = cleanUser
	} else {
		if cleanUser == ".." || strings.HasPrefix(cleanUser, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path traversal not allowed: %s", userPath)
		}
		resolved = filepath.Clean(filepath.Join(absWorkdir, cleanUser))
	}

	// 5. Resolve symlinks - this is the key defense against symlink escapes.
	//
	// EvalSymlinks follows all symlinks in the path and returns the resolved target.
	// If any component doesn't exist yet, it returns an error; we fall back to
	// resolving the parent directory instead.
	realPath, err := filepath.EvalSymlinks(resolved)
	if err == nil {
		resolved = realPath
	} else {
		// Target doesn't exist yet (e.g., writing a new file).
		// Resolve the parent directory as far as possible to catch symlink parents.
		parent := filepath.Dir(resolved)
		realParent, evalErr := filepath.EvalSymlinks(parent)
		if evalErr != nil {
			// Don't leak underlying syscall details; return a user-friendly message.
			if strings.Contains(evalErr.Error(), "no such file") {
				return "", fmt.Errorf("path does not exist: %s", userPath)
			}
			return "", fmt.Errorf("path is invalid or inaccessible: %s", userPath)
		}
		resolved = filepath.Join(realParent, filepath.Base(resolved))
	}

	// 6. Verify the final resolved path is still within workdir.
	//    Compare against absWorkdir (already symlink-resolved in step 2)
	//    so a workdir like /var/folders/... that EvalSymlinks rewrites to
	//    /private/var/folders/... still matches its descendants.
	rel, err := filepath.Rel(absWorkdir, resolved)
	if err != nil {
		return "", fmt.Errorf("failed to resolve relative path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escape attempt blocked: '%s' resolves outside workspace to '%s'", userPath, resolved)
	}

	// 7. For write operations, check against sensitive file patterns
	if allowWrite {
		lowerResolved := strings.ToLower(resolved)
		for _, sp := range sensitivePathPatterns {
			if strings.Contains(lowerResolved, strings.ToLower(sp)) {
				return "", fmt.Errorf("write to sensitive path blocked: %s matches pattern %s", resolved, sp)
			}
		}
	}

	return resolved, nil
}

// IsSensitiveFile checks if a given path looks like a sensitive file.
func IsSensitiveFile(path string) bool {
	lower := strings.ToLower(path)
	for _, sp := range sensitivePathPatterns {
		if strings.Contains(lower, strings.ToLower(sp)) {
			return true
		}
	}
	return false
}
