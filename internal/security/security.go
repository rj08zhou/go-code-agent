// Package security provides path sandboxing, approval, and bash policy.
package security

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// ---------- Path Sandbox ----------

// SecurePath resolves a user-supplied path against root.
// Rel may be relative to root, or an absolute path that still lies under
// root. Absolute inputs must NOT be passed through filepath.Join with the
// root: as of Go 1.25, Join no longer discards prior elements when a later
// element is absolute (Join("/wd", "/Users/x") → "/wd/Users/x"), which
// silently turned valid absolute workspace paths into nonsense and caused
// explore agents to thrash on lstat failures.
// If allowWrite is false, the target must exist.
// Prevents symlink escapes and path traversal.
func SecurePath(root, rel string, allowWrite bool) (string, error) {
	if root == "" {
		return "", fmt.Errorf("no workdir set")
	}
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}

	var candidate string
	if filepath.IsAbs(rel) {
		candidate = filepath.Clean(rel)
	} else {
		candidate = filepath.Join(cleanRoot, rel)
	}
	clean, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	// Check prefix against cleanRoot
	if !strings.HasPrefix(clean, cleanRoot+string(filepath.Separator)) && clean != cleanRoot {
		return "", fmt.Errorf("path escapes workdir (use a path relative to %s, or an absolute path inside it): %s", cleanRoot, rel)
	}
	// Resolve symlinks in the resolved path, then re-check against the
	// symlink-resolved root to handle macOS /var→/private/var
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if !allowWrite {
			return "", fmt.Errorf("cannot resolve path %q under workdir %s: %w", rel, cleanRoot, err)
		}
	} else {
		resolvedRoot, rootErr := filepath.EvalSymlinks(cleanRoot)
		if rootErr == nil {
			if !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) && resolved != resolvedRoot {
				return "", fmt.Errorf("symlink escapes workdir: %s", rel)
			}
		}
	}
	if !allowWrite {
		if _, err := os.Stat(clean); err != nil {
			return "", fmt.Errorf("path not found: %s (workdir=%s)", rel, cleanRoot)
		}
	}
	return clean, nil
}

// IsReadOnlyBash reports whether a command is read-only/inspection-only
// under the default hard policy (no user permission rules).
func IsReadOnlyBash(cmd string) bool {
	return ClassifyCommand(cmd).Verdict == VerdictSafe
}

// ---------- Approval ----------

type ApprovalLevel int

const (
	ApproveAuto    ApprovalLevel = iota // no user input needed
	ApproveSafe                         // safe writes, always allow if auto-approve-safe
	ApproveDanger                       // potentially destructive, requires confirmation
	ApproveBlocked                      // never allowed
)

// ApprovalState is the session's auto-approve posture for tool risk levels.
// It answers "may this risk class run without prompting?" and whether
// file-mutation diff previews should be shown. Distinct from HITLManager,
// which owns the effective manual/safe-auto/all-auto decision mode.
// It is safe for concurrent use.
type ApprovalState struct {
	mu              sync.RWMutex
	autoApproveAll  bool
	autoApproveSafe bool
}

// NewApprovalState returns a fresh state with both auto-approve flags off.
func NewApprovalState() *ApprovalState { return &ApprovalState{} }

func (s *ApprovalState) SetAutoApproveAll(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoApproveAll = v
}
func (s *ApprovalState) SetAutoApproveSafe(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoApproveSafe = v
}
func (s *ApprovalState) IsAutoApproveAll() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.autoApproveAll
}
func (s *ApprovalState) IsAutoApproveSafe() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.autoApproveSafe
}

// ShouldPreviewDiff reports whether file mutation previews should be shown.
// Diff preview is skipped only when the user has opted into full auto-approve.
func (s *ApprovalState) ShouldPreviewDiff() bool { return !s.IsAutoApproveAll() }

// ApplyPreset sets the auto-approval and diff-preview posture. Legacy preset
// names remain accepted for compatibility with stored/internal callers.
func (s *ApprovalState) ApplyPreset(preset string) {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "all-auto", "danger", "all":
		s.SetAutoApproveSafe(true)
		s.SetAutoApproveAll(true)
	case "safe-auto", "safe":
		s.SetAutoApproveSafe(true)
		s.SetAutoApproveAll(false)
	default: // "manual", "off", "reset", or unknown
		s.SetAutoApproveSafe(false)
		s.SetAutoApproveAll(false)
	}
}

func (s *ApprovalState) Decide(level ApprovalLevel, desc string) (allowed bool, reason string) {
	switch level {
	case ApproveAuto:
		return true, ""
	case ApproveSafe:
		if s.IsAutoApproveAll() || s.IsAutoApproveSafe() {
			return true, ""
		}
		return false, fmt.Sprintf("[safe] %s requires approval. Use /approval safe-auto to auto-approve lower-risk reviews.", desc)
	case ApproveDanger:
		if s.IsAutoApproveAll() {
			return true, ""
		}
		return false, fmt.Sprintf("[DANGER] %s requires confirmation. Use /approval all-auto confirm to bypass prompts (risky!).", desc)
	case ApproveBlocked:
		return false, fmt.Sprintf("BLOCKED: %s is not permitted", desc)
	default:
		return false, fmt.Sprintf("unknown approval level for %q", desc)
	}
}

// ---------- Bash Policy ----------

const (
	MCPToolPrefix   = "mcp__"
	MCPDefaultLevel = ApproveSafe
)

// allowedCommands is the whitelist of permitted base commands.
var allowedCommands = map[string]bool{
	"ls": true, "ll": true, "la": true, "pwd": true, "cd": true,
	"cat": true, "head": true, "tail": true, "less": true, "more": true,
	"wc": true, "sort": true, "uniq": true, "cut": true, "tr": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "awk": true, "sed": true,
	"find": true, "locate": true, "which": true, "whereis": true, "type": true,
	"stat": true, "tree": true, "jq": true, "kill": true, "pgrep": true,
	"echo": true, "printf": true, "date": true, "env": true, "printenv": true,
	"uname": true, "hostname": true, "whoami": true, "id": true, "groups": true,
	"ps": true, "top": true, "htop": true, "df": true, "du": true, "free": true,
	"mkdir": true, "touch": true, "cp": true, "mv": true, "rm": true,
	"rmdir": true, "ln": true, "chmod": true, "chown": true,
	"git": true, "go": true, "make": true, "cmake": true, "gcc": true, "g++": true,
	"python": true, "python3": true, "pip": true, "pip3": true, "node": true, "npm": true, "npx": true,
	"curl": true, "wget": true, "nc": true, "ping": true, "dig": true, "nslookup": true,
	"docker": true, "docker-compose": true, "kubectl": true, "helm": true,
	"tar": true, "gzip": true, "gunzip": true, "zip": true, "unzip": true,
	"ssh": true, "scp": true, "rsync": true,
	"diff": true, "patch": true, "xxd": true, "hexdump": true, "file": true,
	"test": true, "timeout": true, "xargs": true, "tee": true,
	"cargo": true, "rustc": true, "javac": true, "java": true, "mvn": true, "gradle": true,
	"sqlite3": true, "psql": true, "mysql": true,
	"openssl": true, "base64": true, "md5": true, "md5sum": true, "shasum": true,
}

// dangerousRegexps are patterns that always deny a command.
var dangerousRegexps = []*regexp.Regexp{
	regexp.MustCompile(`rm\s+-rf\s+/`),
	regexp.MustCompile(`rm\s+-rf\s+\$`),
	regexp.MustCompile(`:\(\)\s*{`), // fork bomb
	regexp.MustCompile(`>\s*/dev/sd[a-z]`),
	regexp.MustCompile(`mkfs\.`),
	regexp.MustCompile(`dd\s+if=`),
	regexp.MustCompile(`shutdown(\s|$)`),
	regexp.MustCompile(`reboot(\s|$)`),
	// Privilege escalation — block any sudo/doas/pkexec, not only destructive forms.
	regexp.MustCompile(`(^|[\s;&|])sudo(\s|$)`),
	regexp.MustCompile(`(^|[\s;&|])doas(\s|$)`),
	regexp.MustCompile(`(^|[\s;&|])pkexec(\s|$)`),
	regexp.MustCompile(`chmod\s+777\s+/`),
	regexp.MustCompile(`wget\s+\S+\s*-O\s+/`),
	regexp.MustCompile(`curl\s+\S+\s*-o\s+/`),
	regexp.MustCompile(`\|(\s*)sh(\s|$)`),
	regexp.MustCompile(`\|(\s*)bash(\s|$)`),
	regexp.MustCompile(`\bnc\s+-l\b`),
	regexp.MustCompile(`\bhistory\s+-c\b`),
}

// confirmRegexps are patterns that require user confirmation.
var confirmRegexps = []*regexp.Regexp{
	regexp.MustCompile(`rm\s+`),
	regexp.MustCompile(`git\s+push\s+(-f|--force)`),
	regexp.MustCompile(`git\s+reset\s+--hard`),
	regexp.MustCompile(`git\s+clean\s+-f`),
	regexp.MustCompile(`git\s+checkout\s+--\s+`),
	regexp.MustCompile(`sed\s+(-i|--in-place)`),
	regexp.MustCompile(`docker\s+(rm|rmi|kill|stop)\s`),
	regexp.MustCompile(`docker\s+system\s+prune`),
	regexp.MustCompile(`npm\s+publish`),
	regexp.MustCompile(`cargo\s+publish`),
	regexp.MustCompile(`pip\s+uninstall`),
	regexp.MustCompile(`npm\s+uninstall`),
	regexp.MustCompile(`kubectl\s+delete`),
	regexp.MustCompile(`terraform\s+(apply|destroy)`),
	regexp.MustCompile(`chmod\s+`),
	regexp.MustCompile(`chown\s+`),
	regexp.MustCompile(`>(\s*)/`),
}

// defaultDenyPatterns / defaultConfirmPatterns are the string-based pattern
// lists consumed by ClassifyCommand (classify.go), the single source of
// command-risk truth.
var defaultDenyPatterns = []string{
	"| sh", "| bash",
	"rm -r /",
	"base64 -d |",
	"docker run", "mkfs.", "dd if=",
	"> /dev/sd", "shutdown", "chmod 777 /",
	"/etc/shadow", "/etc/passwd",
}

var defaultConfirmPatterns = []string{
	"git push --force", "git push -f",
	"git reset --hard", "git clean -f",
}

// BashPolicy is the policy gate for shell commands. Risk classification is
// fully delegated to ClassifyCommand; this type only layers session-scoped
// user permission rules on top of the intrinsic verdict.
type BashPolicy struct{}

func NewDefaultBashPolicy() *BashPolicy { return &BashPolicy{} }

// Validate maps the ClassifyCommand verdict onto the (allowed, needConfirm)
// contract and then applies optional session-scoped user rules (may be nil).
// Hard deny verdicts always win before user rules are considered.
func (p *BashPolicy) Validate(command string, perms *Permissions) (allowed bool, needConfirm bool, reason string) {
	c := ClassifyCommand(command)
	switch c.Verdict {
	case VerdictDeny:
		return false, false, c.Reason
	case VerdictDanger:
		return true, true, c.Reason
	}

	// Session-scoped user permission rules (injected by the composition root).
	if perms != nil {
		switch perms.Match("bash", strings.TrimSpace(command)) {
		case "block":
			return false, false, "blocked by user permission rule"
		case "confirm":
			return true, true, "user permission rule requires confirmation"
		}
	}

	return true, false, ""
}

// ReadLine reads a line from stdin. Replaceable for testing via SetReadLine.
var readLineFn atomicReadLine

type atomicReadLine struct {
	fn atomic.Value // stores func(prompt string) (string, error)
}

func (a *atomicReadLine) Load() func(prompt string) (string, error) {
	v := a.fn.Load()
	if v == nil {
		return defaultReadLine
	}
	return v.(func(string) (string, error))
}

func (a *atomicReadLine) Store(fn func(prompt string) (string, error)) {
	a.fn.Store(fn)
}

var defaultReadLine = func(prompt string) (string, error) {
	fmt.Print(prompt)
	var line string
	_, err := fmt.Scanln(&line)
	return line, err
}

// ReadLine calls the current ReadLine function.
func ReadLine(prompt string) (string, error) {
	return readLineFn.Load()(prompt)
}

// SetReadLine replaces the ReadLine function for testing.
func SetReadLine(fn func(string) (string, error)) {
	readLineFn.Store(fn)
}
