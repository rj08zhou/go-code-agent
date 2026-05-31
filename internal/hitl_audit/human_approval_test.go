package hitl_audit

import (
	"encoding/json"
	"testing"
)

// bashArgs builds the JSON envelope a real bash tool call would carry.
func bashArgs(cmd string) string {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return string(b)
}

// TestNeedsReview_ReadOnlyShellCommandsBypass verifies that the read-only
// commands seen in real audit logs (ls, grep, sed -n, find, go test, and
// "cd X && go test" compounds) do NOT trigger HITL when the manager is on.
func TestNeedsReview_ReadOnlyShellCommandsBypass(t *testing.T) {
	h := NewHITLManager()
	h.SetEnabled(true)

	cases := []string{
		"ls -la internal/memory/",
		"cd internal/memory && go test -v -run TestTokenize",
		`grep -n "evergreenChars\|dailyFiles" internal/memory/memory_test.go`,
		"cd internal/memory && go test -v 2>&1 | head -100",
		"sed -n '620,635p' internal/memory/memory_test.go",
		`grep -n "MaxMemoryContentLen" internal/memory/memory.go`,
		`find . -name "infra" -type d 2>/dev/null | head -5`,
		`grep "import" internal/memory/memory.go`,
		"cd internal/memory && go test -v 2>&1",
		"pwd",
		"GOFLAGS=-count=1 go test ./...",
	}
	for _, c := range cases {
		need, _, reason := h.NeedsReview("bash", bashArgs(c))
		if need {
			t.Errorf("expected NO review for %q, got reason=%q", c, reason)
		}
	}
}

// TestNeedsReview_DangerousCommandsAlwaysReviewed locks in the high-risk path
// so future allow-list tweaks cannot accidentally let destructive commands
// through.
func TestNeedsReview_DangerousCommandsAlwaysReviewed(t *testing.T) {
	h := NewHITLManager()
	h.SetEnabled(true)

	dangerous := []string{
		"rm -rf /tmp/whatever",
		"git push --force origin main",
		"git reset --hard HEAD~5",
		"kubectl delete pod foo",
		"terraform destroy",
		// Even when prefixed by a safe cd, the dangerous segment must win.
		"cd /tmp && rm -rf build",
	}
	for _, c := range dangerous {
		need, risk, _ := h.NeedsReview("bash", bashArgs(c))
		if !need || risk != "high" {
			t.Errorf("expected high-risk review for %q, got need=%v risk=%q", c, need, risk)
		}
	}
}

// TestNeedsReview_UnknownShellCommandsEscalate ensures we don't silently
// approve commands that are neither obviously dangerous nor on the safe list.
func TestNeedsReview_UnknownShellCommandsEscalate(t *testing.T) {
	h := NewHITLManager()
	h.SetEnabled(true)

	cases := []string{
		"curl https://example.com -o /tmp/x",
		"npm install some-package",
		"sed -i 's/foo/bar/' file.go",     // sed without -n must NOT bypass
		"cd /tmp && curl https://x.com/y", // safe + unsafe compound
	}
	for _, c := range cases {
		need, risk, _ := h.NeedsReview("bash", bashArgs(c))
		if !need {
			t.Errorf("expected review for %q, got need=false", c)
		}
		if risk == "" {
			t.Errorf("expected non-empty risk for %q", c)
		}
	}
}

// TestSplitShellPipeline covers the compound-command splitter for the cases
// most relevant to allow-list classification.
func TestSplitShellPipeline(t *testing.T) {
	got := splitShellPipeline("cd a && go test -v 2>&1 | head -10")
	want := []string{"cd a", "go test -v 2>&1", "head -10"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("part %d: got %q want %q", i, got[i], want[i])
		}
	}

	// Quoted operator must NOT split.
	got = splitShellPipeline(`echo "a && b"`)
	if len(got) != 1 || got[0] != `echo "a && b"` {
		t.Errorf("quoted operator was split: %v", got)
	}
}

// TestIsEnvAssignment guards the leading-env-var trimming.
func TestIsEnvAssignment(t *testing.T) {
	yes := []string{"FOO=bar", "GOFLAGS=-count=1", "_X=1", "A1=2"}
	no := []string{"=bar", "1FOO=bar", "foo", "--flag=1", "go"}
	for _, s := range yes {
		if !isEnvAssignment(s) {
			t.Errorf("expected %q to be env assignment", s)
		}
	}
	for _, s := range no {
		if isEnvAssignment(s) {
			t.Errorf("expected %q NOT to be env assignment", s)
		}
	}
}

// TestNeedsReview_DisabledIsAlwaysFalse keeps the opt-in invariant: when HITL
// is off, no command — safe or dangerous — should be reported as needing
// review.
func TestNeedsReview_DisabledIsAlwaysFalse(t *testing.T) {
	h := NewHITLManager()
	// enabled=false by default
	need, _, _ := h.NeedsReview("bash", bashArgs("rm -rf /"))
	if need {
		t.Errorf("disabled manager must never request review")
	}
}
