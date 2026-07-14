package eval

import (
	"context"
	"strings"
	"testing"
)

// TestHarness_MockRunsOffline verifies the whole harness executes with
// the scripted provider and every baseline task passes its Verify. This
// is CI-safe (no network, no API cost).
func TestHarness_MockRunsOffline(t *testing.T) {
	h := New(Tasks, false, "")
	_ = h.Run(context.Background())
	summary := h.Summarize()

	if summary.Total != len(Tasks) {
		t.Fatalf("expected %d tasks, got %d", len(Tasks), summary.Total)
	}
	if summary.Passed != summary.Total {
		var failed []string
		for _, r := range summary.Results {
			if !r.Success {
				failed = append(failed, r.Name+": "+r.Detail)
			}
		}
		t.Fatalf("expected all tasks to pass in mock mode, failures:\n%s", strings.Join(failed, "\n"))
	}
	if summary.SuccessRate != 1.0 {
		t.Fatalf("expected 100%% success, got %.0f%%", summary.SuccessRate*100)
	}
	// Round/tool tallies should be populated.
	for _, r := range summary.Results {
		if r.Rounds < 1 {
			t.Errorf("task %s: rounds not recorded", r.Name)
		}
	}
}

// TestSummaryReport renders without panicking.
func TestSummaryReport(t *testing.T) {
	h := New(Tasks, false, "")
	h.Run(context.Background())
	out := h.Summarize().Report()
	if !strings.Contains(out, "eval baseline") {
		t.Fatalf("report missing header:\n%s", out)
	}
}
