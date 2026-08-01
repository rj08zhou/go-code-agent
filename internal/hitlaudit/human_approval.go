package hitlaudit

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode"

	"go-code-agent/internal/prompt"
	"go-code-agent/internal/security"
	"go-code-agent/internal/utils"
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
		// Risk classification is delegated to the single source of truth in
		// the security package; HITL only maps verdicts to review decisions.
		c := security.ClassifyCommand(extractBashCommand(arguments))
		switch c.Verdict {
		case security.VerdictDeny, security.VerdictDanger:
			return true, "high", c.Reason
		case security.VerdictSafe:
			return false, "low", c.Reason
		default: // VerdictCaution
			return true, "medium", fmt.Sprintf("shell execution via '%s' requires review: %s", toolName, c.Reason)
		}
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
		// Auto-approve safe requests; the interactive panel fully renders risky ones.
		if strings.EqualFold(req.RiskLevel, "danger") || strings.EqualFold(req.RiskLevel, "high") {
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
	label, details := approvalDetails(req)
	fmt.Printf("  %-11s:\n", label)
	fmt.Println(indent(details, "    "))
	fmt.Println(divider)
	fmt.Println("  [y] approve  — run the tool as-is")
	fmt.Println("  [n] reject   — veto, agent will pick another approach")
	fmt.Println("  [m] modify   — veto and provide guidance to the agent")

	return readInteractiveDecision()
}

func readInteractiveDecision() HITLResponse {
	for {
		raw, err := security.ReadLine("Your choice [y/n/m]: ")
		if err != nil {
			fmt.Println("[hitl] input closed; rejected")
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
			fb, err := security.ReadLine("Feedback for the agent: ")
			if err != nil {
				fmt.Println("[hitl] feedback input closed; rejected")
				return HITLResponse{Decision: HITLReject}
			}
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

func approvalDetails(req HITLRequest) (string, string) {
	if req.ToolName == "bash" {
		if command := extractBashCommand(req.Arguments); command != "" {
			command = strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, command)
			return "Command", utils.Truncate(command, 1500)
		}
	}
	return "Arguments", prettyPrintArgs(req.Arguments)
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
	if pl == nil {
		pl = prompt.NewLoader()
	}
	return prompt.Render(pl.MustLoad("human_reject"), map[string]string{
		"tool":   toolName,
		"reason": reason,
	})
}

func FormatModifyMessage(toolName, feedback string, pl *prompt.Loader) string {
	if feedback == "" {
		feedback = "(no additional feedback)"
	}
	if pl == nil {
		pl = prompt.NewLoader()
	}
	return prompt.Render(pl.MustLoad("human_modify"), map[string]string{
		"tool":     toolName,
		"feedback": feedback,
	})
}
