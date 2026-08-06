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
	HITLModeSafeAuto
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
	case HITLModeSafeAuto:
		return "safe-auto"
	default:
		return "unknown"
	}
}

// ReviewSeverity is the HITL review risk class used by SafeAuto and console
// labels. high and danger both require a prompt under SafeAuto; medium is for
// ordinary file mutations routed to the general panel in auto-reject/notify.
type ReviewSeverity string

const (
	SeverityNone   ReviewSeverity = ""
	SeverityLow    ReviewSeverity = "low"
	SeverityMedium ReviewSeverity = "medium"
	SeverityHigh   ReviewSeverity = "high"
	SeverityDanger ReviewSeverity = "danger"
)

func (s ReviewSeverity) String() string { return string(s) }

// RequiresPromptInSafeAuto reports whether SafeAuto must open the interactive
// panel instead of auto-approving.
func (s ReviewSeverity) RequiresPromptInSafeAuto() bool {
	return s == SeverityHigh || s == SeverityDanger
}

type HITLRequest struct {
	ToolName  string
	Arguments string
	Severity  ReviewSeverity
	Reason    string
	SessionID string
}

type HITLManager struct {
	enabled        bool
	mode           HITLMode
	nonTTYFallback HITLDecision
	// alwaysReviewTools always need HITL when enabled (independent of args).
	alwaysReviewTools      map[string]bool
	criticalPathSubstrings []string
	console                security.InteractiveIO
	mu                     sync.RWMutex
	promptLoader           *prompt.Loader
}

func NewHITLManager(pl *prompt.Loader, consoles ...security.InteractiveIO) *HITLManager {
	fallback := HITLReject
	if os.Getenv("HITL_NON_TTY_FALLBACK") == "approve" {
		fallback = HITLApprove
	}
	console := security.DefaultInteractiveIO()
	if len(consoles) > 0 && consoles[0] != nil {
		console = consoles[0]
	}
	return &HITLManager{
		enabled:        false,
		mode:           HITLModeInteractive,
		nonTTYFallback: fallback,
		console:        console,
		promptLoader:   pl,
		alwaysReviewTools: map[string]bool{
			"delete_file": true,
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

func (h *HITLManager) NeedsReview(toolName, arguments string) (bool, ReviewSeverity, string) {
	if !h.IsEnabled() {
		return false, SeverityNone, ""
	}
	r := h.classifyReview(toolName, arguments)
	return r.needsReview, r.severity, r.reason
}

// classifyReview evaluates intrinsic review requirements without re-reading
// the enabled flag, so callers that already froze a policy snapshot stay
// consistent for the rest of DecideTool.
func (h *HITLManager) classifyReview(toolName, arguments string) reviewRequirement {
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Shell tools are classified per-command; IsShellTool is the sole gate
	// (no separate commandClassifiedTools map — bash/execute_command/background_run).
	if security.IsShellTool(toolName) {
		// Risk classification is delegated to the single source of truth in
		// the security package; HITL only maps verdicts to review decisions.
		// An unparseable or absent command classifies as VerdictDeny, so this
		// path fails closed. Every branch sets commandClassified so the static
		// tool RiskDanger cannot overwrite this per-call judgement.
		//
		// Caution maps to SeverityHigh (not Medium) so SafeAuto still prompts —
		// that mode only blocks on high/danger. The labels high and danger
		// are otherwise equivalent for RequestApproval behaviour.
		c := security.ClassifyCommand(extractBashCommand(arguments))
		switch c.Verdict {
		case security.VerdictDeny, security.VerdictDanger:
			return reviewRequirement{
				needsReview: true, severity: SeverityHigh, reason: c.Reason,
				commandClassified: true,
			}
		case security.VerdictSafe:
			return reviewRequirement{
				severity: SeverityLow, reason: c.Reason, commandClassified: true,
			}
		default: // VerdictCaution
			return reviewRequirement{
				needsReview:       true,
				severity:          SeverityHigh,
				reason:            fmt.Sprintf("shell execution via '%s' requires review: %s", toolName, c.Reason),
				commandClassified: true,
			}
		}
	}

	if h.alwaysReviewTools[toolName] {
		return reviewRequirement{
			needsReview: true,
			severity:    SeverityHigh,
			reason:      fmt.Sprintf("tool '%s' is always reviewed", toolName),
		}
	}

	if toolName == "write_file" || toolName == "edit_file" || toolName == "delete_file" {
		if p := extractPathArg(arguments); p != "" {
			if sub := h.matchCriticalPath(p); sub != "" {
				return reviewRequirement{
					needsReview: true,
					severity:    SeverityHigh,
					reason:      fmt.Sprintf("target path '%s' matches critical substring '%s'", p, sub),
				}
			}
		}
	}
	return reviewRequirement{}
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
		h.console.WriteInteractive("[hitl] auto-approved\n")
		return HITLResponse{Decision: HITLApprove}
	case HITLModeSafeAuto:
		// Auto-approve safe requests; the interactive panel fully renders risky ones.
		if req.Severity.RequiresPromptInSafeAuto() {
			return h.promptInteractive(req)
		}
		h.printReviewHeader(req)
		h.console.WriteInteractive("[hitl] auto-approved (safe)\n")
		return HITLResponse{Decision: HITLApprove}
	case HITLModeAutoReject:
		h.printReviewHeader(req)
		h.console.WriteInteractive(fmt.Sprintf("[hitl] auto-rejected (%s)\n", req.Severity))
		return HITLResponse{Decision: HITLReject}
	case HITLModeNotifyOnly:
		h.printReviewHeader(req)
		h.console.WriteInteractive("[hitl] proceeding (notify-only)\n")
		return HITLResponse{Decision: HITLApprove}
	default:
		return h.promptInteractive(req)
	}
}

func (h *HITLManager) printReviewHeader(req HITLRequest) {
	h.console.WriteInteractive("\n")
	h.console.WriteInteractive(fmt.Sprintf("[hitl] reviewing %s", req.ToolName))
	if req.Severity != SeverityNone {
		h.console.WriteInteractive(fmt.Sprintf(" [%s]", req.Severity))
	}
	h.console.WriteInteractive("\n")
	if req.Reason != "" {
		// Reason may already include rendered diff text appended by DecideTool.
		h.console.WriteInteractive(req.Reason + "\n")
	}
}

func (h *HITLManager) promptInteractive(req HITLRequest) HITLResponse {
	if !h.console.IsTTY() {
		if h.nonTTYFallback == HITLApprove {
			h.console.WriteInteractive(fmt.Sprintf("[hitl] no tty, auto-approving %s (fallback=approve)\n", req.ToolName))
			return HITLResponse{Decision: HITLApprove}
		}
		h.console.WriteInteractive(fmt.Sprintf("[hitl] no tty, auto-rejecting %s (set HITL_NON_TTY_FALLBACK=approve to change)\n", req.ToolName))
		return HITLResponse{Decision: HITLReject}
	}
	divider := strings.Repeat("=", 60)
	h.console.WriteInteractive("\n")
	h.console.WriteInteractive(utils.Bold + divider + "\n")
	h.console.WriteInteractive("HUMAN APPROVAL REQUIRED\n")
	h.console.WriteInteractive(divider + utils.Reset + "\n")
	h.console.WriteInteractive(fmt.Sprintf("  Tool       : %s%s%s\n", utils.BoldYellow, req.ToolName, utils.Reset))
	h.console.WriteInteractive(fmt.Sprintf("  Risk level : %s\n", req.Severity))
	if req.Reason != "" {
		h.console.WriteInteractive(fmt.Sprintf("  Reason     : %s\n", req.Reason))
	}
	h.console.WriteInteractive(strings.Repeat("-", 60) + "\n")
	label, details := approvalDetails(req)
	h.console.WriteInteractive(fmt.Sprintf("  %-11s:\n", label))
	h.console.WriteInteractive(indent(details, "    ") + "\n")
	h.console.WriteInteractive(divider + "\n")
	h.console.WriteInteractive("  [y] approve  — run the tool as-is\n")
	h.console.WriteInteractive("  [n] reject   — veto, agent will pick another approach\n")
	h.console.WriteInteractive("  [m] modify   — veto and provide guidance to the agent\n")

	return readInteractiveDecision(h.console)
}

func readInteractiveDecision(consoles ...security.InteractiveIO) HITLResponse {
	console := security.DefaultInteractiveIO()
	if len(consoles) > 0 && consoles[0] != nil {
		console = consoles[0]
	}
	for {
		raw, err := console.ReadLine("Your choice [y/n/m]: ")
		if err != nil {
			console.WriteInteractive("[hitl] input closed; rejected\n")
			return HITLResponse{Decision: HITLReject}
		}
		choice := strings.ToLower(strings.TrimSpace(raw))
		switch choice {
		case "":
			// Blank Enter / empty paste lines should not look like a decision.
			continue
		case "y", "yes", "approve":
			console.WriteInteractive("[hitl] approved\n")
			return HITLResponse{Decision: HITLApprove}
		case "n", "no", "reject":
			console.WriteInteractive("[hitl] rejected\n")
			return HITLResponse{Decision: HITLReject}
		case "m", "modify":
			fb, err := console.ReadLine("Feedback for the agent: ")
			if err != nil {
				console.WriteInteractive("[hitl] feedback input closed; rejected\n")
				return HITLResponse{Decision: HITLReject}
			}
			fb = strings.TrimSpace(fb)
			console.WriteInteractive("[hitl] modified with feedback\n")
			return HITLResponse{Decision: HITLModify, Feedback: fb}
		default:
			console.WriteInteractive("[hitl] enter y (approve), n (reject), or m (modify)\n")
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
