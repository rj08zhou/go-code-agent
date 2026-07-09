package security

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// P0-1: Bash Policy - allowlist + danger patterns + confirmation

// BashPolicy defines which commands are allowed and which patterns are forbidden.
type BashPolicy struct {
	AllowCommands  map[string]bool // explicitly whitelisted base commands
	DangerPatterns []string        // patterns that are always blocked (even for allowed commands)
	RequireConfirm []string        // prefixes that trigger user confirmation prompt
	mu             sync.RWMutex
}

// DefaultBashPolicy is the process-wide bash execution policy.
var DefaultBashPolicy = &BashPolicy{
	AllowCommands: map[string]bool{
		// --- File read/inspect ---
		"ls": true, "cat": true, "head": true, "tail": true,
		"wc": true, "find": true, "grep": true, "rg": true,
		"diff": true, "file": true, "stat": true, "tree": true,
		"less": true, "more": true, "xxd": true, "hexdump": true,

		// --- File write (safe) ---
		"touch": true, "mkdir": true, "cp": true, "mv": true,
		"ln": true, "chmod": true, "chown": true,
		"tee": true, "echo": true, "printf": true,

		// --- Git ---
		"git": true,

		// --- Build / package managers ---
		"go": true, "make": true, "cmake": true, "gcc": true, "g++": true,
		"clang": true, "rustc": true, "cargo": true,
		"npm": true, "npx": true, "yarn": true, "pnpm": true,
		"pip": true, "pip3": true, "python": true, "python3": true,
		"ruby": true, "gem": true, "java": true, "javac": true,
		"mvn": true, "gradle": true, "dotnet": true,

		// --- Docker (restricted by DangerPatterns) ---
		"docker": true,

		// --- Network (read-only preferred) ---
		"curl": true, "wget": true, "ssh": true, "scp": true, "rsync": true,

		// --- System info (read-only) ---
		"uname": true, "whoami": true, "pwd": true, "env": true,
		"which": true, "whereis": true, "type": true,
		"date": true, "df": true, "du": true, "free": true,
		"top": true, "ps": true, "htop": true, "netstat": true,
		"ifconfig": true, "ip": true, "lscpu": true, "lsblk": true,

		// --- Text processing ---
		"sed": true, "awk": true, "sort": true, "uniq": true,
		"cut": true, "tr": true, "join": true, "paste": true,
		"jq": true, "yq": true,

		// --- Archival ---
		"tar": true, "gzip": true, "gunzip": true, "zip": true, "unzip": true,

		// --- Process management ---
		"kill": true, "killall": true, "pgrep": true,

		// --- Shell builtins ---
		"cd": true, "export": true, "source": true, "set": true,

		// --- Misc ---
		"yes": true, "true": true, "false": true, "test": true,
		"[": true, "basename": true, "dirname": true, "realpath": true,
		"readlink": true, "md5sum": true, "sha256sum": true,
	},
	DangerPatterns: []string{
		// Destructive filesystem
		"rm -rf /", "rm -rf /*", "rm -rf ~", "rm -rf ..",
		"rm -r /", "rm /*",
		"> /dev/sda", "> /dev/sdb", "> /dev/",
		"mkfs.", "format [a-z]:", "format /",
		"dd if=", "of=/dev/", ":(){ :|:& };",

		// Permission escalation
		"sudo ", "sudo\t", "doas ", "pkexec",

		// System control
		"shutdown", "reboot", "halt", "init 0", "init 6",
		"systemctl poweroff", "systemctl reboot",
		"chmod -R 777 /", "chmod -R 777 ~", "chmod 777 /",
		"chown -R ", "/etc/passwd", "/etc/shadow",

		// Remote code execution via pipe (disabled to avoid blocking normal agent bash)
		// "curl.*|.*sh", "wget.*|.*sh", "| sh", "| bash",
		// "eval $(", "eval `", "source <(",

		// Encoding tricks to hide commands (disabled)
		// "base64.*-d.*|", "base64.*-d.*sh",
		// "xargs ",

		// Network exfiltration risks (block raw socket / port redirect patterns that look malicious)
		"nc -l", "ncat -l", "nc -e", "/bin/sh -i",

		// History clearing (hides tracks)
		"history -c", "shred ", "wipe ",
	},
	RequireConfirm: []string{
		// File deletion
		"rm ", "rmdir ", "shred ",

		// Git force operations
		"git push --force", "git push -f", "git reset --hard",
		"git clean -f", "git branch -D",

		// Docker destructive
		"docker rm ", "docker rmi ", "docker system prune",
		"docker network rm", "docker volume rm",

		// Package publishing
		"npm publish", "cargo publish", "pip upload",
		"go mod tidy", // may change dependencies unexpectedly

		// Process kill
		"kill -9", "killall -9",

		// Permission changes on workdir root
		"chmod -R ",
	},
}

// Validate checks a command against the policy.
// Returns (allowed, needConfirm, reason).
func (p *BashPolicy) Validate(command string) (bool, bool, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false, false, "empty command"
	}

	// Extract first command token
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false, false, "empty command"
	}
	firstCmd := parts[0]

	// Check allowlist
	if !p.AllowCommands[firstCmd] {
		return false, false, fmt.Sprintf("command '%s' not in allowlist", firstCmd)
	}

	// Check danger patterns (case-insensitive)
	lowerCmd := strings.ToLower(cmd)
	for _, pat := range p.DangerPatterns {
		matched, _ := regexp.MatchString("(?i)"+pat, lowerCmd)
		if matched {
			return false, false, fmt.Sprintf("dangerous pattern detected: %s", pat)
		}
	}

	// User permission rules (permissions.json). Consulted ONLY here,
	// strictly AFTER the DangerPatterns blacklist above has already
	// returned - this ordering is the security invariant: a user
	// `allow` rule can relax confirmation but can never resurrect a
	// command the hard blacklist forbids (see permissions.go).
	//   deny  -> block outright
	//   allow -> auto-run (skip the RequireConfirm prompt below)
	//   ask   -> force confirmation regardless of RequireConfirm
	switch act, rule := GlobalPermissions.Match("bash", cmd); act {
	case PermDeny:
		return false, false, fmt.Sprintf("blocked by permission rule (deny): pattern %q", rule.Pattern)
	case PermAllow:
		return true, false, ""
	case PermAsk:
		return true, true, fmt.Sprintf("permission rule requests confirmation (ask): pattern %q", rule.Pattern)
	}

	// Check if needs confirmation
	for _, prefix := range p.RequireConfirm {
		matched, _ := regexp.MatchString("(?i)"+prefix, lowerCmd)
		if matched {
			return true, true, fmt.Sprintf("command requires confirmation: contains '%s'", prefix)
		}
	}

	return true, false, ""
}

// readOnlyBashCommands lists base commands whose normal usage does not
// modify the workspace. Used by the team's plan-gate to let read-only
// subagents (e.g. verifiers) run inspection commands without first
// going through submit_plan / lead approval.
//
// Anything not on this list is treated as a write operation and
// requires plan approval.
var readOnlyBashCommands = map[string]bool{
	// File read / inspect
	"ls": true, "cat": true, "head": true, "tail": true,
	"wc": true, "find": true, "grep": true, "rg": true,
	"diff": true, "file": true, "stat": true, "tree": true,
	"less": true, "more": true, "xxd": true, "hexdump": true,
	"realpath": true, "readlink": true, "basename": true, "dirname": true,
	"md5sum": true, "sha256sum": true,
	// System info (read-only)
	"uname": true, "whoami": true, "pwd": true, "env": true,
	"which": true, "whereis": true, "type": true,
	"date": true, "df": true, "du": true, "free": true,
	"ps": true, "pgrep": true, "lscpu": true, "lsblk": true,
	"netstat": true, "ifconfig": true, "ip": true,
	// Pure text processing (read stdin / files, write stdout)
	"sort": true, "uniq": true, "cut": true, "tr": true,
	"join": true, "paste": true, "jq": true, "yq": true,
	"awk": true, "sed": true, // operate on pipes by default; -i flag rejected below
	// Misc
	"true": true, "false": true, "test": true, "[": true,
	"echo": true, "printf": true, // emit to stdout; only redirect would make them write (rejected below)
}

// IsReadOnlyBash returns true when `command` is safe to run without
// plan approval: it must be allowed by DefaultBashPolicy (allowlist +
// danger checks), must not need confirmation, and its first token
// must be on readOnlyBashCommands. Shell features that can mutate
// state (>, >>, <, |, ;, &, &&, ||, $(...), `...`) are rejected so we
// don't get tricked by e.g. `ls > /tmp/x`.
func IsReadOnlyBash(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false
	}
	// Reject shell metacharacters that could introduce side effects.
	for _, bad := range []string{">", "<", "|", ";", "&", "$(", "`"} {
		if strings.Contains(cmd, bad) {
			return false
		}
	}
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false
	}
	if !readOnlyBashCommands[parts[0]] {
		return false
	}
	// `sed -i` and `awk -i inplace` mutate files; reject explicitly.
	if (parts[0] == "sed" || parts[0] == "awk") && hasInPlaceFlag(parts[1:]) {
		return false
	}
	allowed, needConfirm, _ := DefaultBashPolicy.Validate(cmd)
	if !allowed || needConfirm {
		return false
	}
	return true
}

func hasInPlaceFlag(args []string) bool {
	for i, a := range args {
		switch {
		case a == "-i", a == "--in-place", a == "--inplace":
			return true
		case strings.HasPrefix(a, "-i") && len(a) > 2 && a[2] != '-':
			// e.g. -i.bak
			return true
		case a == "inplace" && i > 0 && args[i-1] == "-i":
			return true
		}
	}
	return false
}
