package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-code-agent/internal/gateway"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/tool"
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

func TestRunnerReturnsWithoutInjectedTurnContext(t *testing.T) {
	provider := &fakeProvider{name: "fake", content: "done", finishReason: "stop"}
	runner := NewRunner(
		NewLeadProfile("sys"),
		gateway.NewGateway(provider, gateway.NewRoleThrottle(2)),
		tool.NewExecutor(tool.NewToolCatalog(), nil, nil),
		nil,
		nil,
	)
	runner.SetDynamicContext(func() string {
		return BuildSessionContext("evergreen", "todo", "")
	})
	runner.SetMemoryRecall(func(string) string { return "remember this" })

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("do the thing")}, "context-cleanup")
	if outcome.Error != nil {
		t.Fatalf("Run: %v", outcome.Error)
	}
	if provider.lastParams == nil || len(provider.lastParams.Messages) != 3 {
		t.Fatalf("provider did not receive both context snapshots: %#v", provider.lastParams)
	}
	if !strings.HasPrefix(provider.lastParams.Messages[2].Content, "<memory-recall>") {
		t.Fatalf("memory snapshot is not reserved context: %q", provider.lastParams.Messages[2].Content)
	}
	if len(outcome.Messages) != 2 || outcome.Messages[0].Content != "do the thing" || outcome.Messages[1].Content != "done" {
		t.Fatalf("returned messages retained injected context: %#v", outcome.Messages)
	}
}

func TestRunnerCancellationRemovesInjectedTurnContext(t *testing.T) {
	runner := NewRunner(NewLeadProfile("sys"), nil, nil, nil, nil)
	runner.SetDynamicContext(func() string { return BuildSessionContext("evergreen", "", "") })
	runner.SetMemoryRecall(func(string) string { return "remember this" })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	outcome := runner.Run(ctx, []llm.Message{llm.UserMessage("do the thing")}, "context-cancel")
	if !errors.Is(outcome.Error, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", outcome.Error)
	}
	if len(outcome.Messages) != 1 || outcome.Messages[0].Content != "do the thing" {
		t.Fatalf("cancelled outcome retained injected context: %#v", outcome.Messages)
	}
}
