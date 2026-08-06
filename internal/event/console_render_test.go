package event

import (
	"strings"
	"testing"
)

func TestRenderConsoleEventFormatsToolSummaryWithoutIO(t *testing.T) {
	rendered := renderConsoleEvent(Event{
		Type:     ToolFinished,
		ToolName: "search_content",
		Status:   "succeeded",
		Output:   "one\ntwo",
		Payload: map[string]string{
			"path":    "internal/event",
			"pattern": "ConsoleSink",
		},
	})

	for _, want := range []string{
		"[search_content]", "succeeded", "path=internal/event",
		`pattern="ConsoleSink"`, "matches=2", "one", "two",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered event missing %q: %q", want, rendered)
		}
	}
}

func TestRenderConsoleEventFormatsTurnDecision(t *testing.T) {
	rendered := renderConsoleEvent(Event{
		Type: TurnDecision,
		Payload: map[string]string{
			"round": "3", "tool_calls": "2", "failures": "1",
		},
	})
	if !strings.Contains(rendered, "--- turn 3 ---") ||
		!strings.Contains(rendered, "tools=2 failures=1") {
		t.Fatalf("turn decision summary = %q", rendered)
	}
}

func TestRenderConsoleEventFormatsPlanningDecisionAsPolicy(t *testing.T) {
	nudge := renderConsoleEvent(Event{
		Type: PlanningDecision,
		Payload: map[string]string{
			"action": "require_plan",
		},
	})
	if !strings.Contains(nudge, "[policy]") || !strings.Contains(nudge, "plan required") {
		t.Fatalf("nudge = %q", nudge)
	}
	if strings.Contains(nudge, "[planning]") {
		t.Fatalf("nudge still uses [planning]: %q", nudge)
	}

	blocked := renderConsoleEvent(Event{
		Type:     PlanningDecision,
		ToolName: "bash",
		Status:   "blocked",
		Error:    `tool "bash" blocked: ...`,
		Payload: map[string]string{
			"action":         "block_unplanned_side_effect",
			"classification": "process_execution",
		},
	})
	for _, want := range []string{
		"[policy]", "blocked unplanned side effect", "bash", "process_execution", "status=blocked",
	} {
		if !strings.Contains(blocked, want) {
			t.Fatalf("blocked missing %q: %q", want, blocked)
		}
	}
	if strings.Contains(blocked, "[bash]") {
		t.Fatalf("blocked should use [policy], not [bash]: %q", blocked)
	}
}
