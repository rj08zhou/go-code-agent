package hitlaudit

import (
	"testing"

	"go-code-agent/internal/security"
)

func TestApplyModeSyncsApprovalPreset(t *testing.T) {
	tests := []struct {
		mode            HITLMode
		wantAutoAll     bool
		wantAutoSafe    bool
		wantPreviewDiff bool
	}{
		{HITLModeInteractive, false, false, true},
		{HITLModeSafeOnly, false, true, true},
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
		if got := approval.ShouldPreviewDiff(); got != tc.wantPreviewDiff {
			t.Errorf("mode %v: ShouldPreviewDiff() = %v, want %v", tc.mode, got, tc.wantPreviewDiff)
		}
	}
}

func TestApplyModeToleratesNilApproval(t *testing.T) {
	mgr := NewHITLManager(nil)
	ApplyMode(mgr, nil, HITLModeSafeOnly)
	if !mgr.IsEnabled() || mgr.Mode() != HITLModeSafeOnly {
		t.Fatalf("ApplyMode with nil approval should still set manager state")
	}
}
