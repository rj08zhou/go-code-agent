package security

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// User-editable permission rules.
//
// This is the "parameter-aware" layer the static ApprovalLevel model
// (approval.go) lacks: ApprovalState.Decide only sees a tool's Level
// (e.g. "all bash is ApproveDanger"), never the concrete arguments, so
// it cannot express "allow `git commit -m *` but deny `git push
// --force*`". PermissionRules closes that gap with an ordered list of
// {tool, pattern, action} rules loaded from a user-editable
// permissions.json.
//
// SECURITY INVARIANT (critical):
//
//	Permission rules can only be consulted AFTER the hard, non-
//	overridable safety checks have already passed - specifically
//	BashPolicy.Validate checks its DangerPatterns blacklist (rm -rf /,
//	sudo, fork bombs, ...) and returns BEFORE any user rule is applied.
//	A user `allow` rule therefore can NEVER unblock something the hard
//	blacklist forbids; it can only relax "needs confirmation" to
//	"auto-run", or (via `deny`/`ask`) tighten an otherwise-permitted
//	call. Placement, not policy, enforces this - see the call order in
//	BashPolicy.Validate and checkToolApproval.
//
// Matching:
//   - Rules are evaluated top-to-bottom; the FIRST rule whose tool and
//     pattern both match wins (users control precedence via ordering).
//   - Tool match: rule.Tool == "*" matches any tool, otherwise it is a
//     wildcard match against the tool name (so "mcp__*" matches all
//     tools from any MCP server).
//   - Pattern match: a wildcard match (see wildcardMatch) against the
//     tool's most relevant argument - the command for bash, the path
//     for file tools, the raw arguments JSON otherwise. Unlike
//     path.Match, '*' here spans ANY character including '/', which is
//     what users intuitively expect for both command lines
//     ("rm -rf *") and paths ("*/migrations/*").

// PermAction is the outcome of matching a tool call against the rules.
type PermAction int

const (
	// PermNoMatch: no rule matched; the caller falls back to its
	// normal (Level-based / BashPolicy) decision.
	PermNoMatch PermAction = iota
	// PermAllow: auto-approve this call (skip the Level gate / skip the
	// bash confirmation prompt). Cannot bypass hard safety checks.
	PermAllow
	// PermDeny: block this call outright.
	PermDeny
	// PermAsk: force this call through confirmation/approval even if it
	// would otherwise be auto-approved.
	PermAsk
)

func (a PermAction) String() string {
	switch a {
	case PermAllow:
		return "allow"
	case PermDeny:
		return "deny"
	case PermAsk:
		return "ask"
	default:
		return "no-match"
	}
}

// PermissionRule is a single user-authored rule.
type PermissionRule struct {
	Tool    string `json:"tool"`    // tool name or "*"; wildcard-matched
	Pattern string `json:"pattern"` // wildcard pattern against the tool's key arg
	Action  string `json:"action"`  // "allow" | "deny" | "ask"
}

// action parses the rule's textual Action into a PermAction. Unknown
// actions return PermNoMatch so a malformed rule is inert rather than
// silently allowing/denying.
func (r PermissionRule) action() PermAction {
	switch strings.ToLower(strings.TrimSpace(r.Action)) {
	case "allow":
		return PermAllow
	case "deny":
		return PermDeny
	case "ask":
		return PermAsk
	default:
		return PermNoMatch
	}
}

// PermissionRules is the process-wide, concurrency-safe rule set.
type PermissionRules struct {
	mu    sync.RWMutex
	rules []PermissionRule
	path  string // source file, for /permissions reload + diagnostics
}

// GlobalPermissions is the process-wide singleton, mirroring the
// pattern of GlobalApproval / DefaultBashPolicy. Empty (no rules) until
// Load is called, so absent a permissions.json every call is
// PermNoMatch and behavior is exactly as before.
var GlobalPermissions = &PermissionRules{}

// permissionsFile is the schema of permissions.json.
type permissionsFile struct {
	Rules []PermissionRule `json:"rules"`
}

// Load reads and installs rules from path. A missing file is not an
// error (rules become empty). Malformed JSON is an error and leaves the
// previous rules untouched. Rules with an unrecognized action are
// dropped with the count reported via the returned warning string.
func (pr *PermissionRules) Load(path string) (warning string, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			pr.mu.Lock()
			pr.rules = nil
			pr.path = path
			pr.mu.Unlock()
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, readErr)
	}

	var pf permissionsFile
	if e := json.Unmarshal(data, &pf); e != nil {
		return "", fmt.Errorf("parse %s: %w", path, e)
	}

	valid := make([]PermissionRule, 0, len(pf.Rules))
	dropped := 0
	for _, r := range pf.Rules {
		if r.action() == PermNoMatch || strings.TrimSpace(r.Tool) == "" || strings.TrimSpace(r.Pattern) == "" {
			dropped++
			continue
		}
		valid = append(valid, r)
	}

	pr.mu.Lock()
	pr.rules = valid
	pr.path = path
	pr.mu.Unlock()

	if dropped > 0 {
		warning = fmt.Sprintf("%d permission rule(s) ignored (empty tool/pattern or unknown action - valid actions: allow|deny|ask)", dropped)
	}
	return warning, nil
}

// Set replaces the rule set directly. Primarily for tests.
func (pr *PermissionRules) Set(rules []PermissionRule) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	pr.rules = rules
}

// Match returns the action of the first rule matching (tool, target)
// plus that rule (for diagnostics). target is the tool's key argument
// (command / path / raw JSON) chosen by the caller. Returns PermNoMatch
// when nothing matches.
func (pr *PermissionRules) Match(tool, target string) (PermAction, PermissionRule) {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	for _, r := range pr.rules {
		if !toolMatches(r.Tool, tool) {
			continue
		}
		if !wildcardMatch(r.Pattern, target) {
			continue
		}
		return r.action(), r
	}
	return PermNoMatch, PermissionRule{}
}

// Count returns the number of active rules.
func (pr *PermissionRules) Count() int {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	return len(pr.rules)
}

// Path returns the source file path (for /permissions reload).
func (pr *PermissionRules) Path() string {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	return pr.path
}

// Describe renders the active rules for the /permissions command.
func (pr *PermissionRules) Describe() string {
	pr.mu.RLock()
	defer pr.mu.RUnlock()
	if len(pr.rules) == 0 {
		src := pr.path
		if src == "" {
			src = "(no file loaded)"
		}
		return fmt.Sprintf("No permission rules active. Source: %s", src)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Permission rules (%d, first match wins) from %s:\n", len(pr.rules), pr.path)
	for i, r := range pr.rules {
		fmt.Fprintf(&b, "  %d. %-6s tool=%-14s pattern=%q\n", i+1, r.action(), r.Tool, r.Pattern)
	}
	b.WriteString("Note: hard bash DangerPatterns (rm -rf /, sudo, ...) always block regardless of rules.")
	return b.String()
}

// toolMatches reports whether a rule's Tool field matches the concrete
// tool name. "*" matches everything; otherwise it is a wildcard match
// (so "mcp__*" covers every MCP tool, and a plain "bash" is an exact
// match since it contains no wildcards).
func toolMatches(ruleTool, tool string) bool {
	if ruleTool == "*" {
		return true
	}
	return wildcardMatch(ruleTool, tool)
}

// wildcardMatch reports whether s matches pattern, where '*' matches
// any run of characters (including empty and including '/') and '?'
// matches exactly one character. Everything else is literal.
//
// This is deliberately NOT path.Match: path.Match's '*' stops at '/',
// which breaks the two things users most want to match here -
// command lines that contain paths ("rm -rf /tmp/x" vs "rm -rf *") and
// nested path globs ("*/migrations/*"). A classic linear-time
// two-pointer matcher with backtracking gives the intuitive "'*' spans
// anything" semantics with no dependency and no catastrophic-backtrack
// risk (unlike user-supplied regexes).
func wildcardMatch(pattern, s string) bool {
	var (
		p, str       = 0, 0 // current indices into pattern / s
		star         = -1   // index in pattern of the last '*'
		strAfterStar = 0    // index in s to resume from on backtrack
	)
	for str < len(s) {
		if p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[str]) {
			p++
			str++
		} else if p < len(pattern) && pattern[p] == '*' {
			star = p
			strAfterStar = str
			p++
		} else if star != -1 {
			// Backtrack: let the last '*' absorb one more char of s.
			p = star + 1
			strAfterStar++
			str = strAfterStar
		} else {
			return false
		}
	}
	// Consume any trailing '*'s in the pattern.
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
