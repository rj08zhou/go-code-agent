package hitlaudit

import "fmt"

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
	case "safe-only", "safeonly":
		return HITLModeSafeOnly, nil
	default:
		return HITLModeInteractive, fmt.Errorf("unknown HITL mode %q", value)
	}
}
