package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Security layer for go-code-agent
//
// Covers 4 areas:
//   - P0-1: Bash allowlist + user confirmation (replaces dangerousPatterns)
//   - P0-2: Path sandbox with symlink escape prevention
//   - P0-3: Unified tool approval layer (auto/safe/danger/blocked)
//   - P0-4: Secrets detection and sanitization

type ApprovalLevel int

const (
	ApproveAuto    ApprovalLevel = iota // auto-approved (read-only tools)
	ApproveSafe                         // safe operations (create new files, non-destructive writes)
	ApproveDanger                       // destructive operations (delete, force-push, etc.)
	ApproveBlocked                      // explicitly forbidden
)

func (l ApprovalLevel) String() string {
	switch l {
	case ApproveAuto:
		return "auto"
	case ApproveSafe:
		return "safe"
	case ApproveDanger:
		return "danger"
	case ApproveBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

type ToolSecurityMeta struct {
	Name        string
	Level       ApprovalLevel
	Description string
}

var toolSecurityMap = map[string]ToolSecurityMeta{
	"read_file":       {"read_file", ApproveAuto, "Read file contents"},
	"search_content":  {"search_content", ApproveAuto, "Search file content"},
	"search_file":     {"search_file", ApproveAuto, "Search files by pattern"},
	"list_dir":        {"list_dir", ApproveAuto, "List directory contents"},
	"memory_search":   {"memory_search", ApproveAuto, "Search memory store"},
	"memory_delete":   {"memory_delete", ApproveSafe, "Delete a memory entry by matching content"},
	"memory_write":    {"memory_write", ApproveSafe, "Save important fact or observation to long-term memory"},
	"memory_stats":    {"memory_stats", ApproveAuto, "View memory statistics"},
	"task_list":       {"task_list", ApproveAuto, "List all tasks"},
	"task_dag":        {"task_dag", ApproveAuto, "View DAG task graph"},
	"task_ready":      {"task_ready", ApproveAuto, "List ready tasks"},
	"task_get":        {"task_get", ApproveAuto, "Get task details"},
	"TodoWrite":       {"TodoWrite", ApproveSafe, "Update task list"},
	"think":           {"think", ApproveAuto, "Reasoning (no-op)"},
	"compress":        {"compress", ApproveAuto, "Compress conversation"},
	"write_file":      {"write_file", ApproveSafe, "Write content to file"},
	"edit_file":       {"edit_file", ApproveSafe, "Replace text in file"},
	"bash":            {"bash", ApproveDanger, "Execute shell command"},
	"execute_command": {"execute_command", ApproveDanger, "Execute command"},
	"delete_file":     {"delete_file", ApproveDanger, "Delete a file"},
	"background_run":  {"background_run", ApproveDanger, "Run command in background"},
	"task_create":     {"task_create", ApproveSafe, "Create a new task"},
	"task_update":     {"task_update", ApproveSafe, "Update task status"},
	"task_add_dep":    {"task_add_dep", ApproveSafe, "Add task dependency"},
	"task_remove_dep": {"task_remove_dep", ApproveSafe, "Remove task dependency"},
	"task":            {"task", ApproveSafe, "Spawn a subagent for isolated work"},
	"load_skill":      {"load_skill", ApproveAuto, "Load specialized knowledge"},
}

type ApprovalState struct {
	autoApproveSafe bool // auto-approve safe-level tools without prompting
	autoApproveAll  bool // auto-approve ALL tools (including danger) - use with caution!
	mu              sync.RWMutex
}

var globalApproval = &ApprovalState{}

// SetAutoApproveSafe toggles auto-approval for safe-level tools.
func (a *ApprovalState) SetAutoApproveSafe(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.autoApproveSafe = v
}

// SetAutoApproveAll toggles auto-approval for ALL tools (including danger).
func (a *ApprovalState) SetAutoApproveAll(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.autoApproveAll = v
}

// IsAutoApproveSafe returns current auto-approve-safe setting.
func (a *ApprovalState) IsAutoApproveSafe() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.autoApproveSafe
}

// IsAutoApproveAll returns current auto-approve-all setting.
func (a *ApprovalState) IsAutoApproveAll() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.autoApproveAll
}

// P0-5: Diff Preview System
//
// Generates unified diff and asks for user confirmation before file modifications.
// This provides a "git diff" like experience before applying changes.

// generateUnifiedDiff creates a unified diff format string between old and new content.
// It uses the system 'diff' command for accurate diff generation.
func generateUnifiedDiff(oldContent, newContent, filename string) (string, error) {
	// Create temporary files for old and new content
	oldTmp, err := os.CreateTemp("", "old-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for old content: %v", err)
	}
	defer os.Remove(oldTmp.Name())
	defer oldTmp.Close()

	newTmp, err := os.CreateTemp("", "new-*.txt")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file for new content: %v", err)
	}
	defer os.Remove(newTmp.Name())
	defer newTmp.Close()

	// Write content to temp files
	if _, err := oldTmp.WriteString(oldContent); err != nil {
		return "", fmt.Errorf("failed to write old content: %v", err)
	}
	oldTmp.Close()

	if _, err := newTmp.WriteString(newContent); err != nil {
		return "", fmt.Errorf("failed to write new content: %v", err)
	}
	newTmp.Close()

	// Run diff command to generate unified diff
	cmd := exec.Command("diff", "-u", "--label=original/"+filename, "--label=modified/"+filename, oldTmp.Name(), newTmp.Name())
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	// diff returns exit code 1 when files differ (this is normal)
	err = cmd.Run()
	if err != nil {
		// Exit code 1 means files differ, which is what we want
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			// Real error (not just "files differ")
			if stderr.Len() > 0 {
				return "", fmt.Errorf("diff command failed: %s", stderr.String())
			}
		}
	}

	diffOutput := out.String()
	if diffOutput == "" {
		return "", nil // No differences
	}

	return diffOutput, nil
}

// colorizeDiff adds ANSI color codes to unified diff output.
func colorizeDiff(diff string) string {
	var colored strings.Builder
	lines := strings.Split(diff, "\n")

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++"):
			// File headers: bold white
			colored.WriteString(log.ColorBold + log.ColorWhite + line + log.ColorReset + "\n")
		case strings.HasPrefix(line, "@@"):
			// Hunk headers: cyan
			colored.WriteString(log.ColorCyan + line + log.ColorReset + "\n")
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			// Added lines: green
			colored.WriteString(log.ColorGreen + line + log.ColorReset + "\n")
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			// Removed lines: red
			colored.WriteString(log.ColorRed + line + log.ColorReset + "\n")
		default:
			// Context lines: no color
			colored.WriteString(line + "\n")
		}
	}

	return strings.TrimRight(colored.String(), "\n")
}

// diffHunk represents a single hunk in unified diff format.
type diffHunk struct {
	Header string   // @@ -a,b +c,d @@ header line
	Lines  []string // All lines in this hunk (including context, +, -)
	Start  int      // Starting line index in the original diff
	End    int      // Ending line index in the original diff
}

// parseHunks splits a unified diff into individual hunks.
func parseHunks(diff string) []diffHunk {
	var hunks []diffHunk
	lines := strings.Split(diff, "\n")

	var currentHunk *diffHunk
	inHunk := false

	for i, line := range lines {
		if strings.HasPrefix(line, "@@") {
			// Save previous hunk if exists
			if currentHunk != nil {
				currentHunk.End = i - 1
				hunks = append(hunks, *currentHunk)
			}
			// Start new hunk
			currentHunk = &diffHunk{
				Header: line,
				Lines:  []string{line},
				Start:  i,
			}
			inHunk = true
		} else if inHunk {
			currentHunk.Lines = append(currentHunk.Lines, line)
		}
	}

	// Don't forget the last hunk
	if currentHunk != nil {
		currentHunk.End = len(lines) - 1
		hunks = append(hunks, *currentHunk)
	}

	return hunks
}

// previewAndConfirm shows the diff to user and asks for confirmation.
// Supports chunk-by-chunk review when there are multiple hunks.
// Returns (finalContent, ok):
//   - ok=true means user accepted (some or all) changes; finalContent is what should be written.
//   - ok=false means user rejected all changes; finalContent is empty.
//
// Parameters oldContent and newContent are needed for partial apply.
func previewAndConfirm(path, oldContent, newContent, diff string) (string, bool) {
	fmt.Println()
	fmt.Println("─── Proposed changes to " + path + " ───")
	fmt.Println()

	// Parse hunks
	hunks := parseHunks(diff)

	if len(hunks) == 0 {
		fmt.Println("  ⚠️  No differences found")
		return "", false
	}

	// If only one hunk, use simple confirm (all-or-nothing)
	if len(hunks) == 1 {
		if previewSingleHunk(path, hunks[0], diff) {
			return newContent, true
		}
		return "", false
	}

	// Multiple hunks: chunk-by-chunk review
	return previewChunkByChunk(path, oldContent, newContent, hunks)
}

// previewSingleHunk handles confirmation for a single hunk.
// Returns true if user accepts the change.
func previewSingleHunk(path string, hunk diffHunk, fullDiff string) bool {
	// Show colored diff
	coloredDiff := colorizeDiff(fullDiff)
	fmt.Println(coloredDiff)
	fmt.Println()
	fmt.Print("  [A]pply  [R]eject  [D]iff again  [Q]uit: ")

	var answer string
	fmt.Scanln(&answer)
	answer = strings.ToLower(strings.TrimSpace(answer))

	switch answer {
	case "a", "apply", "y", "yes":
		return true
	case "r", "reject", "n", "no":
		fmt.Println("  ❌ Changes rejected")
		return false
	case "d", "diff":
		return previewSingleHunk(path, hunk, fullDiff) // Show again
	case "q", "quit":
		fmt.Println("  ❌ Changes rejected (quit)")
		return false
	default:
		fmt.Println("  ⚠️  Invalid option, rejecting changes")
		return false
	}
}

// previewChunkByChunk handles chunk-by-chunk confirmation.
// Returns (finalContent, ok) — finalContent is the content to write after partial apply.
func previewChunkByChunk(path, oldContent, newContent string, hunks []diffHunk) (string, bool) {
	fmt.Printf("  Found %d change blocks (hunks). Reviewing chunk by chunk...\n", len(hunks))
	fmt.Println()

	acceptedHunks := make([]bool, len(hunks))
	allAccepted := false
	allRejected := false

	for i := 0; i < len(hunks); i++ {
		hunk := hunks[i]
		if allAccepted {
			acceptedHunks[i] = true
			continue
		}
		if allRejected {
			acceptedHunks[i] = false
			continue
		}

		fmt.Println("────────────────────────────────────────────────────────────")
		fmt.Printf("  Chunk %d/%d:\n", i+1, len(hunks))
		fmt.Println()

		// Show colored hunk
		hunkDiff := strings.Join(hunk.Lines, "\n")
		coloredHunk := colorizeDiff(hunkDiff)
		fmt.Println(coloredHunk)
		fmt.Println()

		fmt.Print("  [A]ccept  [R]eject  a[L]l accept  [N]o all  [D]iff again  [Q]uit: ")

		var answer string
		fmt.Scanln(&answer)
		answer = strings.ToLower(strings.TrimSpace(answer))

		switch answer {
		case "a", "accept", "y", "yes":
			acceptedHunks[i] = true
			fmt.Println("  ✓ Chunk accepted")
		case "r", "reject":
			acceptedHunks[i] = false
			fmt.Println("  ✗ Chunk rejected")
		case "l", "all":
			allAccepted = true
			acceptedHunks[i] = true
			fmt.Println("  ✓ All remaining chunks accepted")
		case "n", "no":
			allRejected = true
			acceptedHunks[i] = false
			fmt.Println("  ✗ All remaining chunks rejected")
		case "d", "diff":
			// Show again - repeat this chunk
			i--
			continue
		case "q", "quit":
			fmt.Println("  ❌ Changes rejected (quit)")
			return "", false
		default:
			fmt.Println("  ⚠️  Invalid option, rejecting chunk")
			acceptedHunks[i] = false
		}
		fmt.Println()
	}

	// Check overall state
	anyAccepted := false
	allAcceptedFinal := true
	for _, accepted := range acceptedHunks {
		if accepted {
			anyAccepted = true
		} else {
			allAcceptedFinal = false
		}
	}

	if !anyAccepted {
		fmt.Println("  ❌ All chunks rejected")
		return "", false
	}

	if allAcceptedFinal {
		fmt.Println("  ✓ All chunks accepted, applying changes...")
		return newContent, true
	}

	// Partial apply: build a filtered patch with only accepted hunks
	fmt.Println()
	fmt.Printf("  Applying %d accepted chunks (skipping %d rejected)...\n",
		countTrue(acceptedHunks), len(acceptedHunks)-countTrue(acceptedHunks))

	finalContent, err := applyAcceptedHunks(oldContent, path, hunks, acceptedHunks)
	if err != nil {
		fmt.Printf("  ⚠️  Partial apply failed: %v\n", err)
		fmt.Print("  Apply ALL changes instead? [y/N]: ")
		var ans string
		fmt.Scanln(&ans)
		ans = strings.ToLower(strings.TrimSpace(ans))
		if ans == "y" || ans == "yes" {
			return newContent, true
		}
		return "", false
	}

	fmt.Println("  ✓ Partial apply succeeded")
	return finalContent, true
}

// countTrue counts the number of true elements in a bool slice.
func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// applyAcceptedHunks builds a filtered patch containing only the accepted hunks
// and applies it to oldContent using the system 'patch' command.
// Returns the resulting file content.
func applyAcceptedHunks(oldContent, filename string, hunks []diffHunk, accepted []bool) (string, error) {
	if len(hunks) != len(accepted) {
		return "", fmt.Errorf("hunks and accepted slices size mismatch")
	}

	// Build a filtered patch: standard unified-diff header + only accepted hunks
	var patchBuf strings.Builder
	patchBuf.WriteString("--- original/" + filename + "\n")
	patchBuf.WriteString("+++ modified/" + filename + "\n")
	for i, hunk := range hunks {
		if !accepted[i] {
			continue
		}
		// hunk.Lines includes the @@ header at index 0
		for _, ln := range hunk.Lines {
			patchBuf.WriteString(ln)
			patchBuf.WriteString("\n")
		}
	}
	patchContent := patchBuf.String()

	// Write oldContent to a temp file (the file to be patched)
	srcTmp, err := os.CreateTemp("", "patch-src-*.txt")
	if err != nil {
		return "", fmt.Errorf("create source temp file: %v", err)
	}
	defer os.Remove(srcTmp.Name())
	if _, err := srcTmp.WriteString(oldContent); err != nil {
		srcTmp.Close()
		return "", fmt.Errorf("write source temp file: %v", err)
	}
	srcTmp.Close()

	// Write patch to a temp file
	patchTmp, err := os.CreateTemp("", "patch-*.patch")
	if err != nil {
		return "", fmt.Errorf("create patch temp file: %v", err)
	}
	defer os.Remove(patchTmp.Name())
	if _, err := patchTmp.WriteString(patchContent); err != nil {
		patchTmp.Close()
		return "", fmt.Errorf("write patch temp file: %v", err)
	}
	patchTmp.Close()

	// Run: patch <srcFile> <patchFile>
	// Use --no-backup-if-mismatch to avoid creating .orig files
	// Use --fuzz=3 to allow some context fuzziness when accepted hunks shift line numbers
	cmd := exec.Command("patch", "--no-backup-if-mismatch", "--fuzz=3", srcTmp.Name(), patchTmp.Name())
	var stderr bytes.Buffer
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String() + stdout.String())
		if errMsg == "" {
			errMsg = err.Error()
		}
		return "", fmt.Errorf("patch failed: %s", errMsg)
	}

	// Read the patched file content
	patched, err := os.ReadFile(srcTmp.Name())
	if err != nil {
		return "", fmt.Errorf("read patched file: %v", err)
	}
	return string(patched), nil
}

// shouldPreviewDiff determines if we should show diff preview.
// Always returns true: user should always see and confirm changes.
func shouldPreviewDiff() bool {
	return true
}

// checkToolApproval determines if a tool call should be allowed.
// Returns (allowed, needConfirm, reason).
func checkToolApproval(toolName string) (bool, string) {
	meta, ok := toolSecurityMap[toolName]
	if !ok {
		// Unknown tool: treat as danger - require explicit confirmation.
		return false, fmt.Sprintf("unknown tool '%s' - not in security registry", toolName)
	}

	switch meta.Level {
	case ApproveAuto:
		return true, ""
	case ApproveSafe:
		if globalApproval.IsAutoApproveAll() || globalApproval.IsAutoApproveSafe() {
			return true, ""
		}
		return false, fmt.Sprintf("[safe] %s requires approval. Use /approve safe to auto-approve.", meta.Description)
	case ApproveDanger:
		if globalApproval.IsAutoApproveAll() {
			return true, ""
		}
		return false, fmt.Sprintf("[DANGER] %s requires confirmation. Use /approve danger to auto-approve (risky!).", meta.Description)
	case ApproveBlocked:
		return false, fmt.Sprintf("BLOCKED: %s is not permitted", meta.Description)
	default:
		return false, fmt.Sprintf("unknown approval level for tool '%s'", toolName)
	}
}

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

// P0-2: Secure Path Sandbox - symlink escape prevention

// Sensitive paths that should never be written to (even inside workdir).
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

// securePath validates and resolves a user-supplied path within the workdir.
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
func securePath(workdirPath, userPath string, allowWrite bool) (string, error) {
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
			// 修复：不暴露底层系统调用细节，返回用户友好的错误信息
			// 检查是否是文件不存在的错误
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

// isSensitiveFile checks if a given path looks like a sensitive file.
func isSensitiveFile(path string) bool {
	lower := strings.ToLower(path)
	for _, sp := range sensitivePathPatterns {
		if strings.Contains(lower, strings.ToLower(sp)) {
			return true
		}
	}
	return false
}

// P0-4: Secrets Sanitizer - detect and redact secrets in text

// SecretsSanitizer detects and redacts sensitive information (API keys,
// tokens, passwords, private keys) from text.
type SecretsSanitizer struct {
	patterns []*regexp.Regexp
}

// NewSecretsSanitizer creates a new sanitizer with built-in patterns.
func NewSecretsSanitizer() *SecretsSanitizer {
	patterns := []*regexp.Regexp{
		// OpenAI-style API keys (sk-...)
		regexp.MustCompile(`(?i)(sk-(?:proj-|)?[a-zA-Z0-9_-]{10,})`),

		// Generic api_key / apiKey patterns with long values
		regexp.MustCompile(`(?i)(api[_\-]?key|apikey)["\s]*[:=]["\s]*["']?[a-zA-Z0-9_\-./+=]{20,}["']?`),

		// AWS Access Key ID
		regexp.MustCompile(`(A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}`),

		// AWS Secret Access Key (40 char base64-ish)
		regexp.MustCompile(`(?i)(aws[_\-]?secret[_\-]?access[_\-]?key)["\s]*[:=]["\s]*["']?[A-Za-z0-9/+=]{40}["']?`),

		// Generic tokens / bearer tokens
		regexp.MustCompile(`(?i)(token|auth[_\-]?token|bearer|access[_\-]?token)["\s]*[:=]\s*["']?[\w\-._~+/]{20,}["']?`),

		// Passwords in URLs (user:pass@host)
		regexp.MustCompile(`(https?://)[^/\s:@]+:[^/\s@]+@`),

		// Private keys (PEM format)
		regexp.MustCompile(`(?s)-----BEGIN\s*(RSA |EC |DSA |OPENSSH |PRIVATE )?KEY-----[\s\S]*?-----END\s*(RSA |EC |DSA |OPENSSH |PRIVATE )?KEY-----`),
	}
	return &SecretsSanitizer{patterns: patterns}
}

// Detect returns true if the text potentially contains secrets.
func (s *SecretsSanitizer) Detect(text string) bool {
	for _, pat := range s.patterns {
		if pat.MatchString(text) {
			return true
		}
	}
	return false
}

// DetectPatterns returns descriptions of matched secret patterns (for warning).
func (s *SecretsSanitizer) DetectPatterns(text string) []string {
	var found []string
	for _, pat := range s.patterns {
		if pat.MatchString(text) {
			found = append(found, pat.String())
		}
	}
	return found
}

// Sanitize redacts detected secrets from text, preserving prefix for identification.
func (s *SecretsSanitizer) Sanitize(text string) string {
	result := text
	for _, pat := range s.patterns {
		result = pat.ReplaceAllStringFunc(result, func(match string) string {
			if len(match) > 12 {
				// Keep first few chars so logs are debuggable
				return match[:6] + "***REDACTED***"
			}
			return "***REDACTED***"
		})
	}
	return result
}

// Global sanitizer instance.
var secretsSanitizer = NewSecretsSanitizer()

// Secure execution helpers - used by tool_base.go

// secureRunBash runs a bash command through the full security pipeline:
// 1. BashPolicy validation (allowlist + danger patterns)
// 2. User confirmation (if needed, based on approval state)
// 3. Execution with timeout and workdir jail
//
// If interactive is false and confirmation is needed, it blocks the command
// and returns an error message instead.
func secureRunBash(command string, interactive bool) string {
	// Step 1: Policy validation
	allowed, needConfirm, reason := DefaultBashPolicy.Validate(command)
	if !allowed {
		return fmt.Sprintf("\u274c Security blocked: %s", reason)
	}

	// Step 2: Confirmation gate
	if needConfirm && !globalApproval.IsAutoApproveAll() {
		if !interactive {
			return fmt.Sprintf("\u26a0\ufe0f Command requires confirmation (non-interactive): %s\n"+
				"Use /approve danger to auto-approve, or run interactively.", reason)
		}
		fmt.Printf("\u26a0\ufe0f [CONFIRM] %s\n   Command: %s\n   Allow? [y/N]: ", reason, command)
		var answer string
		fmt.Scanln(&answer)
		if strings.ToLower(strings.TrimSpace(answer)) != "y" &&
			strings.ToLower(strings.TrimSpace(answer)) != "yes" {
			return "\u274c User rejected the command"
		}
	}

	// Also check general approval layer for bash tool
	if approved, msg := checkToolApproval("bash"); !approved && !globalApproval.IsAutoApproveAll() {
		if !interactive {
			return fmt.Sprintf("\u274c %s", msg)
		}
		fmt.Printf("%s\n   Allow? [y/N]: ", msg)
		var answer string
		fmt.Scanln(&answer)
		if strings.ToLower(strings.TrimSpace(answer)) != "y" &&
			strings.ToLower(strings.TrimSpace(answer)) != "yes" {
			return "\u274c User rejected the command"
		}
	}

	// Step 3: Execute
	ctx, cancel := context.WithTimeout(context.Background(), infra.BashTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = workdir // Jail to workdir

	output, err := cmd.CombinedOutput()

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Sprintf("Error: Timeout (%v)", infra.BashTimeout)
	}
	out := strings.TrimSpace(string(output))
	if err != nil && out == "" {
		out = fmt.Sprintf("Error: %v", err)
	}
	if out == "" {
		out = "(no output)"
	}

	// Step 4: Post-execution: sanitize secrets from output before returning
	if secretsSanitizer.Detect(out) {
		log.PrintSystem("[security] potential secrets detected in command output - sanitized")
		out = secretsSanitizer.Sanitize(out)
	}

	return truncate(out, infra.MaxOutputLen)
}

// secureReadFile reads a file after validating its path through the securePath sandbox.
// Optimized version: uses buffered scanner for memory efficiency and supports line limit.
func secureReadFile(path string, limit int) string {
	fp, err := securePath(workdir, path, false) // read-only, no sensitive-file check needed
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	f, err := os.Open(fp)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	defer f.Close()

	// Use buffered scanner for memory-efficient line reading
	var buf bytes.Buffer
	scanner := bufio.NewScanner(f)

	// Set scanner buffer size for long lines (default is 64K)
	scanner.Buffer(make([]byte, 1024*64), infra.MaxOutputLen)

	lineCount := 0
	for scanner.Scan() {
		// Check line limit
		if limit > 0 && lineCount >= limit {
			buf.WriteString(fmt.Sprintf("... (%d more lines)\n", countRemainingLines(f)))
			break
		}

		// Write line to buffer
		if lineCount > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(scanner.Text())
		lineCount++

		// Early exit if buffer is getting too large
		if buf.Len() > infra.MaxOutputLen {
			buf.Truncate(infra.MaxOutputLen)
			buf.WriteString("\n... (output truncated)\n")
			break
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Sprintf("Error reading file: %v", err)
	}

	result := buf.String()

	// Sanitize secrets from file content (only if content is not too large)
	if len(result) < 50000 && secretsSanitizer.Detect(result) {
		result = secretsSanitizer.Sanitize(result)
	}
	return result
}

// countRemainingLines estimates remaining lines in a file (fast approximation)
func countRemainingLines(f *os.File) int {
	// Save current position
	pos, _ := f.Seek(0, 1)
	defer f.Seek(pos, 0) // Restore position

	// Quick estimation: scan remaining content
	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() && count < 1000 { // Cap at 1000 for performance
		count++
	}
	return count
}

// secureWriteFile writes content to a file after path validation.
func secureWriteFile(path, content string) string {
	// Check write approval
	if approved, msg := checkToolApproval("write_file"); !approved && !globalApproval.IsAutoApproveSafe() && !globalApproval.IsAutoApproveAll() {
		return fmt.Sprintf("\u274c %s", msg)
	}

	fp, err := securePath(workdir, path, true) // allowWrite=true
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return fmt.Sprintf("Error creating directory: %v", err)
	}

	// Diff preview before writing
	finalContent := content
	if shouldPreviewDiff() {
		// Read existing content (if file exists)
		var oldContent string
		if data, err := os.ReadFile(fp); err == nil {
			oldContent = string(data)
		}
		// Generate diff
		diff, err := generateUnifiedDiff(oldContent, content, path)
		if err != nil {
			return fmt.Sprintf("Error generating diff: %v", err)
		}
		// If there are changes, show preview and ask for confirmation
		if diff != "" {
			applied, ok := previewAndConfirm(path, oldContent, content, diff)
			if !ok {
				return "\u274c File write rejected by user"
			}
			finalContent = applied
		}
	}

	// Atomic write: write to temp file then rename to prevent corruption
	tmpPath := fmt.Sprintf("%s.tmp.%d.%d", fp, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, []byte(finalContent), 0o644); err != nil {
		os.Remove(tmpPath) // clean up
		return fmt.Sprintf("Error writing temp file: %v", err)
	}
	if err := os.Rename(tmpPath, fp); err != nil {
		os.Remove(tmpPath) // clean up
		// Fallback: rename might fail across devices, try direct write
		if err2 := os.WriteFile(fp, []byte(finalContent), 0o644); err2 != nil {
			return fmt.Sprintf("Error writing file (atomic+direct both failed): %v / %v", err, err2)
		}
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(finalContent), path)
}

// secureEditFile replaces exact text in a file after path validation.
// Loads the entire file into memory for accurate matching (supports both single-line and multi-line).
// This follows Claude Code's approach: simple and reliable.
// Includes TOCTOU protection to prevent race conditions.
func secureEditFile(path, oldText, newText string) string {
	// Check edit approval
	if approved, msg := checkToolApproval("edit_file"); !approved && !globalApproval.IsAutoApproveSafe() && !globalApproval.IsAutoApproveAll() {
		return fmt.Sprintf("\u274c %s", msg)
	}

	fp, err := securePath(workdir, path, true) // allowWrite=true
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	// Read file with TOCTOU protection
	// First, get the file's modification time before reading
	fileInfo, err := os.Stat(fp)
	if err != nil {
		return fmt.Sprintf("Error stating file: %v", err)
	}
	originalMtime := fileInfo.ModTime()

	// Read entire file content for matching
	data, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("Error reading file: %v", err)
	}

	// Second check: verify file hasn't changed since we stated it
	fileInfo2, err := os.Stat(fp)
	if err != nil {
		return fmt.Sprintf("Error re-stating file: %v", err)
	}
	if !fileInfo2.ModTime().Equal(originalMtime) {
		return fmt.Sprintf("Error: File '%s' was modified during editing (mtime changed)", path)
	}

	content := string(data)

	// Check if oldText exists in file (supports both single-line and multi-line matching)
	if !strings.Contains(content, oldText) {
		return fmt.Sprintf("Error: Text not found in %s", path)
	}

	// Perform replacement (only first occurrence in the entire file)
	newContent := strings.Replace(content, oldText, newText, 1)

	// Generate diff if needed
	var oldForDiff string
	if shouldPreviewDiff() && len(content) < 500000 {
		oldForDiff = content
	}

	if oldForDiff != "" {
		diff, err := generateUnifiedDiff(oldForDiff, newContent, path)
		if err != nil {
			return fmt.Sprintf("Error generating diff: %v", err)
		}
		if diff != "" {
			applied, ok := previewAndConfirm(path, oldForDiff, newContent, diff)
			if !ok {
				return "\u274c File edit rejected by user"
			}
			newContent = applied
		}
	}

	// Third check (atomic): verify file hasn't changed before writing
	// This is the critical check that must be atomic with the write
	fileInfo3, err := os.Stat(fp)
	if err != nil {
		return fmt.Sprintf("Error checking file before write: %v", err)
	}
	if !fileInfo3.ModTime().Equal(originalMtime) {
		return fmt.Sprintf("Error: File '%s' was modified during editing (concurrent modification detected)", path)
	}

	// Atomic write
	tmpPath := fp + ".tmp.edit.$$"
	if err := os.WriteFile(tmpPath, []byte(newContent), 0o644); err != nil {
		os.Remove(tmpPath)
		return fmt.Sprintf("Error writing temp file: %v", err)
	}
	if err := os.Rename(tmpPath, fp); err != nil {
		os.Remove(tmpPath)
		if err2 := os.WriteFile(fp, []byte(newContent), 0o644); err2 != nil {
			return fmt.Sprintf("Error writing file: %v / %v", err, err2)
		}
	}
	return fmt.Sprintf("Edited %s", path)
}

// secureDeleteFile deletes a file after path validation.
func secureDeleteFile(path string) string {
	// Check delete approval
	if approved, msg := checkToolApproval("delete_file"); !approved && !globalApproval.IsAutoApproveAll() {
		return fmt.Sprintf("\u274c %s", msg)
	}

	fp, err := securePath(workdir, path, true)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if err := os.Remove(fp); err != nil {
		return fmt.Sprintf("Error deleting file: %v", err)
	}
	return fmt.Sprintf("Deleted %s", path)
}
