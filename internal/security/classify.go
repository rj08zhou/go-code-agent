package security

import (
	"fmt"
	"strings"
)

// Verdict is the single source of truth for shell-command risk across the
// codebase. Both the bash policy gate (BashPolicy.Validate) and the HITL
// review gate (hitlaudit.NeedsReview) consume ClassifyCommand; neither may
// implement its own command parsing or risk rules.
type Verdict int

const (
	// VerdictSafe: read-only / inspection-only. May run without review.
	VerdictSafe Verdict = iota
	// VerdictCaution: has side effects but no known dangerous pattern.
	// Runs without confirmation under the policy gate; HITL (when enabled)
	// reviews it at medium risk.
	VerdictCaution
	// VerdictDanger: matches a destructive pattern or carries a sensitive
	// environment override. Requires confirmation.
	VerdictDanger
	// VerdictDeny: never allowed.
	VerdictDeny
)

func (v Verdict) String() string {
	switch v {
	case VerdictSafe:
		return "safe"
	case VerdictCaution:
		return "caution"
	case VerdictDanger:
		return "danger"
	case VerdictDeny:
		return "deny"
	default:
		return "unknown"
	}
}

// Classification is the result of ClassifyCommand.
type Classification struct {
	Verdict Verdict
	Reason  string
}

// sensitiveEnvVars are environment variables whose assignment as a command
// prefix can change what code the command executes (library injection,
// interpreter startup files, lookup-path hijack). Assignments of these are
// NEVER stripped-and-ignored: they escalate the command to VerdictDanger.
var sensitiveEnvVars = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "DYLD_INSERT_LIBRARIES": true,
	"DYLD_LIBRARY_PATH": true, "PATH": true, "IFS": true,
	"BASH_ENV": true, "ENV": true, "SHELLOPTS": true,
	"PYTHONPATH": true, "PYTHONSTARTUP": true, "NODE_OPTIONS": true,
	"PERL5LIB": true, "RUBYOPT": true, "GIT_SSH_COMMAND": true,
	"GIT_PAGER": true, "PAGER": true, "EDITOR": true, "VISUAL": true,
}

// readOnlyPrefixes lists token-prefix patterns considered read-only.
// Multi-word entries (e.g. "go test") must match leading tokens exactly.
var readOnlyPrefixes = []string{
	"ls", "ll", "la", "pwd", "cd",
	"cat", "head", "tail", "less", "more",
	"grep", "egrep", "fgrep", "rg", "ag",
	"find", "locate", "which", "whereis", "type",
	"echo", "printf", "date", "whoami", "hostname", "uname",
	"go test", "go build", "go vet", "go run", "go list", "go doc", "go mod",
	"git status", "git log", "git diff", "git show", "git branch",
	"wc", "stat", "file", "du", "df", "env", "printenv",
	"sort", "uniq", "cut", "tr", "jq", "tree", "diff",
}

// ClassifyCommand returns the risk verdict for a shell command string.
//
// Order of precedence (first match wins):
//  1. deny patterns (hard destructive / privilege escalation)
//  2. base-command whitelist (unknown executables are denied)
//  3. sensitive environment-variable assignment prefixes → danger
//  4. confirm patterns (destructive-but-confirmable) → danger
//  5. every pipeline segment read-only → safe
//  6. otherwise → caution
//
// Session-scoped user permission rules are NOT applied here; they are
// layered on top by BashPolicy.Validate, because they express user intent,
// not intrinsic command risk.
func ClassifyCommand(command string) Classification {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return Classification{VerdictDeny, "empty command"}
	}
	lower := strings.ToLower(cmd)

	// 1. Hard deny patterns run against the raw string so obfuscation via
	// subshells/quoting inside segments cannot dodge them.
	for _, re := range dangerousRegexps {
		if re.MatchString(lower) {
			return Classification{VerdictDeny, fmt.Sprintf("dangerous command blocked: %q", cmd)}
		}
	}
	for _, pat := range defaultDenyPatterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return Classification{VerdictDeny, fmt.Sprintf("dangerous command pattern blocked: %q", pat)}
		}
	}

	segments := splitShellPipeline(cmd)
	if len(segments) == 0 {
		return Classification{VerdictDeny, "empty command"}
	}

	// 2+3. Per-segment: strip env-assignment prefixes (tracking sensitive
	// ones) and check the executable against the whitelist.
	sensitiveEnv := ""
	for _, seg := range segments {
		tokens := tokenizeShellWords(seg)
		for len(tokens) > 0 && isEnvAssignment(tokens[0]) {
			name := tokens[0][:strings.IndexByte(tokens[0], '=')]
			if sensitiveEnvVars[strings.ToUpper(name)] && sensitiveEnv == "" {
				sensitiveEnv = name
			}
			tokens = tokens[1:]
		}
		if len(tokens) == 0 {
			continue // pure assignment segment, e.g. "FOO=1"
		}
		exe := tokens[0]
		if strings.HasPrefix(exe, "./") || strings.HasPrefix(exe, "/") {
			continue // path-based executables: treated as project tools
		}
		if !allowedCommands[strings.ToLower(exe)] {
			return Classification{VerdictDeny, fmt.Sprintf("command %q is not in the allowed list", exe)}
		}
	}
	if sensitiveEnv != "" {
		return Classification{VerdictDanger,
			fmt.Sprintf("sensitive environment override %q requires confirmation", sensitiveEnv)}
	}

	// 4. Confirmable destructive patterns.
	for _, re := range confirmRegexps {
		if re.MatchString(lower) {
			return Classification{VerdictDanger, fmt.Sprintf("potentially dangerous: %q", cmd)}
		}
	}
	for _, pat := range defaultConfirmPatterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return Classification{VerdictDanger, fmt.Sprintf("potentially dangerous: %q", pat)}
		}
	}

	// 5. Read-only if every segment matches the read-only prefix table.
	if allSegmentsReadOnly(segments) {
		return Classification{VerdictSafe, "read-only/inspection-only"}
	}

	// 6. Default: side effects possible, no known dangerous pattern.
	return Classification{VerdictCaution, "command has side effects; no dangerous pattern matched"}
}

// allSegmentsReadOnly reports whether every pipeline segment is read-only.
func allSegmentsReadOnly(segments []string) bool {
	for _, seg := range segments {
		if !isReadOnlySegment(seg) {
			return false
		}
	}
	return true
}

func isReadOnlySegment(seg string) bool {
	tokens := tokenizeShellWords(seg)
	// Env prefixes were already vetted for sensitive vars by the caller;
	// plain assignments (GOOS=linux go build) don't affect read-only-ness.
	for len(tokens) > 0 && isEnvAssignment(tokens[0]) {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return false
	}
	// sed is read-only only in explicit -n (no in-place) mode.
	if tokens[0] == "sed" {
		for _, t := range tokens[1:] {
			if t == "-n" || strings.HasPrefix(t, "-n") {
				return true
			}
		}
		return false
	}
	for _, p := range readOnlyPrefixes {
		pTokens := strings.Fields(p)
		if len(pTokens) == 0 || len(tokens) < len(pTokens) {
			continue
		}
		match := true
		for i, pt := range pTokens {
			if tokens[i] != pt {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// isEnvAssignment reports whether tok looks like NAME=value with a valid
// shell identifier as NAME.
func isEnvAssignment(tok string) bool {
	if tok == "" {
		return false
	}
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i := 0; i < eq; i++ {
		c := tok[i]
		if !(c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// splitShellPipeline splits a command on |, ||, &&, ;, & and newlines,
// respecting single/double quotes and backslash escapes.
func splitShellPipeline(cmd string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		s := strings.TrimSpace(cur.String())
		if s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\\':
			cur.WriteByte(c)
			if i+1 < len(cmd) {
				i++
				cur.WriteByte(cmd[i])
			}
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '&':
			prev := byte(0)
			if cur.Len() > 0 {
				prev = cur.String()[cur.Len()-1]
			}
			if prev == '>' || prev == '<' {
				cur.WriteByte(c)
				continue
			}
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				flush()
				i++
			} else {
				flush()
			}
		case '|':
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				flush()
				i++
			} else {
				flush()
			}
		case ';', '\n':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}

// tokenizeShellWords splits a single command segment into words, respecting
// quotes and backslash escapes.
func tokenizeShellWords(cmd string) []string {
	var out []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(cmd) {
				i++
				cur.WriteByte(cmd[i])
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			flush()
		case '\'', '"':
			quote = c
		case '\\':
			if i+1 < len(cmd) {
				i++
				cur.WriteByte(cmd[i])
			}
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}
