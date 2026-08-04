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
