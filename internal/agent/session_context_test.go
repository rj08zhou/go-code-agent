package agent

import (
	"strings"
	"testing"

	"go-code-agent/internal/llm"
)

func TestBuildSessionContext(t *testing.T) {
	got := BuildSessionContext("remember X", "todo: 1", "server-a")
	for _, want := range []string{
		"<session-context>",
		"## Evergreen memory",
		"remember X",
		"## Open tasks",
		"todo: 1",
		"## MCP",
		"server-a",
		"</session-context>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestBuildSessionContextSkipsEmptyAndDefaultMCP(t *testing.T) {
	if got := BuildSessionContext("", "", "No MCP servers configured."); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
	if got := BuildSessionContext("  ", "\n", ""); got != "" {
		t.Fatalf("want empty for whitespace, got %q", got)
	}
	got := BuildSessionContext("m", "", "")
	if !strings.Contains(got, "Evergreen") || strings.Contains(got, "## MCP") {
		t.Fatalf("unexpected:\n%s", got)
	}
}

func TestSessionContextIsEphemeralNudge(t *testing.T) {
	m := llm.UserMessage(BuildSessionContext("x", "", ""))
	if !isEphemeralNudge(m) {
		t.Fatal("session-context should be droppable after the model responds")
	}
}

func TestInjectTurnContextAddsSessionBlock(t *testing.T) {
	r := NewRunner(NewLeadProfile("sys"), nil, nil, nil, nil)
	r.SetDynamicContext(func() string {
		return BuildSessionContext("ever", "task-a", "")
	})
	msgs := r.injectTurnContext([]llm.Message{llm.UserMessage("do the thing")})
	if len(msgs) != 2 {
		t.Fatalf("len=%d want 2: %+v", len(msgs), msgs)
	}
	if !strings.HasPrefix(msgs[1].Content, "<session-context>") {
		t.Fatalf("expected session-context, got %q", msgs[1].Content)
	}
	if r.turn.originalTask != "do the thing" {
		t.Fatalf("originalTask=%q", r.turn.originalTask)
	}
}
