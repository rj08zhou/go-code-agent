package agent

import (
	"context"
	"encoding/json"
	"testing"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/tool"
)

func TestRunner_BlocksRepeatedIdenticalToolCalls(t *testing.T) {
	fake := &fakeProvider{name: "fake", content: "continue"}
	gateway := model.NewGateway(fake, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{{
		Name:    "noop",
		Effects: tool.Effects(),
		Handler: func(*tool.ToolScope, json.RawMessage) tool.Result { return tool.Succeeded("ok") },
	}})

	runner := NewRunner(NewExploreProfile(), gateway, tool.NewExecutor(catalog, nil, nil), &tool.ToolScope{
		Role:       "explore",
		CanRead:    true,
		CanExecute: true,
	})
	fake.toolCalls = []llm.ToolCall{{ID: "call_1", Name: "noop", Arguments: `{}`}}

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("inspect")}, "repeat-test")
	foundBlocked := false
	for _, result := range outcome.ToolResults {
		if result.Status == tool.StatusFailed && result.Name == "noop" {
			foundBlocked = true
			break
		}
	}
	if !foundBlocked {
		t.Fatal("expected repeated identical tool call to be blocked")
	}
}
