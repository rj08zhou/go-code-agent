package agent

import (
	"strings"
	"testing"

	"go-code-agent/internal/prompt"
	"go-code-agent/internal/session"
)

// setupReflectApp wires a minimal global `App` backed by a temp-dir
// session so reflect()'s App.DagSched()/App.PromptLoader references are
// non-nil. Returns the active session for tests that need to seed tasks.
func setupReflectApp(t *testing.T) *session.Session {
	t.Helper()
	dir := t.TempDir()
	pl := prompt.NewLoader("") // empty dir -> LoadOr falls back to defaults
	bv := func(string) (bool, bool, string) { return true, false, "" }
	sm := session.NewSessionManager(dir, "test-model", pl, nil, bv)
	s, err := sm.NewSession("reflect-test")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sm.Activate(s)
	App = &AppContext{PromptLoader: pl, SessionManager: sm}
	return s
}

// contains reports whether any prompt in the slice has substr.
func containsPrompt(prompts []string, substr string) bool {
	for _, p := range prompts {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// hasKind reports whether kind is in the triggered slice.
func hasKind(triggered []string, kind string) bool {
	for _, k := range triggered {
		if k == kind {
			return true
		}
	}
	return false
}

// TestReflectMiniFiresOnFirstFailure: a single consecutive failure emits
// a mini-reflect and is not subject to any cool-down (it is naturally
// one-shot, gated by the "first failure" condition).
func TestReflectMiniFiresOnFirstFailure(t *testing.T) {
	setupReflectApp(t)
	last := map[string]int{}

	prompts, _, _, _, triggered := reflect(
		1, "read_file", 3,
		1, 1,
		0, 0,
		8, 0, false,
		last,
	)
	if !containsPrompt(prompts, "mini-reflect") {
		t.Fatalf("expected mini-reflect prompt, got %v", prompts)
	}
	if !hasKind(triggered, reflectKindMini) {
		t.Fatalf("expected %q in triggered, got %v", reflectKindMini, triggered)
	}
}

// TestReflectStrategyChangeCooldown verifies the strategy-change prompt
// is suppressed within its cool-down window but the failure counter is
// still cleared every round so the loop can recover.
func TestReflectStrategyChangeCooldown(t *testing.T) {
	setupReflectApp(t)
	last := map[string]int{}

	// Round 10: first strategy-change. Fires + records.
	prompts, resetFailures, _, _, triggered := reflect(
		3, "bash", 3,
		10, 5,
		0, 0,
		8, 0, false,
		last,
	)
	if !containsPrompt(prompts, "strategy-change") {
		t.Fatalf("round 10: expected strategy-change prompt, got %v", prompts)
	}
	if !resetFailures {
		t.Fatal("round 10: expected resetFailures=true")
	}
	if !hasKind(triggered, reflectKindStrategy) {
		t.Fatalf("round 10: expected %q triggered, got %v", reflectKindStrategy, triggered)
	}
	last[reflectKindStrategy] = 10

	// Round 12: within cool-down (12-10=2 < 5). Suppressed prompt, but
	// resetFailures must still be true.
	prompts, resetFailures, _, _, triggered = reflect(
		3, "bash", 3,
		12, 6,
		0, 0,
		8, 0, false,
		last,
	)
	if containsPrompt(prompts, "strategy-change") {
		t.Fatalf("round 12: strategy-change should be on cool-down, got %v", prompts)
	}
	if hasKind(triggered, reflectKindStrategy) {
		t.Fatalf("round 12: strategy must not be triggered during cool-down, got %v", triggered)
	}
	if !resetFailures {
		t.Fatal("round 12: resetFailures must still be true during cool-down")
	}

	// Round 15: cool-down elapsed (15-10=5, not < 5). Fires again.
	prompts, _, _, _, triggered = reflect(
		3, "bash", 3,
		15, 7,
		0, 0,
		8, 0, false,
		last,
	)
	if !containsPrompt(prompts, "strategy-change") {
		t.Fatalf("round 15: expected strategy-change to re-fire, got %v", prompts)
	}
	if !hasKind(triggered, reflectKindStrategy) {
		t.Fatalf("round 15: expected %q triggered again, got %v", reflectKindStrategy, triggered)
	}
}

// TestReflectStuckCooldown verifies stuck detection fires once, is
// suppressed during cool-down (while still resetting the counter), and
// re-fires after the window.
func TestReflectStuckCooldown(t *testing.T) {
	s := setupReflectApp(t)
	// Seed a task so DagSched().TaskCount() > 0 (stuck precondition).
	s.TaskMgr.Create("dummy", "for stuck test", nil)

	last := map[string]int{}

	// Round 20: first stuck.
	prompts, _, _, resetStuck, triggered := reflect(
		0, "", 3,
		20, 0,
		8, 0,
		8, 0, false,
		last,
	)
	if !containsPrompt(prompts, "<stuck>") {
		t.Fatalf("round 20: expected stuck prompt, got %v", prompts)
	}
	if !resetStuck {
		t.Fatal("round 20: expected resetStuck=true")
	}
	if !hasKind(triggered, reflectKindStuck) {
		t.Fatalf("round 20: expected %q triggered, got %v", reflectKindStuck, triggered)
	}
	last[reflectKindStuck] = 20

	// Round 22: within cool-down (22-20=2 < 5). Suppressed but still resets.
	prompts, _, _, resetStuck, triggered = reflect(
		0, "", 3,
		22, 0,
		8, 0,
		8, 0, false,
		last,
	)
	if containsPrompt(prompts, "<stuck>") {
		t.Fatalf("round 22: stuck should be on cool-down, got %v", prompts)
	}
	if !resetStuck {
		t.Fatal("round 22: resetStuck must still be true during cool-down")
	}
	if hasKind(triggered, reflectKindStuck) {
		t.Fatalf("round 22: stuck must not be triggered during cool-down, got %v", triggered)
	}

	// Round 25: cool-down elapsed (25-20=5, not < 5). Re-fires.
	prompts, _, _, _, triggered = reflect(
		0, "", 3,
		25, 0,
		8, 0,
		8, 0, false,
		last,
	)
	if !containsPrompt(prompts, "<stuck>") {
		t.Fatalf("round 25: expected stuck to re-fire, got %v", prompts)
	}
	if !hasKind(triggered, reflectKindStuck) {
		t.Fatalf("round 25: expected %q triggered again, got %v", reflectKindStuck, triggered)
	}
}

// TestReflectPeriodicNoCooldown verifies periodic reflection fires on
// every interval boundary (periodicCooldown is 0 — governed by the
// interval itself).
func TestReflectPeriodicNoCooldown(t *testing.T) {
	setupReflectApp(t)
	last := map[string]int{}

	for _, round := range []int{5, 10} {
		prompts, _, _, _, triggered := reflect(
			0, "", 3,
			round, 0,
			0, 0,
			8, 5, false,
			last,
		)
		if !containsPrompt(prompts, "<reflect>") {
			t.Fatalf("round %d: expected periodic reflect, got %v", round, prompts)
		}
		if !hasKind(triggered, reflectKindPeriodic) {
			t.Fatalf("round %d: expected %q triggered, got %v", round, reflectKindPeriodic, triggered)
		}
		last[reflectKindPeriodic] = round
	}

	// Off a boundary (round 7) -> no periodic reflect.
	prompts, _, _, _, triggered := reflect(
		0, "", 3,
		7, 0,
		0, 0,
		8, 5, false,
		last,
	)
	if containsPrompt(prompts, "<reflect>") {
		t.Fatalf("round 7: periodic should not fire off-boundary, got %v", prompts)
	}
	if hasKind(triggered, reflectKindPeriodic) {
		t.Fatalf("round 7: periodic must not be triggered off-boundary, got %v", triggered)
	}
}

// TestReflectTodoNagCooldown verifies the stand-alone todo-nag (when no
// periodic reflection is due) honors its cool-down window.
func TestReflectTodoNagCooldown(t *testing.T) {
	setupReflectApp(t)
	last := map[string]int{}

	// reflectInterval=0 disables periodic reflect, isolating the nag branch.
	// Round 3: first nag.
	prompts, _, resetTodoNag, _, triggered := reflect(
		0, "", 3,
		3, 0,
		0, 3,
		8, 0, true,
		last,
	)
	if len(prompts) == 0 {
		t.Fatal("round 3: expected a todo-nag prompt")
	}
	if !resetTodoNag {
		t.Fatal("round 3: expected resetTodoNag=true")
	}
	if !hasKind(triggered, reflectKindTodoNag) {
		t.Fatalf("round 3: expected %q triggered, got %v", reflectKindTodoNag, triggered)
	}
	last[reflectKindTodoNag] = 3

	// Round 5: within cool-down (5-3=2 < 3). Suppressed.
	prompts, _, resetTodoNag, _, triggered = reflect(
		0, "", 3,
		5, 0,
		0, 4,
		8, 0, true,
		last,
	)
	if len(prompts) != 0 {
		t.Fatalf("round 5: todo-nag should be on cool-down, got %v", prompts)
	}
	if resetTodoNag {
		t.Fatal("round 5: resetTodoNag must be false during cool-down")
	}
	if hasKind(triggered, reflectKindTodoNag) {
		t.Fatalf("round 5: todo-nag must not be triggered during cool-down, got %v", triggered)
	}

	// Round 6: cool-down elapsed (6-3=3, not < 3). Re-fires.
	prompts, _, _, _, triggered = reflect(
		0, "", 3,
		6, 0,
		0, 5,
		8, 0, true,
		last,
	)
	if len(prompts) == 0 {
		t.Fatal("round 6: expected todo-nag to re-fire")
	}
	if !hasKind(triggered, reflectKindTodoNag) {
		t.Fatalf("round 6: expected %q triggered again, got %v", reflectKindTodoNag, triggered)
	}
}
