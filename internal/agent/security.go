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

// Security layer glue: tool security registry, checkToolApproval, and
// workdir-aware wrappers around internal/security primitives.

type ToolSecurityMeta struct {
	Name        string
	Level       security.ApprovalLevel
	Description string
}

// workdirKey is the private context key under which a per-call working
// directory override is carried. See WithWorkdir / workdirFromCtx.
type workdirKey struct{}

// WithWorkdir pins tool file/bash operations to dir instead of App.Workdir.
// No-op if dir is empty.
func WithWorkdir(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, workdirKey{}, dir)
}

// workdirFromCtx returns the ctx override if set, else App.Workdir.
func workdirFromCtx(ctx context.Context) string {
	if ctx != nil {
		if d, ok := ctx.Value(workdirKey{}).(string); ok && d != "" {
			return d
		}
	}
	return App.Workdir
}

// ToolSecurityMap holds the approval Level for every registered tool.
// Populated by registerToolSpec (see tool_base.go / tool_registry.go).
var ToolSecurityMap = map[string]ToolSecurityMeta{}

// checkToolApproval determines if a tool call should be allowed.
// Resolution: ToolSecurityMap exact match → MCP default → deny (unknown).
// Then permissions.json rules (deny/allow/ask) override the Level decision,
// except for bash (enforced in BashPolicy.Validate to avoid double-prompting).
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
		fmt.Printf("\u26a0\ufe0f [CONFIRM] %s\n   Command: %s\n", reason, command)
		line, _ := security.ReadLine("   Allow? [y/N]: ")
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
		fmt.Println(msg)
		line, _ := security.ReadLine("   Allow? [y/N]: ")
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
// Uses a buffered scanner for memory efficiency. When offset and limit are both 0,
// ReadFileDefaultLimit is applied to avoid injecting an entire large file at once.
func secureReadFile(ctx context.Context, path string, offset, limit int) string {
	if offset == 0 && limit == 0 {
		limit = infra.ReadFileDefaultLimit
	}
	fp, err := security.SecurePath(workdirFromCtx(ctx), path, false)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	f, err := os.Open(fp)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
	defer f.Close()

	var buf bytes.Buffer
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*64), infra.MaxOutputLen)

	lineCount := 0
	for scanner.Scan() {
		if offset > 0 && lineCount < offset {
			lineCount++
			continue
		}
		if limit > 0 && lineCount-offset >= limit {
			buf.WriteString(fmt.Sprintf("... (%d more lines)\n", countRemainingLines(f)))
			break
		}
		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(scanner.Text())
		lineCount++
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

	// Hint the model to page when the default cap truncated the file.
	if offset == 0 && limit == infra.ReadFileDefaultLimit && hasMoreLines(f) {
		result += fmt.Sprintf("\n... (showing first %d lines; pass a larger `limit` or an `offset` to read more)", infra.ReadFileDefaultLimit)
	}

	// Sanitize secrets from file content (only if content is not too large)
	if len(result) < 50000 && security.GlobalSecretsSanitizer.Detect(result) {
		result = security.GlobalSecretsSanitizer.Sanitize(result)
	}
	return result
}

// hasMoreLines reports whether the file has unread content past the scanner position.
func hasMoreLines(f *os.File) bool {
	pos, _ := f.Seek(0, 1)
	defer f.Seek(pos, 0)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*64), 64*1024)
	return scanner.Scan()
}

// countRemainingLines estimates remaining lines (capped at 1000).
func countRemainingLines(f *os.File) int {
	pos, _ := f.Seek(0, 1)
	defer f.Seek(pos, 0)
	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() && count < 1000 {
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
// Includes TOCTOU protection and whitespace-tolerant matching.
func secureEditFile(ctx context.Context, path, oldText, newText string, replaceAll bool) string {
	// Check edit approval
	if approved, msg := checkToolApproval("edit_file", path); !approved && !security.GlobalApproval.IsAutoApproveSafe() && !security.GlobalApproval.IsAutoApproveAll() {
		return fmt.Sprintf("\u274c %s", msg)
	}

	fp, err := security.SecurePath(workdirFromCtx(ctx), path, true)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	// Baseline mtime for the concurrent-modification check below - this
	// guards against a lost update (someone else changes the file while
	// we're computing the diff / waiting on PreviewAndConfirm), which
	// os.Rename's write-atomicity does NOT protect against.
	fi, err := os.Stat(fp)
	if err != nil {
		return fmt.Sprintf("Error stating file: %v", err)
	}
	originalMtime := fi.ModTime()

	data, err := os.ReadFile(fp)
	if err != nil {
		return fmt.Sprintf("Error reading file: %v", err)
	}
	content := string(data)
	matchedOld := oldText
	whitespaceTolerant := false
	if !strings.Contains(content, oldText) {
		if m, count := findWhitespaceTolerantMatch(content, oldText); count == 1 {
			matchedOld = m
			whitespaceTolerant = true
		} else if count > 1 {
			if replaceAll {
				// replace_all: apply every whitespace-tolerant match in
				// order, each at its exact (real-whitespace) location.
				newContent := content
				offset := 0
				for {
					m, found := findWhitespaceTolerantMatchAt(newContent[offset:], oldText)
					if !found {
						break
					}
					idx := offset + strings.Index(newContent[offset:], m)
					newContent = newContent[:idx] + newText + newContent[idx+len(m):]
					offset = idx + len(newText)
				}
				content = newContent
				matchedOld = oldText
				whitespaceTolerant = false
			} else {
				return fmt.Sprintf("Error: Text not found in %s (exact match), and %d locations match when ignoring whitespace - too ambiguous to pick one automatically. Add more surrounding context to old_text to make it unique, or set replace_all=true to replace all of them.%s",
					path, count, closestMatchHint(content, oldText))
			}
		} else {
			return fmt.Sprintf("Error: Text not found in %s.%s", path, closestMatchHint(content, oldText))
		}
	}

	// Perform replacement. With replace_all, every occurrence is
	// replaced; otherwise only the first occurrence in the file is
	// changed (deliberately, so a non-unique old_text does not
	// silently mutate unrelated locations unless the caller opts in).
	var newContent string
	if replaceAll {
		newContent = strings.ReplaceAll(content, matchedOld, newText)
	} else {
		newContent = strings.Replace(content, matchedOld, newText, 1)
	}

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

	// Final check right before the write: catches a concurrent edit
	// that happened during diff generation / user confirmation above.
	if fi2, err := os.Stat(fp); err != nil {
		return fmt.Sprintf("Error checking file before write: %v", err)
	} else if !fi2.ModTime().Equal(originalMtime) {
		return fmt.Sprintf("Error: File '%s' was modified during editing (concurrent modification detected)", path)
	}

	tmpPath := fmt.Sprintf("%s.tmp.%d.%d", fp, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, []byte(newContent), 0o644); err != nil {
		os.Remove(tmpPath)
		return fmt.Sprintf("Error writing temp file: %v", err)
	}
	if err := os.Rename(tmpPath, fp); err != nil {
		os.Remove(tmpPath)
		// Fallback: rename might fail across devices, try direct write
		if err2 := os.WriteFile(fp, []byte(newContent), 0o644); err2 != nil {
			return fmt.Sprintf("Error writing file (atomic+direct both failed): %v / %v", err, err2)
		}
	}

	if whitespaceTolerant {
		return fmt.Sprintf("Edited %s (matched ignoring leading/trailing whitespace differences per line - the file's original indentation was preserved for surrounding text)", path)
	}
	return fmt.Sprintf("Edited %s", path)
}

// secureInsertFile inserts newText after a 1-based line number.
// afterLine==0 prepends; afterLine>=line count appends.
// Shares edit_file's safety posture (security gate, TOCTOU, diff preview, atomic write).
func secureInsertFile(ctx context.Context, path string, afterLine int, newText string) string {
	if approved, msg := checkToolApproval("insert_file", path); !approved && !security.GlobalApproval.IsAutoApproveSafe() && !security.GlobalApproval.IsAutoApproveAll() {
		return fmt.Sprintf("\u274c %s", msg)
	}

	fp, err := security.SecurePath(workdirFromCtx(ctx), path, true) // allowWrite=true
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	// Baseline mtime (zero-value means the file didn't exist yet) - see
	// secureEditFile's comment for why this matters even with atomic rename.
	var originalMtime time.Time
	if fi, serr := os.Stat(fp); serr == nil {
		originalMtime = fi.ModTime()
	}

	var content string
	if data, rerr := os.ReadFile(fp); rerr == nil {
		content = string(data)
	} else if !os.IsNotExist(rerr) {
		return fmt.Sprintf("Error reading file: %v", rerr)
	}

	hadTrailingNewline := strings.HasSuffix(content, "\n")
	lines := strings.Split(content, "\n")
	if hadTrailingNewline {
		lines = lines[:len(lines)-1]
	}
	if content == "" {
		lines = nil
	}

	if afterLine < 0 {
		afterLine = 0
	}
	if afterLine > len(lines) {
		afterLine = len(lines)
	}

	insertLines := strings.Split(newText, "\n")
	head := lines[:afterLine]
	tail := lines[afterLine:]
	newLines := make([]string, 0, len(lines)+len(insertLines))
	newLines = append(newLines, head...)
	newLines = append(newLines, insertLines...)
	newLines = append(newLines, tail...)
	newContent := strings.Join(newLines, "\n")
	if hadTrailingNewline || content == "" {
		newContent += "\n"
	}

	if security.ShouldPreviewDiff() && len(content) < 500000 {
		diff, derr := security.GenerateUnifiedDiff(content, newContent, path)
		if derr != nil {
			return fmt.Sprintf("Error generating diff: %v", derr)
		}
		if diff != "" {
			applied, ok := security.PreviewAndConfirm(path, content, newContent, diff)
			if !ok {
				return "\u274c File insert rejected by user"
			}
			newContent = applied
		}
	}

	// Final check right before the write (see secureEditFile).
	if fi, serr := os.Stat(fp); serr == nil {
		if !originalMtime.IsZero() && !fi.ModTime().Equal(originalMtime) {
			return fmt.Sprintf("Error: File '%s' was modified during editing (concurrent modification detected)", path)
		}
	} else if !originalMtime.IsZero() {
		return fmt.Sprintf("Error checking file before write: %v", serr)
	}

	if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
		return fmt.Sprintf("Error creating directory: %v", err)
	}
	tmpPath := fmt.Sprintf("%s.tmp.%d.%d", fp, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, []byte(newContent), 0o644); err != nil {
		os.Remove(tmpPath)
		return fmt.Sprintf("Error writing temp file: %v", err)
	}
	if err := os.Rename(tmpPath, fp); err != nil {
		os.Remove(tmpPath)
		if err2 := os.WriteFile(fp, []byte(newContent), 0o644); err2 != nil {
			return fmt.Sprintf("Error writing file (atomic+direct both failed): %v / %v", err, err2)
		}
	}
	return fmt.Sprintf("Inserted %d line(s) into %s after line %d", len(insertLines), path, afterLine)
}

// findWhitespaceTolerantMatch returns a substring of content whose lines
// match oldText after per-line TrimSpace, tolerating indentation/spacing
// differences. Returns (match, count) where count is the number of
// matching positions; callers should only use the match when count==1.
// O(lines*lines) — oldText is typically only a few lines.
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

func findWhitespaceTolerantMatchAt(content, oldText string) (match string, found bool) {
	oldLines := strings.Split(oldText, "\n")
	if len(oldLines) == 0 {
		return "", false
	}
	trimmedOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		trimmedOld[i] = strings.TrimSpace(l)
	}

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
	for start := 0; start+n <= len(spans); start++ {
		ok := true
		for j := range n {
			line := content[spans[start+j].start:spans[start+j].end]
			if strings.TrimSpace(line) != trimmedOld[j] {
				ok = false
				break
			}
		}
		if ok {
			return content[spans[start].start:spans[start+n-1].end], true
		}
	}
	return "", false
}

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
