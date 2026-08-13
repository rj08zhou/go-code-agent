package hitl

import (
	"fmt"

	"go-code-agent/internal/security"
)

// ParseMode converts the CLI spelling to the manager mode.
func ParseMode(value string) (HITLMode, error) {
	switch value {
	case "interactive":
		return HITLModeInteractive, nil
	case "auto-approve", "approve":
		return HITLModeAutoApprove, nil
	case "auto-reject", "reject":
		return HITLModeAutoReject, nil
	case "notify-only", "notify":
		return HITLModeNotifyOnly, nil
	case "safe-auto", "safeauto", "safe-only", "safeonly":
		// safe-only / safeonly remain accepted CLI spellings for compatibility.
		return HITLModeSafeAuto, nil
	default:
		return HITLModeInteractive, fmt.Errorf("unknown HITL mode %q", value)
	}
}

// ApplyMode enables HITL, sets its mode, and syncs ApprovalState's
// auto-approve / ShouldShowDiffUI posture in one place. Callers (CLI startup,
// REPL /approval) must go through this instead of setting HITLManager and
// ApprovalState separately, so the two can never drift out of sync.
func ApplyMode(mgr *HITLManager, approval *security.ApprovalState, mode HITLMode) {
	mgr.SetEnabled(true)
	mgr.SetMode(mode)
	if approval == nil {
		return
	}
	approval.ApplyPreset(presetForMode(mode))
}

// presetForMode maps a HITLMode to the ApprovalState preset name. Interactive,
// AutoReject, and NotifyOnly all require manual review for anything beyond
// safe/auto tools, so they share the "manual" (no auto-approve) posture.
func presetForMode(mode HITLMode) string {
	switch mode {
	case HITLModeAutoApprove:
		return "all-auto"
	case HITLModeSafeAuto:
		return "safe-auto"
	default: // HITLModeInteractive, HITLModeAutoReject, HITLModeNotifyOnly
		return "manual"
	}
}
