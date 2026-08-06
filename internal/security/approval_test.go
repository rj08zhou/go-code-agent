package security

import "testing"

func TestApprovePresetClearsDangerPreviewSkip(t *testing.T) {
	s := NewApprovalState()

	s.ApplyPreset("danger")
	if s.ShouldShowDiffUI() {
		t.Fatal("danger preset should skip diff preview")
	}
	s.ApplyPreset("safe")
	if !s.ShouldShowDiffUI() {
		t.Fatal("safe preset must re-enable diff preview")
	}
	s.ApplyPreset("danger")
	s.ApplyPreset("off")
	if !s.ShouldShowDiffUI() {
		t.Fatal("off preset must re-enable diff preview")
	}
	if s.IsAutoApproveAll() || s.IsAutoApproveSafe() {
		t.Fatal("off preset must clear both auto-approve flags")
	}
}
