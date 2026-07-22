package hitlaudit

import (
	"encoding/json"
	"fmt"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/security"
	"go-code-agent/internal/tool"
	"go-code-agent/internal/utils"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

type HITLDecision int

const (
	HITLApprove HITLDecision = iota
	HITLReject
	HITLModify
)

type HITLResponse struct {
	Decision HITLDecision
	Feedback string
}

type HITLMode int

const (
	HITLModeInteractive HITLMode = iota
	HITLModeAutoApprove
	HITLModeAutoReject
	HITLModeNotifyOnly
	HITLModeSafeOnly
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
	case HITLModeSafeOnly:
		return "safe-only"
	default:
		return "unknown"
	}
}

type HITLRequest struct {
	ToolName  string
	Arguments string
	RiskLevel string
	Reason    string
	SessionID string
}

type HITLManager struct {
	enabled                bool
	mode                   HITLMode
	nonTTYFallback         HITLDecision
	toolsRequiringReview   map[string]bool
	dangerousBashPrefixes  []string
	safeBashPrefixes       []string
	criticalPathSubstrings []string
	mu                     sync.RWMutex
	promptLoader           *prompt.Loader
}

func NewHITLManager(pl *prompt.Loader) *HITLManager {
	fallback := HITLReject
	if os.Getenv("HITL_NON_TTY_FALLBACK") == "approve" {
		fallback = HITLApprove
	}
	return &HITLManager{
		enabled:        false,
		mode:           HITLModeInteractive,
		nonTTYFallback: fallback,
		promptLoader:   pl,
		toolsRequiringReview: map[string]bool{
			"delete_file": true, "bash": true, "execute_command": true, "background_run": true,
		},
		dangerousBashPrefixes: []string{
			"rm -rf", "rm -r", "rm /", "git push --force", "git push -f",
			"git reset --hard", "git clean -f", "docker rm", "docker rmi",
			"npm publish", "cargo publish", "kubectl delete", "terraform apply", "terraform destroy",
		},
		safeBashPrefixes: []string{
			"ls", "ll", "la", "pwd", "cd",
			"cat", "head", "tail", "less", "more",
			"grep", "egrep", "fgrep", "rg", "ag",
			"find", "locate", "which", "whereis",
			"echo", "printf", "date", "whoami", "hostname", "uname",
			"go test", "go build", "go vet", "go run", "go list", "go doc", "go mod",
			"wc", "stat", "file", "du", "df", "env", "printenv",
		},
		criticalPathSubstrings: []string{
			".env", ".env.local", ".env.production", "credentials", "secrets",
			"Dockerfile", "docker-compose", "go.mod", "package.json",
			"internal/main.go", "internal/agent_loop.go",
		},
	}
}

func (h *HITLManager) SetEnabled(v bool)  { h.mu.Lock(); defer h.mu.Unlock(); h.enabled = v }
func (h *HITLManager) IsEnabled() bool    { h.mu.RLock(); defer h.mu.RUnlock(); return h.enabled }
func (h *HITLManager) SetMode(m HITLMode) { h.mu.Lock(); defer h.mu.Unlock(); h.mode = m }
func (h *HITLManager) Mode() HITLMode     { h.mu.RLock(); defer h.mu.RUnlock(); return h.mode }

func (h *HITLManager) NeedsReview(toolName, arguments string) (bool, string, string) {
	if !h.IsEnabled() {
		return false, "", ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()

	isShellTool := toolName == "bash" || toolName == "execute_command" || toolName == "background_run"

	if isShellTool && h.toolsRequiringReview[toolName] {
		if prefix := h.matchBashPrefix(arguments); prefix != "" {
			return true, "high", fmt.Sprintf("bash matches dangerous prefix '%s'", prefix)
		}
		if h.allSubCommandsSafe(arguments) {
			return false, "low", "bash command is read-only/inspection-only"
		}
		return true, "medium", fmt.Sprintf("shell execution via '%s' requires review", toolName)
	}

	if h.toolsRequiringReview[toolName] {
		return true, "high", fmt.Sprintf("tool '%s' is always reviewed", toolName)
	}

	if toolName == "write_file" || toolName == "edit_file" || toolName == "delete_file" {
		if p := extractPathArg(arguments); p != "" {
			if sub := h.matchCriticalPath(p); sub != "" {
				return true, "high", fmt.Sprintf("target path '%s' matches critical substring '%s'", p, sub)
			}
		}
	}
	return false, "", ""
}

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
				prev = []byte(cur.String())[cur.Len()-1]
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

func (h *HITLManager) isSafeSingleCommand(cmd string) bool {
	tokens := tokenizeShellWords(cmd)
	if len(tokens) == 0 {
		return false
	}
	for len(tokens) > 0 && isEnvAssignment(tokens[0]) {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return false
	}
	exe := tokens[0]
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

func (h *HITLManager) matchCriticalPath(path string) string {
	lower := strings.ToLower(path)
	for _, s := range h.criticalPathSubstrings {
		if strings.Contains(lower, strings.ToLower(s)) {
			return s
		}
	}
	return ""
}

func (h *HITLManager) RequestApproval(req HITLRequest) HITLResponse {
	if !h.IsEnabled() {
		return HITLResponse{Decision: HITLApprove}
	}
	mode := h.Mode()
	switch mode {
	case HITLModeAutoApprove:
		h.printReviewHeader(req)
		fmt.Println("[hitl] auto-approved")
		return HITLResponse{Decision: HITLApprove}
	case HITLModeSafeOnly:
		// Show preview but auto-approve RiskSafe; prompt for RiskDanger.
		if strings.EqualFold(req.RiskLevel, "danger") || strings.EqualFold(req.RiskLevel, "high") {
			h.printReviewHeader(req)
			return h.promptInteractive(req)
		}
		h.printReviewHeader(req)
		fmt.Println("[hitl] auto-approved (safe)")
		return HITLResponse{Decision: HITLApprove}
	case HITLModeAutoReject:
		h.printReviewHeader(req)
		fmt.Printf("[hitl] auto-rejected (%s)\n", req.RiskLevel)
		return HITLResponse{Decision: HITLReject}
	case HITLModeNotifyOnly:
		h.printReviewHeader(req)
		fmt.Println("[hitl] proceeding (notify-only)")
		return HITLResponse{Decision: HITLApprove}
	default:
		return h.promptInteractive(req)
	}
}

func (h *HITLManager) printReviewHeader(req HITLRequest) {
	fmt.Println()
	fmt.Printf("[hitl] reviewing %s", req.ToolName)
	if req.RiskLevel != "" {
		fmt.Printf(" [%s]", req.RiskLevel)
	}
	fmt.Println()
	if req.Reason != "" {
		// Reason already contains the diff preview appended by DecideTool.
		fmt.Println(req.Reason)
	}
}

func (h *HITLManager) promptInteractive(req HITLRequest) HITLResponse {
	if !isStdinTTY() {
		if h.nonTTYFallback == HITLApprove {
			fmt.Printf("[hitl] no tty, auto-approving %s (fallback=approve)\n", req.ToolName)
			return HITLResponse{Decision: HITLApprove}
		}
		fmt.Printf("[hitl] no tty, auto-rejecting %s (set HITL_NON_TTY_FALLBACK=approve to change)\n", req.ToolName)
		return HITLResponse{Decision: HITLReject}
	}
	divider := strings.Repeat("=", 60)
	fmt.Println()
	fmt.Println(utils.Bold + divider)
	fmt.Println("HUMAN APPROVAL REQUIRED")
	fmt.Println(divider + utils.Reset)
	fmt.Printf("  Tool       : %s%s%s\n", utils.BoldYellow, req.ToolName, utils.Reset)
	fmt.Printf("  Risk level : %s\n", req.RiskLevel)
	if req.Reason != "" {
		fmt.Printf("  Reason     : %s\n", req.Reason)
	}
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println("  Arguments  :")
	fmt.Println(indent(prettyPrintArgs(req.Arguments), "    "))
	fmt.Println(divider)
	fmt.Println("  [y] approve  — run the tool as-is")
	fmt.Println("  [n] reject   — veto, agent will pick another approach")
	fmt.Println("  [m] modify   — veto and provide guidance to the agent")

	for {
		raw, err := security.ReadLine("Your choice [y/n/m]: ")
		if err != nil {
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
			fb, _ := security.ReadLine("Feedback for the agent: ")
			fb = strings.TrimSpace(fb)
			fmt.Println("[hitl] modified with feedback")
			return HITLResponse{Decision: HITLModify, Feedback: fb}
		}
	}
}

func extractPathArg(arguments string) string {
	if arguments == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(arguments), &m) != nil {
		return ""
	}
	if v, ok := m["path"].(string); ok {
		return v
	}
	return ""
}

func extractBashCommand(arguments string) string {
	if arguments == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal([]byte(arguments), &m) != nil {
		return ""
	}
	if v, ok := m["command"].(string); ok {
		return v
	}
	return ""
}

func prettyPrintArgs(arguments string) string {
	if arguments == "" {
		return "(none)"
	}
	var tmp any
	if json.Unmarshal([]byte(arguments), &tmp) == nil {
		pretty, _ := json.MarshalIndent(tmp, "", "  ")
		return utils.Truncate(string(pretty), 1500)
	}
	return utils.Truncate(arguments, 1500)
}

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

func FormatRejectMessage(toolName, reason string, pl *prompt.Loader) string {
	tmpl := ""
	if pl != nil {
		tmpl = pl.Load("human_reject")
	}
	return prompt.Render(tmpl, map[string]string{"tool": toolName, "reason": reason})
}

func FormatModifyMessage(toolName, feedback string, pl *prompt.Loader) string {
	if feedback == "" {
		feedback = "(no additional feedback)"
	}
	tmpl := ""
	if pl != nil {
		tmpl = pl.Load("human_modify")
	}
	return prompt.Render(tmpl, map[string]string{"tool": toolName, "feedback": feedback})
}

// HITLApprovalAdapter adapts HITLManager to the tool.ApprovalChecker interface,
// enabling the executor to call HITL for human-in-the-loop approval.
type HITLApprovalAdapter struct {
	mgr                 *HITLManager
	workdir             string
	mu                  sync.Mutex
	lastAcceptedContent string
}

func NewHITLApprovalAdapter(mgr *HITLManager) *HITLApprovalAdapter {
	return &HITLApprovalAdapter{mgr: mgr}
}

func (a *HITLApprovalAdapter) SetWorkdir(wd string) { a.workdir = wd }

// AcceptedContent returns the partially-accepted content from chunk-by-chunk
// diff review, or empty string if no review happened or content wasn't modified.
func (a *HITLApprovalAdapter) AcceptedContent() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastAcceptedContent
}

func (a *HITLApprovalAdapter) readOldContent(path string) string {
	if path == "" || a.workdir == "" {
		return ""
	}
	content, err := os.ReadFile(filepath.Join(a.workdir, path))
	if err != nil {
		return ""
	}
	return string(content)
}

func extractPathAndContent(args json.RawMessage) (string, string) {
	var m struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &m); err != nil {
		return "", ""
	}
	return m.Path, m.Content
}

func (a *HITLApprovalAdapter) AllowTool(toolName string, args json.RawMessage) (bool, string) {
	decision, reason, feedback := a.DecideTool(toolName, args, "")
	if decision == tool.ApprovalModified {
		return false, feedback
	}
	return decision == tool.ApprovalAllowed, reason
}

func (a *HITLApprovalAdapter) AllowToolWithPreview(toolName string, args json.RawMessage, preview string) (bool, string) {
	decision, reason, feedback := a.DecideTool(toolName, args, preview)
	if decision == tool.ApprovalModified {
		return false, feedback
	}
	return decision == tool.ApprovalAllowed, reason
}

func isFileMutation(toolName string) bool {
	switch toolName {
	case "write_file", "edit_file", "insert_file", "delete_file":
		return true
	}
	return false
}

func (a *HITLApprovalAdapter) DecideTool(toolName string, args json.RawMessage, preview string) (tool.ApprovalDecision, string, string) {
	needsReview, riskLevel, reason := a.mgr.NeedsReview(toolName, string(args))

	// For file mutations with diff preview, do chunk-by-chunk review
	// only when not in auto-approve mode and diff preview is enabled.
	if preview != "" && a.mgr.IsEnabled() && isFileMutation(toolName) &&
		a.mgr.Mode() != HITLModeAutoApprove && security.ShouldPreviewDiff() {
		path, newContent := extractPathAndContent(args)
		oldContent := a.readOldContent(path)
		a.mu.Lock()
		a.lastAcceptedContent = ""
		a.mu.Unlock()

		accepted, ok := security.PreviewAndConfirm(path, oldContent, newContent, preview)
		if !ok {
			return tool.ApprovalRejected, "changes rejected by operator", ""
		}
		a.mu.Lock()
		a.lastAcceptedContent = accepted
		a.mu.Unlock()
		return tool.ApprovalAllowed, "", ""
	}
	if !needsReview {
		return tool.ApprovalAllowed, "", ""
	}
	if preview != "" {
		reason += "\n\nProposed mutation:\n" + preview
	}
	resp := a.mgr.RequestApproval(HITLRequest{ToolName: toolName, Arguments: string(args), RiskLevel: riskLevel, Reason: reason})
	switch resp.Decision {
	case HITLApprove:
		return tool.ApprovalAllowed, "", ""
	case HITLReject:
		return tool.ApprovalRejected, fmt.Sprintf("HITL rejected %s: %s", toolName, reason), ""
	case HITLModify:
		return tool.ApprovalModified, "", resp.Feedback
	default:
		return tool.ApprovalRejected, "unknown HITL decision", ""
	}
}
