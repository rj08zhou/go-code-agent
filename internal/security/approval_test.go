package security

import "testing"

func TestApprovalStateDecide(t *testing.T) {
	cases := []struct {
		name    string
		preset  string
		level   ApprovalLevel
		allowed bool
	}{
		{"off auto", "off", ApproveAuto, true},
		{"off safe", "off", ApproveSafe, false},
		{"off danger", "off", ApproveDanger, false},
		{"off blocked", "off", ApproveBlocked, false},
		{"safe auto", "safe", ApproveAuto, true},
		{"safe safe", "safe", ApproveSafe, true},
		{"safe danger", "safe", ApproveDanger, false},
		{"danger auto", "danger", ApproveAuto, true},
		{"danger safe", "danger", ApproveSafe, true},
		{"danger danger", "danger", ApproveDanger, true},
		{"danger blocked", "danger", ApproveBlocked, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewApprovalState()
			s.ApplyPreset(tc.preset)
			got, _ := s.Decide(tc.level, "tool")
			if got != tc.allowed {
				t.Fatalf("Decide(%v) under %q = %v, want %v", tc.level, tc.preset, got, tc.allowed)
			}
		})
	}
}

func TestApprovePresetClearsDangerPreviewSkip(t *testing.T) {
	s := NewApprovalState()
	SetActiveApproval(s)
	t.Cleanup(func() { SetActiveApproval(nil) })

	s.ApplyPreset("danger")
	if ShouldPreviewDiff() {
		t.Fatal("danger preset should skip diff preview")
	}
	s.ApplyPreset("safe")
	if !ShouldPreviewDiff() {
		t.Fatal("safe preset must re-enable diff preview")
	}
	s.ApplyPreset("danger")
	s.ApplyPreset("off")
	if !ShouldPreviewDiff() {
		t.Fatal("off preset must re-enable diff preview")
	}
	if s.IsAutoApproveAll() || s.IsAutoApproveSafe() {
		t.Fatal("off preset must clear both auto-approve flags")
	}
}
