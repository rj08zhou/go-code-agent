package agent

import (
	"go-code-agent/internal/prompt"
	"testing"
)

func TestReflection_Eval_MiniReflect(t *testing.T) {
	pl := prompt.NewLoader()
	ref := NewReflection(pl)

	prompts, _, _, _, triggered := ref.Eval(
		1, "bash", 3, // consecutiveFailures=1, max=3
		5, 2, // toolRounds=5, totalFailures=2
		0, 0, // roundsSinceComplete=0, roundsWithoutTodo=0
		20, 20, false, // stuckThreshold, reflectInterval, hasOpenItems
		map[string]int{}, // lastTriggered
		0, "",            // taskCount, progressSummary
	)

	// On first failure, should get a mini-reflect
	if len(prompts) == 0 {
		t.Fatal("expected mini-reflect on first failure")
	}
	for _, k := range triggered {
		if k == reflectKindMini {
			return // found
		}
	}
	t.Fatalf("triggered kinds: %v (expected mini)", triggered)
}

func TestReflection_Eval_StrategyChange(t *testing.T) {
	pl := prompt.NewLoader()
	ref := NewReflection(pl)

	prompts, resetFailures, _, _, triggered := ref.Eval(
		3, "bash", 3, // consecutiveFailures=3, max=3
		10, 5, // toolRounds=10
		0, 0, // roundsSinceComplete, roundsWithoutTodo
		20, 20, false, // stuckThreshold, reflectInterval, hasOpenItems
		map[string]int{}, 0, "",
	)

	if !resetFailures {
		t.Fatal("expected resetFailures=true after strategy change")
	}
	foundStrategy := false
	for _, k := range triggered {
		if k == reflectKindStrategy {
			foundStrategy = true
		}
	}
	if !foundStrategy {
		t.Fatalf("expected strategy change trigger, got: %v", triggered)
	}
	_ = prompts
}

func TestReflection_Eval_Stuck(t *testing.T) {
	pl := prompt.NewLoader()
	ref := NewReflection(pl)

	_, _, _, resetStuck, triggered := ref.Eval(
		0, "", 3,
		5, 0,
		25, 0,
		20, 20, false,
		map[string]int{}, 1, "",
	)

	if !resetStuck {
		t.Fatal("expected resetStuck=true")
	}
	foundStuck := false
	for _, k := range triggered {
		if k == reflectKindStuck {
			foundStuck = true
		}
	}
	if !foundStuck {
		t.Fatalf("expected stuck trigger, got: %v", triggered)
	}
}

func TestReflection_Eval_Periodic(t *testing.T) {
	pl := prompt.NewLoader()
	ref := NewReflection(pl)

	prompts, _, _, _, triggered := ref.Eval(
		0, "", 3, // no failures
		20, 0, // toolRounds=20 (multiple of reflectInterval=10)
		0, 0, 20, 10, false, // stuckThreshold=20, reflectInterval=10
		map[string]int{}, 0, "",
	)

	foundPeriodic := false
	for _, k := range triggered {
		if k == reflectKindPeriodic {
			foundPeriodic = true
		}
	}
	if !foundPeriodic {
		t.Fatalf("expected periodic reflection at round 20 (interval=10), got: %v (prompts=%d)", triggered, len(prompts))
	}
}

func TestReflection_Eval_TodoNag(t *testing.T) {
	pl := prompt.NewLoader()
	ref := NewReflection(pl)

	_, _, resetTodoNag, _, triggered := ref.Eval(
		0, "", 3, // no failures
		10, 0, // toolRounds=10
		0, 4, // roundsWithoutTodo=4 (>=3 triggers nag)
		20, 20, true, // hasOpenItems=true
		map[string]int{}, 0, "",
	)

	if !resetTodoNag {
		t.Fatal("expected resetTodoNag=true")
	}
	foundNag := false
	for _, k := range triggered {
		if k == reflectKindTodoNag {
			foundNag = true
		}
	}
	if !foundNag {
		t.Fatalf("expected todo nag trigger, got: %v", triggered)
	}
}

func TestReflection_Eval_BelowThresholdNoTrigger(t *testing.T) {
	pl := prompt.NewLoader()
	ref := NewReflection(pl)

	prompts, _, _, _, _ := ref.Eval(
		2, "bash", 3, // consecutiveFailures=2, below max=3
		5, 1, // toolRounds=5, totalFailures=1
		1, 1, // roundsSinceComplete=1, roundsWithoutTodo=1
		20, 20, false, // not stuck, no todo issues
		map[string]int{}, 0, "",
	)

	if len(prompts) > 0 {
		t.Fatalf("expected no triggers below threshold, got %d prompts", len(prompts))
	}
}
