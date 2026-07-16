package hitlaudit

import (
	"encoding/json"
	"fmt"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/security"
	"go-code-agent/utils"
	"os"
	"strings"
	"sync"
)

// isStdinTTY reports whether stdin is a terminal, to fail fast in
// interactive HITL mode when no human is reachable.
func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// Human-in-the-Loop Approval: gates critical tool invocations behind
// explicit human sign-off. Second line of defense on top of security.go's
// static ApprovalLevel. Opt-in; when disabled NeedsReview returns false.

// HITLDecision captures the three possible outcomes of a human review.
type HITLDecision int

const (
	HITLApprove HITLDecision = iota
	HITLReject
	HITLModify
)

// HITLResponse carries the decision plus any free-form feedback the operator
// typed when choosing "modify".
type HITLResponse struct {
	Decision HITLDecision
	Feedback string
}

// HITLMode controls runtime behavior when a tool requires approval.
type HITLMode int

const (
	// HITLModeInteractive prompts the operator at the terminal and blocks.
	HITLModeInteractive HITLMode = iota
	// HITLModeAutoApprove logs the event and lets the tool through. Useful
	// for CI / non-interactive batch runs that want the notification trail
	// without blocking.
	HITLModeAutoApprove
	// HITLModeAutoReject rejects every gated tool. Useful as a panic switch
	// or for sandboxed evaluations.
	HITLModeAutoReject
	// HITLModeNotifyOnly logs the event and approves, identical to auto-
	// approve today; kept separate so logging policy can diverge later.
	HITLModeNotifyOnly
)

func (m HITLMode) String() string {
	switch m {
	case HITLModeInteractive:
		return "interactive"
	case HITLModeAutoApprove:
		return "auto-approve"
	case HITLModeAutoReject:
		return "auto-reject"
	case HITLModeNotifyOnly:
		return "notify-only"
	}
	return "unknown"
}

// HITLRequest is what the agent loop hands the manager for review.
type HITLRequest struct {
	ToolName  string
	Arguments string // raw JSON; the manager pretty-prints selectively
	RiskLevel string // "low" | "medium" | "high"
	Reason    string // why this invocation was escalated

	// SessionID is the active session at the moment of the gate. Carried
	// through purely for the audit log (hitlaudit.go) - the prompt UI
	// itself never displays it.
	SessionID string
}

// HITLManager tracks whether HITL is active and renders prompts.
type HITLManager struct {
	enabled bool
	mode    HITLMode

	// toolsRequiringReview: whitelist of tool names that always trigger
	// HITL review when enabled=true. Empty slice = default risky set.
	toolsRequiringReview map[string]bool

	// dangerousBashPrefixes: bash command prefixes that force review even when
	// "bash" is not in toolsRequiringReview. Checked against the raw arguments
	// of a bash tool call.
	dangerousBashPrefixes []string

	// safeBashPrefixes: bash command prefixes that are explicitly safe and
	// should never trigger HITL review. This provides an explicit exclusion
	// list to avoid false positives. Examples: ls, find, cat, echo, pwd.
	safeBashPrefixes []string

	// criticalPathSubstrings: if write_file / edit_file / delete_file
	// targets a path containing any of these, escalate to HITL.
	criticalPathSubstrings []string

	mu sync.RWMutex
}

// NewHITLManager returns a disabled manager populated with sensible defaults.
// Enable it via SetEnabled(true) after flags are parsed.
func NewHITLManager() *HITLManager {
	return &HITLManager{
		enabled: false,
		mode:    HITLModeInteractive,
		toolsRequiringReview: map[string]bool{
			// Destructive file ops always get a human.
			"delete_file": true,
			// Shell escape: even allowlisted bash can surprise us.
			"bash":            true,
			"execute_command": true,
			"background_run":  true,
		},
		dangerousBashPrefixes: []string{
			"rm -rf", "rm -r", "rm /",
			"git push --force", "git push -f",
			"git reset --hard",
			"git clean -f",
			"docker rm", "docker rmi", "docker system prune",
			"npm publish", "cargo publish",
			"kubectl delete", "kubectl apply -f",
			"terraform apply", "terraform destroy",
		},
		// safeBashPrefixes: read-only / inspection-only executables that
		// should bypass HITL review. Matched against the FIRST token of
		// the (sub)command — never as a substring — so "tools" cannot
		// silently match "ls" and "controls" cannot match "cat".
		//
		// Anything that mutates filesystem / network / processes (rm, mv,
		// cp, chmod, kill, curl with -o/--data, git push, etc.) MUST NOT
		// be added here. When in doubt: leave it out.
		safeBashPrefixes: []string{
			// Listing / navigation
			"ls", "ll", "la", "tree", "pwd", "cd",
			// Read-only file inspection
			"cat", "head", "tail", "less", "more", "nl",
			"wc", "stat", "file", "du", "df",
			// Search / pattern (read-only)
			"grep", "egrep", "fgrep", "rg", "ag",
			"find", "locate", "which", "whereis", "type",
			// Print / trivial
			"echo", "printf", "true", "false",
			"date", "whoami", "hostname", "id", "uname", "uptime",
			"env", "printenv",
			// Go read-only / build-only verbs (no install / no clean)
			"go test", "go build", "go vet", "go run", "go list",
			"go doc", "go env", "go version", "go mod",
			"gofmt", "goimports",
		},
		criticalPathSubstrings: []string{
			".env", ".env.local", ".env.production",
			"credentials", "secrets",
			"Dockerfile", "docker-compose",
			"go.mod", "package.json",
			// Agent's own source code: prevent self-modification surprises.
			"internal/main.go", "internal/agent_loop.go", "internal/security.go",
		},
	}
}

// SetEnabled toggles HITL at runtime.
func (h *HITLManager) SetEnabled(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = v
}

// IsEnabled reports current state.
func (h *HITLManager) IsEnabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.enabled
}

// SetMode configures the decision strategy.
func (h *HITLManager) SetMode(mode HITLMode) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mode = mode
}

// Mode returns current mode.
func (h *HITLManager) Mode() HITLMode {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.mode
}

// NeedsReview classifies an upcoming tool invocation. Returns (needReview, riskLevel, reason).
// When the manager is disabled, this ALWAYS returns false regardless of args.
//
// Decision order for shell-style tools (bash / execute_command / background_run):
//  1. If the command contains a *dangerous* prefix (rm -rf, git push -f, ...)
//     → always review at high risk, regardless of any safe-prefix overlap.
//  2. Otherwise, if every sub-command of the pipeline matches a *safe*
//     allow-listed executable (ls, cat, grep, go test, ...) → bypass review.
//     Compound commands joined by "&&", "||", ";" or "|" are split and each
//     piece is checked independently so that "cd X && go test" is recognised
//     as safe.
//  3. Otherwise the shell call falls through to medium-risk review.
//
// For non-shell tools the previous behaviour is unchanged: whitelisted tools
// (delete_file, ...) always review, and write/edit/delete on critical paths
// escalates to review.
func (h *HITLManager) NeedsReview(toolName, arguments string) (bool, string, string) {
	if !h.IsEnabled() {
		return false, "", ""
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	isShellTool := toolName == "bash" || toolName == "execute_command" || toolName == "background_run"

	// Shell-style tools: dangerous-first, then safe-allowlist bypass, then review.
	if isShellTool && h.toolsRequiringReview[toolName] {
		if prefix := h.matchBashPrefix(arguments); prefix != "" {
			return true, "high",
				fmt.Sprintf("Bash command matches dangerous prefix '%s'", prefix)
		}
		if h.allSubCommandsSafe(arguments) {
			return false, "low", "Bash command is read-only / inspection-only"
		}
		return true, "medium",
			fmt.Sprintf("Shell execution via '%s' requires review", toolName)
	}

	// Non-shell whitelisted tools (delete_file, ...): always review.
	if h.toolsRequiringReview[toolName] {
		return true, "high",
			fmt.Sprintf("Tool '%s' is always reviewed under HITL policy", toolName)
	}

	// Critical-path writes/edits/deletes.
	if toolName == "write_file" || toolName == "edit_file" || toolName == "delete_file" {
		if path := extractPathArg(arguments); path != "" {
			if sub := h.matchCriticalPath(path); sub != "" {
				return true, "high",
					fmt.Sprintf("Target path '%s' matches critical substring '%s'", path, sub)
			}
		}
	}

	return false, "", ""
}

// matchBashPrefix checks if any dangerous bash prefix appears inside the
// bash tool's command argument.
func (h *HITLManager) matchBashPrefix(arguments string) string {
	cmd := extractBashCommand(arguments)
	if cmd == "" {
		return ""
	}
	lower := strings.ToLower(cmd)
	for _, p := range h.dangerousBashPrefixes {
		if strings.Contains(lower, strings.ToLower(p)) {
			return p
		}
	}
	return ""
}

// allSubCommandsSafe returns true iff every sub-command of the bash argument
// is on the safe allow-list. The argument may be a compound shell expression
// joined by "&&", "||", ";" or "|"; each piece is examined independently so
// that "cd internal/memory && go test -v" is correctly recognised as safe.
//
// Empty arguments → false (we don't bypass when we can't see what runs).
func (h *HITLManager) allSubCommandsSafe(arguments string) bool {
	cmd := extractBashCommand(arguments)
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	parts := splitShellPipeline(cmd)
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if !h.isSafeSingleCommand(p) {
			return false
		}
	}
	return true
}

// splitShellPipeline splits a shell expression on the unquoted operators
// "&&", "||", ";" and "|" into individual command strings. It is intentionally
// conservative: the goal is "did the operator chain only safe commands?", not
// full shell parsing. Quotes and backslashes are honoured for splitting; we
// do NOT expand variables, glob, or evaluate sub-shells.
func splitShellPipeline(cmd string) []string {
	var out []string
	var cur strings.Builder
	var quote byte // 0, '\'', or '"'
	flush := func() {
		s := strings.TrimSpace(cur.String())
		if s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		// In quotes: copy verbatim until the matching close quote.
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
			// "2>&1", ">&2": '&' immediately follows '>' as part of a
			// redirection — keep it in the current command.
			prev := byte(0)
			if cur.Len() > 0 {
				s := cur.String()
				prev = s[len(s)-1]
			}
			if prev == '>' || prev == '<' {
				cur.WriteByte(c)
				continue
			}
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				flush()
				i++
			} else {
				// Background "&" — treat as separator too; safer to split.
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

// isSafeSingleCommand checks one shell sub-command against the safe allow-list.
// The first whitespace-delimited token is treated as the executable. Multi-word
// allow-list entries (e.g. "go test") are matched against the first N tokens.
//
// Special-cases:
//   - "sed" is only safe when invoked with "-n" (print-only mode); plain
//     "sed" can mutate files via "-i".
//   - "awk" is intentionally NOT in the safe list — it can shell-out via
//     system().
func (h *HITLManager) isSafeSingleCommand(cmd string) bool {
	tokens := tokenizeShellWords(cmd)
	if len(tokens) == 0 {
		return false
	}
	// Allow leading env assignments like FOO=bar BAZ=1 cmd ...
	for len(tokens) > 0 && isEnvAssignment(tokens[0]) {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return false
	}
	exe := tokens[0]

	// sed special-case: only "-n" usage is read-only.
	if exe == "sed" {
		for _, t := range tokens[1:] {
			if t == "-n" || strings.HasPrefix(t, "-n") {
				return true
			}
		}
		return false
	}

	for _, p := range h.safeBashPrefixes {
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

// isEnvAssignment reports whether a token looks like NAME=VALUE (a leading
// environment-variable assignment in shell, e.g. "GOFLAGS=-count=1 go test").
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
		if !(c == '_' ||
			(c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(i > 0 && c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// tokenizeShellWords splits a single shell command into argv-like tokens.
// It honours single/double quotes and backslash escapes but does not perform
// expansion. Sufficient for allow-list classification, NOT a full lexer.
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

// matchCriticalPath checks a file path against the critical substrings list.
func (h *HITLManager) matchCriticalPath(path string) string {
	lower := strings.ToLower(path)
	for _, s := range h.criticalPathSubstrings {
		if strings.Contains(lower, strings.ToLower(s)) {
			return s
		}
	}
	return ""
}

// RequestApproval dispatches to the appropriate UX for the current mode.
// It is safe to call even when disabled (returns Approve).
//
// Every decision produced here - including auto-approve / auto-reject /
// notify-only - is appended to the HITL audit log via hitlAuditor.Record.
// We never audit the disabled-manager fast path: when HITL is off, no
// review actually happened, and writing a row would be misleading.
func (h *HITLManager) RequestApproval(req HITLRequest) HITLResponse {
	if !h.IsEnabled() {
		return HITLResponse{Decision: HITLApprove}
	}

	mode := h.Mode()
	resp := h.decide(req, mode)
	hitlAuditor.Record(req, mode, resp)
	return resp
}

// decide produces the HITLResponse without touching audit/IO side effects.
// Splitting it out keeps RequestApproval's audit-record path single-exit
// and easy to reason about.
func (h *HITLManager) decide(req HITLRequest, mode HITLMode) HITLResponse {
	switch mode {
	case HITLModeAutoApprove:
		// Only surface high-risk approvals on the console; low/medium are
		// pure noise in auto-approve mode (e.g. routine `bash` calls that
		// merely "require review"). The full trail — including low/medium —
		// is still persisted to hitl_audit.jsonl by RequestApproval, so the
		// audit record is never lost; we only quiet the interactive output.
		if req.RiskLevel == "high" {
			logging.PrintSystem(fmt.Sprintf("[hitl] auto-approve: %s (%s) - %s",
				req.ToolName, req.RiskLevel, req.Reason))
		}
		return HITLResponse{Decision: HITLApprove}

	case HITLModeAutoReject:
		logging.PrintSystem(fmt.Sprintf("[hitl] auto-reject: %s (%s) - %s",
			req.ToolName, req.RiskLevel, req.Reason))
		return HITLResponse{Decision: HITLReject}

	case HITLModeNotifyOnly:
		logging.PrintSystem(fmt.Sprintf("[hitl] NOTIFY: %s (%s) - %s",
			req.ToolName, req.RiskLevel, req.Reason))
		return HITLResponse{Decision: HITLApprove}

	case HITLModeInteractive:
		// Bubble check: interactive mode requires a real tty to ask the
		// operator. If we don't have one (background goroutine, cron
		// task, CI, container entrypoint with no -it), there is no human
		// to ask — we deliberately do NOT invent a remote channel here.
		// Auto-reject loudly so the agent gets a clear signal and the
		// audit log records what was attempted.
		if !isStdinTTY() {
			logging.PrintSystem(fmt.Sprintf("[hitl] no tty available, auto-rejecting %s (%s) - %s; "+
				"re-run with --human-mode auto-approve or auto-reject for non-interactive contexts",
				req.ToolName, req.RiskLevel, req.Reason))
			return HITLResponse{Decision: HITLReject}
		}
		fallthrough
	default:
		return h.promptInteractive(req)
	}
}

// promptInteractive renders a block-style prompt and reads a decision from stdin.
// Loops until the operator gives a valid answer.
func (h *HITLManager) promptInteractive(req HITLRequest) HITLResponse {
	divider := strings.Repeat("=", 60)
	fmt.Println()
	fmt.Println(divider)
	fmt.Println("HUMAN APPROVAL REQUIRED")
	fmt.Println(divider)
	fmt.Printf("  Tool       : %s\n", req.ToolName)
	fmt.Printf("  Risk level : %s\n", req.RiskLevel)
	fmt.Printf("  Reason     : %s\n", req.Reason)
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  Arguments  :")
	fmt.Println(indent(prettyPrintArgs(req.Arguments), "    "))
	fmt.Println(divider)
	fmt.Println("Options:")
	fmt.Println("  [y] approve  - run the tool as-is")
	fmt.Println("  [n] reject   - veto, agent will pick another approach")
	fmt.Println("  [m] modify   - veto and provide guidance to the agent")

	reader := security.ReadLine
	for {
		raw, err := reader("Your choice [y/n/m]: ")
		if err != nil {
			// stdin closed or unreadable: default to reject, never silently
			// approve something that couldn't be confirmed.
			logging.PrintSystem("[hitl] stdin unavailable, defaulting to reject")
			return HITLResponse{Decision: HITLReject}
		}
		choice := strings.ToLower(strings.TrimSpace(raw))
		switch choice {
		case "y", "yes", "approve":
			fmt.Println("[hitl] approved")
			return HITLResponse{Decision: HITLApprove}
		case "n", "no", "reject":
			fmt.Println("[hitl] rejected")
			return HITLResponse{Decision: HITLReject}
		case "m", "modify":
			fb, _ := reader("Feedback / instructions for the agent: ")
			fb = strings.TrimSpace(fb)
			fmt.Println("[hitl] modified with feedback")
			return HITLResponse{Decision: HITLModify, Feedback: fb}
		default:
		}
	}
}

// Formatting helpers ---------------------------------------------------------

// extractPathArg digs a "path" string out of a tool call's JSON arguments.
// Returns "" if the arguments aren't JSON or don't contain a path field.
func extractPathArg(arguments string) string {
	if arguments == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return ""
	}
	if v, ok := m["path"].(string); ok {
		return v
	}
	return ""
}

// extractBashCommand pulls the "command" field from a bash / execute_command
// tool call's JSON arguments.
func extractBashCommand(arguments string) string {
	if arguments == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return ""
	}
	if v, ok := m["command"].(string); ok {
		return v
	}
	return ""
}

// prettyPrintArgs tries to render JSON-encoded tool arguments with indentation;
// falls back to raw text (truncated) when parsing fails.
func prettyPrintArgs(arguments string) string {
	if arguments == "" {
		return "(none)"
	}
	var tmp any
	if err := json.Unmarshal([]byte(arguments), &tmp); err == nil {
		pretty, _ := json.MarshalIndent(tmp, "", "  ")
		return utils.Truncate(string(pretty), 1500)
	}
	return utils.Truncate(arguments, 1500)
}

// indent prepends `prefix` to every line of `text`.
func indent(text, prefix string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// Feedback injection helpers -------------------------------------------------

// promptLoader is set by the main package at init time via SetPromptLoader.
// This avoids a circular import while still allowing the hitlaudit package
// to load prompt templates.
var promptLoader *prompt.Loader

// SetPromptLoader configures the prompt loader used by FormatRejectMessage
// and FormatModifyMessage. Called once at startup from main.
func SetPromptLoader(l *prompt.Loader) {
	promptLoader = l
}

// FormatRejectMessage produces the text injected back into the agent
// conversation when the operator rejects a tool call. The template lives in
// prompts/human_reject.md.
func FormatRejectMessage(toolName, reason string) string {
	tmpl := ""
	if promptLoader != nil {
		tmpl = promptLoader.Load("human_reject")
	}
	return prompt.Render(tmpl, map[string]string{
		"tool":   toolName,
		"reason": reason,
	})
}

// FormatModifyMessage wraps operator feedback into a block the LLM can act on.
// The template lives in prompts/human_modify.md.
func FormatModifyMessage(toolName, feedback string) string {
	if feedback == "" {
		feedback = "(no additional feedback provided)"
	}
	tmpl := ""
	if promptLoader != nil {
		tmpl = promptLoader.Load("human_modify")
	}
	return prompt.Render(tmpl, map[string]string{
		"tool":     toolName,
		"feedback": feedback,
	})
}

// Global singleton, initialized at package load. main() enables it when --human is set.
var HitlManager = NewHITLManager()
