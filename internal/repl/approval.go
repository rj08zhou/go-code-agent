package repl

import (
	"fmt"
	"strings"

	"go-code-agent/internal/application"
	"go-code-agent/internal/hitlaudit"
)

// approvalModeToHITL maps the canonical /approval UX spelling to the
// HITLMode it applies. Keep in sync with effectiveApprovalMode's reverse
// mapping below.
var approvalModeToHITL = map[string]hitlaudit.HITLMode{
	"manual":      hitlaudit.HITLModeInteractive,
	"safe-auto":   hitlaudit.HITLModeSafeOnly,
	"all-auto":    hitlaudit.HITLModeAutoApprove,
	"reject":      hitlaudit.HITLModeAutoReject,
	"notify-only": hitlaudit.HITLModeNotifyOnly,
}

func effectiveApprovalMode(b *application.BuiltRunner) string {
	if b == nil || b.Security.HITL == nil {
		return "unavailable"
	}
	if !b.Security.HITL.IsEnabled() {
		return "all-auto (legacy HITL off)"
	}
	switch b.Security.HITL.Mode() {
	case hitlaudit.HITLModeInteractive:
		return "manual"
	case hitlaudit.HITLModeSafeOnly:
		return "safe-auto"
	case hitlaudit.HITLModeAutoApprove:
		return "all-auto"
	case hitlaudit.HITLModeAutoReject:
		return "reject"
	case hitlaudit.HITLModeNotifyOnly:
		return "notify-only"
	default:
		return "unknown"
	}
}

func (r *Loop) handleApproval(parts []string) string {
	if r.built.Security.HITL == nil || r.built.Security.Approval == nil {
		return "Approval controls are unavailable."
	}
	if len(parts) == 1 {
		mode := effectiveApprovalMode(r.built)
		preview := "skipped"
		if (mode == "manual" || mode == "safe-auto") && r.built.Security.Approval.ShouldPreviewDiff() {
			preview = "enabled"
		}
		return fmt.Sprintf("Approval mode: %s\nDiff preview: %s", mode, preview)
	}

	mode := strings.ToLower(parts[1])
	legacy := parts[0] != "/approval"
	switch parts[0] {
	case "/approve":
		switch mode {
		case "off", "reset":
			mode = "manual"
		case "safe":
			mode = "safe-auto"
		case "danger", "all":
			mode = "all-auto"
		default:
			return "Usage: /approve off|safe|danger [confirm] (compatibility alias for /approval)"
		}
	case "/hitl":
		switch mode {
		case "on", "safe-only", "safeonly":
			mode = "safe-auto"
		case "off", "auto-approve", "approve":
			mode = "all-auto"
		case "interactive":
			mode = "manual"
		case "auto-reject":
			mode = "reject"
		case "notify-only", "notify":
			mode = "notify-only"
		default:
			return "Usage: /hitl on|off|interactive|safe-only|auto-approve|auto-reject|notify-only [confirm] (compatibility alias for /approval)"
		}
	}

	if mode == "all-auto" {
		if len(parts) != 3 || strings.ToLower(parts[2]) != "confirm" {
			return "WARNING: all-auto disables approval prompts and skips diff previews.\nHard Bash deny rules and permissions.json remain enforced.\nConfirm with: /approval all-auto confirm"
		}
	} else if len(parts) != 2 {
		return "Usage: /approval manual|safe-auto|all-auto|reject|notify-only"
	}

	hitlMode, ok := approvalModeToHITL[mode]
	if !ok {
		return "Usage: /approval manual|safe-auto|all-auto|reject|notify-only"
	}
	hitlaudit.ApplyMode(r.built.Security.HITL, r.built.Security.Approval, hitlMode)

	prefix := ""
	if legacy {
		canonical := "/approval " + mode
		if mode == "all-auto" {
			canonical += " confirm"
		}
		prefix = fmt.Sprintf("Compatibility alias: use %s.\n", canonical)
	}
	if mode == "all-auto" {
		return prefix + "Approval mode: all-auto — prompts disabled and diff previews skipped; hard deny rules still apply."
	}
	return fmt.Sprintf("%sApproval mode: %s", prefix, mode)
}
