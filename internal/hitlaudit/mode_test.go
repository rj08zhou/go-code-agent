package hitlaudit

import (
	"testing"

	"go-code-agent/internal/security"
)

func TestApplyModeSyncsApprovalPreset(t *testing.T) {
	tests := []struct {
		mode           HITLMode
		wantAutoAll    bool
		wantAutoSafe   bool
		wantShowDiffUI bool
	}{
		{HITLModeInteractive, false, false, true},
		{HITLModeSafeAuto, false, true, true},
		{HITLModeAutoApprove, true, true, false},
		{HITLModeAutoReject, false, false, true},
		{HITLModeNotifyOnly, false, false, true},
	}
	for _, tc := range tests {
		mgr := NewHITLManager(nil)
		mgr.SetEnabled(false)
		approval := security.NewApprovalState()

		ApplyMode(mgr, approval, tc.mode)

		if !mgr.IsEnabled() {
			t.Errorf("mode %v: ApplyMode did not enable HITL", tc.mode)
		}
		if got := mgr.Mode(); got != tc.mode {
			t.Errorf("mode %v: HITLManager.Mode() = %v", tc.mode, got)
		}
		if got := approval.IsAutoApproveAll(); got != tc.wantAutoAll {
			t.Errorf("mode %v: IsAutoApproveAll() = %v, want %v", tc.mode, got, tc.wantAutoAll)
		}
		if got := approval.IsAutoApproveSafe(); got != tc.wantAutoSafe {
			t.Errorf("mode %v: IsAutoApproveSafe() = %v, want %v", tc.mode, got, tc.wantAutoSafe)
		}
		if got := approval.ShouldShowDiffUI(); got != tc.wantShowDiffUI {
			t.Errorf("mode %v: ShouldShowDiffUI() = %v, want %v", tc.mode, got, tc.wantShowDiffUI)
		}
	}
}

func TestParseModeSafeAutoAliases(t *testing.T) {
	for _, spelling := range []string{"safe-auto", "safeauto", "safe-only", "safeonly"} {
		mode, err := ParseMode(spelling)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", spelling, err)
		}
		if mode != HITLModeSafeAuto {
			t.Fatalf("ParseMode(%q) = %v, want HITLModeSafeAuto", spelling, mode)
		}
	}
	if got := HITLModeSafeAuto.String(); got != "safe-auto" {
		t.Fatalf("String() = %q, want safe-auto", got)
	}
}
