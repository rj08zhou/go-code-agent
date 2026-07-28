package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/tool"
)

func TestIsRepoWalkBash(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{`find . -name "*.go" | head -50`, true},
		{`find /tmp -type f`, true},
		{`ls -laR`, true},
		{`ls -R src`, true},
		{`tree .`, true},
		{`ls -la`, false},
		{`ls -r`, false}, // reverse sort, not recursive
		{`go test ./...`, false},
		{`git status`, false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isRepoWalkBash(tc.cmd); got != tc.want {
			t.Errorf("isRepoWalkBash(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestPostExploreBlock_BudgetAndBash(t *testing.T) {
	r := &Runner{profile: NewLeadProfile("test")}
	r.turn = newTurnState()
	r.turn.explore.Succeeded = true
	r.turn.explore.ReadsAfter = config.MaxReadsAfterExplore

	if blocked, why := r.postExploreBlock(llm.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`}); !blocked {
		t.Fatal("expected read_file blocked after budget exhausted")
	} else if !strings.Contains(why, "post-explore read budget") {
		t.Fatalf("unexpected reason: %s", why)
	}

	r.turn.explore.ReadsAfter = 0
	if blocked, _ := r.postExploreBlock(llm.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`}); blocked {
		t.Fatal("expected read_file allowed within budget")
	}

	if blocked, why := r.postExploreBlock(llm.ToolCall{
		Name: "bash", Arguments: `{"command":"find . -name '*.go'"}`,
	}); !blocked || !strings.Contains(why, "repo walk") {
		t.Fatalf("expected bash find blocked, blocked=%v why=%q", blocked, why)
	}

	if blocked, _ := r.postExploreBlock(llm.ToolCall{
		Name: "bash", Arguments: `{"command":"go test ./internal/agent/"}`,
	}); blocked {
		t.Fatal("expected go test bash allowed after explore")
	}

	if blocked, _ := r.postExploreBlock(llm.ToolCall{
		Name: "search_content", Arguments: `{"query":"Runner"}`,
	}); blocked {
		t.Fatal("search_content must remain unlimited after explore")
	}

	// Explore-role runners are never gated.
	r.profile.Role = "explore"
	r.turn.explore.ReadsAfter = config.MaxReadsAfterExplore
	if blocked, _ := r.postExploreBlock(llm.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`}); blocked {
		t.Fatal("explore role must not hit post-explore budget")
	}
}

func TestRunner_PostExploreReadBudgetAndNudge(t *testing.T) {
	fake := &fakeProvider{
		name:    "fake",
		content: "ok",
		callScript: [][]llm.ToolCall{
			{{ID: "e1", Name: "explore", Arguments: `{"prompt":"survey the repo"}`}},
			{{ID: "r1", Name: "read_file", Arguments: `{"path":"a.go"}`}},
			{{ID: "r2", Name: "read_file", Arguments: `{"path":"b.go"}`}},
			{{ID: "r3", Name: "read_file", Arguments: `{"path":"c.go"}`}},
			{}, // final answer
		},
	}
	gw := model.NewGateway(fake, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name: "explore", Effects: tool.Effects(),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				return tool.Succeeded("architecture summary")
			},
		},
		{
			Name: "read_file", Effects: tool.Effects(tool.EffectReadFile),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				return tool.Succeeded("file body")
			},
		},
	})
	runner := NewRunner(NewLeadProfile("test"), gw, tool.NewExecutor(catalog, nil, nil), &tool.ToolScope{
		Role: "lead", CanRead: true, CanExecute: true,
	}, nil)

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("analyze the project")}, "post-explore")

	var exploreOK, blockedRead bool
	allowedReads := 0
	for _, tr := range outcome.ToolResults {
		switch {
		case tr.Name == "explore" && tr.Status == tool.StatusSucceeded:
			exploreOK = true
		case tr.Name == "read_file" && tr.Status == tool.StatusSucceeded:
			allowedReads++
		case tr.Name == "read_file" && tr.Status == tool.StatusFailed &&
			strings.Contains(tr.Output, "post-explore read budget"):
			blockedRead = true
		}
	}

	if !exploreOK {
		t.Fatal("explore should have succeeded")
	}
	if allowedReads != config.MaxReadsAfterExplore {
		t.Fatalf("allowed reads after explore = %d, want %d", allowedReads, config.MaxReadsAfterExplore)
	}
	if !blockedRead {
		t.Fatal("expected third read_file to be blocked by post-explore budget")
	}
}

func TestExecuteToolBatch_InjectsPostExploreNudge(t *testing.T) {
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{{
		Name: "explore", Effects: tool.Effects(),
		Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
			return tool.Succeeded("summary")
		},
	}})
	runner := NewRunner(NewLeadProfile("test"), model.NewGateway(&fakeProvider{name: "fake"}, model.NewRoleThrottle(10)),
		tool.NewExecutor(catalog, nil, nil), &tool.ToolScope{Role: "lead", CanRead: true}, nil)

	out := &TurnOutcome{}
	batch := runner.executeToolBatch(context.Background(), nil,
		[]llm.ToolCall{{ID: "e1", Name: "explore", Arguments: `{"prompt":"x"}`}},
		"t", out)

	found := false
	for _, m := range batch.messages {
		if m.Role == "user" && strings.Contains(m.Content, "<post-explore>") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected <post-explore> nudge after successful explore")
	}
	if !runner.turn.explore.Succeeded {
		t.Fatal("exploreSucceeded should be set")
	}
}

func TestRunner_PostExploreBlocksBashFind(t *testing.T) {
	fake := &fakeProvider{
		name:    "fake",
		content: "ok",
		callScript: [][]llm.ToolCall{
			{{ID: "e1", Name: "explore", Arguments: `{"prompt":"survey"}`}},
			{{ID: "b1", Name: "bash", Arguments: `{"command":"find . -name '*.go' | head -50"}`}},
			{},
		},
	}
	gw := model.NewGateway(fake, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{
		{
			Name: "explore", Effects: tool.Effects(),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				return tool.Succeeded("summary")
			},
		},
		{
			Name: "bash", Effects: tool.Effects(tool.EffectExecuteProcess),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				return tool.Succeeded("should not run")
			},
		},
	})
	runner := NewRunner(NewLeadProfile("test"), gw, tool.NewExecutor(catalog, nil, nil), &tool.ToolScope{
		Role: "lead", CanRead: true, CanExecute: true,
	}, nil)

	outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("analyze")}, "post-explore-bash")
	found := false
	for _, tr := range outcome.ToolResults {
		if tr.Name == "bash" && tr.Status == tool.StatusFailed && strings.Contains(tr.Output, "repo walk") {
			found = true
		}
		if tr.Name == "bash" && tr.Status == tool.StatusSucceeded {
			t.Fatal("bash find should not have executed after explore")
		}
	}
	if !found {
		t.Fatal("expected bash find to be blocked after explore")
	}
}

func TestDropConsumedNudges_PostExplore(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("task"),
		llm.UserMessage(postExploreNudge),
		llm.AssistantMessage("answered"),
	}
	out, removed := dropConsumedNudges(msgs)
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	for _, m := range out {
		if isEphemeralNudge(m) {
			t.Fatalf("post-explore nudge survived: %q", m.Content)
		}
	}
}
