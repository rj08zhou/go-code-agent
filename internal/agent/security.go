package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/security"
	"go-code-agent/utils"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Security layer glue for go-code-agent.
//
// The reusable, agent-agnostic security primitives (approval
// state machine, bash policy, path sandbox, secrets sanitizer, diff
// preview) live in internal/security - see that package for their
// implementation and doc comments. This file only keeps what's
// genuinely tied to this package's concerns:
//
//   - ToolSecurityMeta / ToolSecurityMap: the concrete tool registry
//     (populated by registerToolSpec, see tool_base.go/tool_registry.go).
//   - checkToolApproval: resolves a tool name to a security.ApprovalLevel
//     via that registry (with an MCP-prefix fallback), then delegates
//     the actual allow/deny decision to security.GlobalApproval.Decide.
//   - secure*: workdir-aware wrappers around internal/security's pure
//     functions (they need this package's App.Workdir, logging.Print*,
//     and checkToolApproval).

type ToolSecurityMeta struct {
	Name        string
	Level       security.ApprovalLevel
	Description string
}

// workdirKey is the private context key under which a per-call working
// directory override is carried. See WithWorkdir / workdirFromCtx.
type workdirKey struct{}

// WithWorkdir returns a context that pins tool file/bash operations to
// dir instead of the process-wide App.Workdir. It is the mechanism that
// lets an isolated agent (e.g. a teammate running in its own git
// worktree) confine its reads/writes/commands to a private directory
// while the main agent keeps using App.Workdir untouched. An empty dir
// is a no-op (returns ctx unchanged) so callers can pass through
// unconditionally.
func WithWorkdir(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, workdirKey{}, dir)
}

// workdirFromCtx resolves the working directory for a tool call: the
// ctx override if one was set via WithWorkdir, else the process-wide
// App.Workdir. This fallback is what keeps the main agent's behavior
// identical to before this indirection existed - it never sets the
// override, so it always lands on App.Workdir.
func workdirFromCtx(ctx context.Context) string {
	if ctx != nil {
		if d, ok := ctx.Value(workdirKey{}).(string); ok && d != "" {
			return d
		}
	}
	return App.Workdir
}

// ToolSecurityMap holds the approval Level for every registered tool.
//
// It starts empty and is populated exclusively by registerToolSpec
// (see tool_base.go / tool_registry.go's InitTools) — never by a
// static literal here. That used to be a hand-maintained map living in
// this file, disconnected from the Def+Handler registration in
// tool_registry.go; it drifted in both directions in practice: some
// registered tools had no entry here (silently unusable — see
// checkToolApproval's "unknown tool" branch below) and some entries
// here referred to tool names this agent never actually registers.
// Routing every tool through registerToolSpec makes that class of
// drift impossible: a tool cannot exist in ToolDefs without also
// landing here with an explicit Level.
var ToolSecurityMap = map[string]ToolSecurityMeta{}

// checkToolApproval determines if a tool call should be allowed.
// Returns (allowed, reason).
//
// Resolution order:
//  1. Exact match in ToolSecurityMap (the tool registry).
//  2. MCP-prefixed tools ("mcp__<server>__<tool>") with no explicit
//     entry fall back to security.MCPDefaultLevel (see that constant's
//     doc comment for why).
//  3. Anything else is an unknown tool: permanently denied, regardless
//     of /approve settings (fail-safe default).
//
// User permission rules (permissions.json) are then consulted with the
// call's arguments, EXCEPT for bash: bash command-pattern rules are
// enforced inside BashPolicy.Validate instead (which also guarantees
// the DangerPatterns blacklist is checked first and can't be bypassed),
// so matching them here too would double-prompt. For every other tool a
// matching rule overrides the Level decision:
//
//	deny  -> blocked
//	allow -> approved (skips the Level gate, but note it can never
//	         bypass hard checks like the path sandbox / bash blacklist,
//	         which run independently in the secure* wrappers)
//	ask   -> forced through danger-level approval so it is never
//	         silently auto-approved
//
// With no matching rule (the default when there is no permissions.json)
// the resolved Level is delegated to security.GlobalApproval.Decide, so
// behavior is identical to before.
func checkToolApproval(toolName, argsJSON string) (bool, string) {
	meta, ok := ToolSecurityMap[toolName]
	if !ok {
		if strings.HasPrefix(toolName, security.MCPToolPrefix) {
			meta = ToolSecurityMeta{Name: toolName, Level: security.MCPDefaultLevel, Description: fmt.Sprintf("MCP tool '%s'", toolName)}
		} else {
			return false, fmt.Sprintf("unknown tool '%s' - not in security registry", toolName)
		}
	}

	if toolName != "bash" {
		target := permissionTarget(toolName, argsJSON)
		switch act, rule := security.GlobalPermissions.Match(toolName, target); act {
		case security.PermDeny:
			return false, fmt.Sprintf("blocked by permission rule (deny): tool %q matches pattern %q", toolName, rule.Pattern)
		case security.PermAllow:
			return true, ""
		case security.PermAsk:
			return security.GlobalApproval.Decide(security.ApproveDanger,
				fmt.Sprintf("%s (ask rule: %q)", meta.Description, rule.Pattern))
		}
	}

	return security.GlobalApproval.Decide(meta.Level, meta.Description)
}

// permissionTarget extracts the argument a permission rule matches
// against for a given tool: the command for bash, the path for file
// tools, else the raw arguments JSON. It tolerates being handed either
// a JSON arguments blob (the dispatch path) or, when JSON parsing
// fails, the raw target string directly (some internal callers) - in
// both cases it yields the string a rule's pattern should test.
func permissionTarget(toolName, argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	var a struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	if json.Unmarshal([]byte(argsJSON), &a) == nil {
		switch toolName {
		case "bash":
			if a.Command != "" {
				return a.Command
			}
		case "read_file", "write_file", "edit_file", "delete_file":
			if a.Path != "" {
				return a.Path
			}
		}
	}
	return argsJSON
}

// hitlGateResult is nil when a tool call may proceed (no review needed,
// or the human approved it as-is). Otherwise it carries the reject/modify
// verdict for the caller to format and act on.
type hitlGateResult struct {
	Rejected bool
	Reason   string // populated when Rejected
	Feedback string // populated when modified (not rejected)
}

// runHITLGate consults hitlaudit.HitlManager for tc and, if human review
// is required, requests approval and returns the verdict. Shared by
// loop.go's dispatchTool and subagent.go's dispatchSubagentTool, which
// only differ in how they format/log the returned verdict.
func runHITLGate(tc llm.ToolCall) *hitlGateResult {
	needReview, risk, hitlReason := hitlaudit.HitlManager.NeedsReview(tc.Name, tc.Arguments)
	if !needReview {
		return nil
	}
	sid := App.ActiveSessionID()
	resp := hitlaudit.HitlManager.RequestApproval(hitlaudit.HITLRequest{
		ToolName:  tc.Name,
		Arguments: tc.Arguments,
		RiskLevel: risk,
		Reason:    hitlReason,
		SessionID: sid,
	})
	switch resp.Decision {
	case hitlaudit.HITLReject:
		return &hitlGateResult{Rejected: true, Reason: hitlReason}
	case hitlaudit.HITLModify:
		return &hitlGateResult{Feedback: resp.Feedback}
	default: // hitlaudit.HITLApprove
		return nil
	}
}

// Secure execution helpers - used by tool_base.go

// secureRunBash runs a bash command through the full security pipeline:
// 1. BashPolicy validation (allowlist + danger patterns)
// 2. User confirmation (if needed, based on approval state)
// 3. Execution with timeout and workdir jail
//
// If interactive is false and confirmation is needed, it blocks the command
// and returns an error message instead.
//
// The working directory (both the SecurePath jail root and the command's
// cwd) comes from workdirFromCtx(ctx): the main agent gets App.Workdir,
// an isolated teammate gets its private worktree.
func secureRunBash(ctx context.Context, command string, interactive bool) string {
	// Step 1: Policy validation
	allowed, needConfirm, reason := security.DefaultBashPolicy.Validate(command)
	if !allowed {
		return fmt.Sprintf("\u274c Security blocked: %s", reason)
	}

	// Step 2: Confirmation gate
	if needConfirm && !security.GlobalApproval.IsAutoApproveAll() {
		if !interactive {
			return fmt.Sprintf("\u26a0\ufe0f Command requires confirmation (non-interactive): %s\n"+
				"Use /approve danger to auto-approve, or run interactively.", reason)
		}
		fmt.Printf("\u26a0\ufe0f [CONFIRM] %s\n   Command: %s\n   Allow? [y/N]: ", reason, command)
		line, _ := security.ReadLine()
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			return "\u274c User rejected the command"
		}
	}

	// Also check general approval layer for bash tool
	if approved, msg := checkToolApproval("bash", command); !approved && !security.GlobalApproval.IsAutoApproveAll() {
		if !interactive {
			return fmt.Sprintf("\u274c %s", msg)
		}
		fmt.Printf("%s\n   Allow? [y/N]: ", msg)
		line, _ := security.ReadLine()
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			return "\u274c User rejected the command"
		}
	}

	// Step 3: Execute
	execCtx, cancel := context.WithTimeout(context.Background(), infra.BashTimeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "sh", "-c", command)
	cmd.Dir = workdirFromCtx(ctx) // Jail to the (possibly overridden) workdir
	utils.SetNewProcessGroup(cmd)

	output, err := cmd.CombinedOutput()

	if execCtx.Err() == context.DeadlineExceeded {
		// Actionable message, not a bare "Timeout": otherwise the model
		// tends to blindly retry the same blocking command under bash or
		// wrap it in a `timeout` shell command (which isn't installed on
		// macOS -> "sh: timeout: command not found"). Point it at the
		// right tool instead.
		return fmt.Sprintf("Error: command timed out after %v and was killed (along with its child processes). "+
			"If this is a long-running or blocking process (dev server, watch-mode build, tail -f, etc.), "+
			"re-run it with the background_run tool instead of bash - do not retry it under bash and do not wrap it in a 'timeout' command.", infra.BashTimeout)
	}
	out := strings.TrimSpace(string(output))
	if err != nil && out == "" {
		out = fmt.Sprintf("Error: %v", err)
	}
	if out == "" {
		out = "(no output)"
	}

	// Step 4: Post-execution: sanitize secrets from output before returning
	if security.GlobalSecretsSanitizer.Detect(out) {
		logging.PrintSystem("[security] potential secrets detected in command output - sanitized")
		out = security.GlobalSecretsSanitizer.Sanitize(out)
	}

	return utils.Truncate(out, infra.MaxOutputLen)
}

// secureReadFile reads a file after validating its path through the securePath sandbox.
// Optimized version: uses buffered scanner for memory efficiency and supports line limit.
func secureReadFile(ctx context.Context, path string, limit int) string {
	fp, err := security.SecurePath(workdirFromCtx(ctx), path, false) // read-only, no sensitive-file check needed
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
	if len(result) < 50000 && security.GlobalSecretsSanitizer.Detect(result) {
		result = security.GlobalSecretsSanitizer.Sanitize(result)
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
func secureWriteFile(ctx context.Context, path, content string) string {
	// Check write approval
	if approved, msg := checkToolApproval("write_file", path); !approved && !security.GlobalApproval.IsAutoApproveSafe() && !security.GlobalApproval.IsAutoApproveAll() {
		return fmt.Sprintf("\u274c %s", msg)
	}

	fp, err := security.SecurePath(workdirFromCtx(ctx), path, true) // allowWrite=true
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return fmt.Sprintf("Error creating directory: %v", err)
	}

	// Diff preview before writing
	finalContent := content
	if security.ShouldPreviewDiff() {
		// Read existing content (if file exists)
		var oldContent string
		if data, err := os.ReadFile(fp); err == nil {
			oldContent = string(data)
		}
		// Generate diff
		diff, err := security.GenerateUnifiedDiff(oldContent, content, path)
		if err != nil {
			return fmt.Sprintf("Error generating diff: %v", err)
		}
		// If there are changes, show preview and ask for confirmation
		if diff != "" {
			applied, ok := security.PreviewAndConfirm(path, oldContent, content, diff)
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
func secureEditFile(ctx context.Context, path, oldText, newText string) string {
	// Check edit approval
	if approved, msg := checkToolApproval("edit_file", path); !approved && !security.GlobalApproval.IsAutoApproveSafe() && !security.GlobalApproval.IsAutoApproveAll() {
		return fmt.Sprintf("\u274c %s", msg)
	}

	fp, err := security.SecurePath(workdirFromCtx(ctx), path, true) // allowWrite=true
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

	// Match old_text against the file in decreasing order of strictness:
	//  1. Exact substring (fast path, unchanged behavior).
	//  2. Whitespace-tolerant (see findWhitespaceTolerantMatch) - only
	//     accepted when it uniquely identifies one location; ambiguous
	//     matches are treated as failure, not guessed.
	//  3. If both fail, attach a diagnostic hint (closestMatchHint)
	//     pointing at the most similar real content instead of a bare
	//     "not found" - lets the model self-correct old_text in one
	//     more turn instead of blindly re-reading the whole file.
	matchedOld := oldText
	whitespaceTolerant := false
	if !strings.Contains(content, oldText) {
		if m, count := findWhitespaceTolerantMatch(content, oldText); count == 1 {
			matchedOld = m
			whitespaceTolerant = true
		} else if count > 1 {
			return fmt.Sprintf("Error: Text not found in %s (exact match), and %d locations match when ignoring whitespace - too ambiguous to pick one automatically. Add more surrounding context to old_text to make it unique.%s",
				path, count, closestMatchHint(content, oldText))
		} else {
			return fmt.Sprintf("Error: Text not found in %s.%s", path, closestMatchHint(content, oldText))
		}
	}

	// Perform replacement (only first occurrence in the entire file)
	newContent := strings.Replace(content, matchedOld, newText, 1)

	// Generate diff if needed
	var oldForDiff string
	if security.ShouldPreviewDiff() && len(content) < 500000 {
		oldForDiff = content
	}

	if oldForDiff != "" {
		diff, err := security.GenerateUnifiedDiff(oldForDiff, newContent, path)
		if err != nil {
			return fmt.Sprintf("Error generating diff: %v", err)
		}
		if diff != "" {
			applied, ok := security.PreviewAndConfirm(path, oldForDiff, newContent, diff)
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
	if whitespaceTolerant {
		return fmt.Sprintf("Edited %s (matched ignoring leading/trailing whitespace differences per line - the file's original indentation was preserved for surrounding text)", path)
	}
	return fmt.Sprintf("Edited %s", path)
}

// findWhitespaceTolerantMatch looks for a substring of content whose
// lines equal old_text's lines after per-line TrimSpace, tolerating
// differences in indentation / trailing spaces (a common mismatch
// when a model recalls a file's content slightly off). It returns the
// exact substring of `content` that matched (so replacement preserves
// the file's real whitespace) and how many distinct start positions
// matched - callers should only apply the fix when count == 1;
// ambiguous or zero matches are the caller's responsibility.
//
// Deliberately a plain O(lines*lines) scan over line boundaries, not a
// generic diff/similarity library: old_text is typically a handful of
// lines, and this only runs on the (already rare) exact-match-failure
// path, so simplicity and zero new dependencies win over asymptotic
// elegance.
func findWhitespaceTolerantMatch(content, oldText string) (match string, count int) {
	oldLines := strings.Split(oldText, "\n")
	if len(oldLines) == 0 {
		return "", 0
	}
	trimmedOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		trimmedOld[i] = strings.TrimSpace(l)
	}

	// Split content into lines but keep enough information to slice
	// back into the original string: track each line's [start,end)
	// byte offset (end excludes the trailing '\n', if any).
	type lineSpan struct{ start, end int }
	var spans []lineSpan
	lineStart := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' {
			spans = append(spans, lineSpan{lineStart, i})
			lineStart = i + 1
		}
	}
	spans = append(spans, lineSpan{lineStart, len(content)})

	n := len(trimmedOld)
	var lastMatch string
	for start := 0; start+n <= len(spans); start++ {
		ok := true
		for j := range n {
			line := content[spans[start+j].start:spans[start+j].end]
			if strings.TrimSpace(line) != trimmedOld[j] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		count++
		lastMatch = content[spans[start].start:spans[start+n-1].end]
	}
	if count == 1 {
		return lastMatch, 1
	}
	return "", count
}

// closestMatchHint returns a short, actionable diagnostic appended to
// edit_file's error message when neither exact nor whitespace-tolerant
// matching found old_text. It locates the file line most similar to
// old_text's first non-blank line (via a cheap word-overlap score, not
// a full diff library) and shows a few lines of real file content
// around it with line numbers - enough for the model to correct
// old_text in its next call instead of re-reading the entire file.
// Returns "" if the file has no lines or old_text is empty.
func closestMatchHint(content, oldText string) string {
	firstLine := ""
	for l := range strings.SplitSeq(oldText, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			firstLine = t
			break
		}
	}
	if firstLine == "" {
		return ""
	}
	targetWords := newWordSet(firstLine)
	if len(targetWords) == 0 {
		return ""
	}

	lines := strings.Split(content, "\n")
	bestIdx, bestScore := -1, 0
	for i, l := range lines {
		score := len(targetWords.intersect(newWordSet(l)))
		if score > bestScore {
			bestScore, bestIdx = score, i
		}
	}
	if bestIdx < 0 || bestScore == 0 {
		return ""
	}

	const ctx = 2
	from, to := bestIdx-ctx, bestIdx+ctx
	if from < 0 {
		from = 0
	}
	if to >= len(lines) {
		to = len(lines) - 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, " Closest match found around line %d:\n", bestIdx+1)
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "  %6d: %s\n", i+1, lines[i])
	}
	return b.String()
}

// wordSet is a tiny helper for closestMatchHint's similarity heuristic.
type wordSet map[string]bool

func newWordSet(s string) wordSet {
	ws := make(wordSet)
	for f := range strings.FieldsSeq(s) {
		ws[f] = true
	}
	return ws
}

func (ws wordSet) intersect(other wordSet) wordSet {
	out := make(wordSet)
	for w := range ws {
		if other[w] {
			out[w] = true
		}
	}
	return out
}

// secureDeleteFile deletes a file after path validation.
func secureDeleteFile(ctx context.Context, path string) string {
	// Check delete approval
	if approved, msg := checkToolApproval("delete_file", path); !approved && !security.GlobalApproval.IsAutoApproveAll() {
		return fmt.Sprintf("\u274c %s", msg)
	}

	fp, err := security.SecurePath(workdirFromCtx(ctx), path, true)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	if err := os.Remove(fp); err != nil {
		return fmt.Sprintf("Error deleting file: %v", err)
	}
	return fmt.Sprintf("Deleted %s", path)
}
