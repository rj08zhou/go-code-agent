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
	p := NewDefaultBashPolicy()
	allowed, needConfirm, _ := p.Validate(cmd, nil)
	return allowed && !needConfirm
}

// ---------- Approval ----------

type ApprovalLevel int

const (
	ApproveAuto    ApprovalLevel = iota // no user input needed
	ApproveSafe                         // safe writes, always allow if auto-approve-safe
	ApproveDanger                       // potentially destructive, requires confirmation
	ApproveBlocked                      // never allowed
)

// ApprovalState tracks the user's current approval posture for a session.
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

// ApplyPreset sets the posture for /approve off|safe|danger.
func (s *ApprovalState) ApplyPreset(preset string) {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "danger", "all":
		s.SetAutoApproveSafe(true)
		s.SetAutoApproveAll(true)
	case "safe":
		s.SetAutoApproveSafe(true)
		s.SetAutoApproveAll(false)
	default: // "off", "reset", or unknown → manual
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
		return false, fmt.Sprintf("[safe] %s requires approval. Use /approve safe to auto-approve.", desc)
	case ApproveDanger:
		if s.IsAutoApproveAll() {
			return true, ""
		}
		return false, fmt.Sprintf("[DANGER] %s requires confirmation. Use /approve danger to auto-approve (risky!).", desc)
	case ApproveBlocked:
		return false, fmt.Sprintf("BLOCKED: %s is not permitted", desc)
	default:
		return false, fmt.Sprintf("unknown approval level for %q", desc)
	}
}

// Session-scoped active approval used by diff-preview and /approve.
// Defaults to a non-nil empty state so callers never panic.
var (
	activeApprovalMu sync.RWMutex
	activeApproval   = NewApprovalState()
)

// SetActiveApproval installs the session's ApprovalState as the process-wide
// active posture (diff preview, legacy SetAutoApproveAll). Pass nil to reset.
func SetActiveApproval(s *ApprovalState) {
	activeApprovalMu.Lock()
	defer activeApprovalMu.Unlock()
	if s == nil {
		activeApproval = NewApprovalState()
		return
	}
	activeApproval = s
}

// ActiveApproval returns the session ApprovalState.
func ActiveApproval() *ApprovalState {
	activeApprovalMu.RLock()
	defer activeApprovalMu.RUnlock()
	return activeApproval
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

// BashPolicy validates shell commands against an allow/deny/confirm model.
type BashPolicy struct {
	denyPatterns    []string
	confirmPatterns []string
}

func NewDefaultBashPolicy() *BashPolicy {
	return &BashPolicy{
		denyPatterns: []string{
			"| sh", "| bash",
			"rm -r /",
			"base64 -d |",
			"docker run", "mkfs.", "dd if=",
			"> /dev/sd", "shutdown", "chmod 777 /",
			"/etc/shadow", "/etc/passwd",
		},
		confirmPatterns: []string{
			"git push --force", "git push -f",
			"git reset --hard", "git clean -f",
		},
	}
}

// Validate checks the command against the allow/deny/confirm lists.
// perms is optional session-scoped user rules (may be nil); hard deny
// patterns always win before user allow/confirm rules are considered.
func (p *BashPolicy) Validate(command string, perms *Permissions) (allowed bool, needConfirm bool, reason string) {
	cmd := strings.TrimSpace(command)
	lower := strings.ToLower(cmd)

	// 1. Extract base command and check against whitelist
	base := extractBaseCommand(cmd)
	if base != "" && !allowedCommands[strings.ToLower(base)] {
		if strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "/") {
			// Path-based executables: allow if they look like project tools
		} else {
			return false, false, fmt.Sprintf("command %q is not in the allowed list", base)
		}
	}

	// 2. Check regex-based dangerous patterns
	for _, re := range dangerousRegexps {
		if re.MatchString(lower) {
			return false, false, fmt.Sprintf("dangerous command blocked: %q", cmd)
		}
	}

	// 3. Check string-based deny patterns (backward compatibility)
	for _, pat := range p.denyPatterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return false, false, fmt.Sprintf("dangerous command pattern blocked: %q", pat)
		}
	}

	// 4. Check regex-based confirm patterns
	for _, re := range confirmRegexps {
		if re.MatchString(lower) {
			return true, true, fmt.Sprintf("potentially dangerous: %q", cmd)
		}
	}

	// 5. Check string-based confirm patterns
	for _, pat := range p.confirmPatterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return true, true, fmt.Sprintf("potentially dangerous: %q", pat)
		}
	}

	// 6. Session-scoped user permission rules (injected by the composition root).
	if perms != nil {
		switch perms.Match("bash", cmd) {
		case "block":
			return false, false, "blocked by user permission rule"
		case "confirm":
			return true, true, "user permission rule requires confirmation"
		}
	}

	return true, false, ""
}

func matchPattern(cmd, pattern string) bool {
	return strings.Contains(strings.ToLower(cmd), strings.ToLower(strings.TrimSpace(pattern)))
}

// extractBaseCommand returns the first word of a shell command after stripping pipes and redirects.
func extractBaseCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	// Handle compound commands
	if strings.HasPrefix(cmd, "cd ") || strings.HasPrefix(cmd, "export ") {
		return cmd[:strings.Index(cmd, " ")]
	}
	// Split on first space
	first := cmd
	if idx := strings.IndexByte(cmd, ' '); idx > 0 {
		first = cmd[:idx]
	} else if idx := strings.IndexByte(cmd, ';'); idx > 0 {
		first = strings.TrimSpace(cmd[:idx])
	}
	return first
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
