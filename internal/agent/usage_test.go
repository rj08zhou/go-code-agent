package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
)

func TestUsageTrackerCreatesSessionDir(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "sessions", "abc")

	u, err := NewUsageTracker(sessionDir)
	if err != nil {
		t.Fatalf("NewUsageTracker: %v", err)
	}
	defer u.Close()

	u.Record("openai", "lead", "m", "t1", llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, 0.5)
	if _, err := os.Stat(filepath.Join(sessionDir, "usage.jsonl")); err != nil {
		t.Fatalf("usage.jsonl missing: %v", err)
	}
}

func TestUsageTrackerPersistsAndRendersReasoningTokens(t *testing.T) {
	u, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	u.Record("openai", "lead", "m", "t1", llm.Usage{
		PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, ReasoningTokens: 9,
	}, 0.5)

	data, err := os.ReadFile(u.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"reasoning_tokens":9`) {
		t.Fatalf("usage.jsonl missing reasoning token count: %s", data)
	}
	if got := u.Render(); !strings.Contains(got, "9 reasoning") {
		t.Fatalf("Render() omitted reasoning token count: %q", got)
	}
}

func TestUsageTrackerDoesNotReopenAfterClose(t *testing.T) {
	u, err := NewUsageTracker(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	u.Record("openai", "lead", "m", "before-close", llm.Usage{TotalTokens: 1}, 0.1)
	before, err := os.ReadFile(u.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}

	u.Record("openai", "lead", "m", "after-close", llm.Usage{TotalTokens: 2}, 0.2)
	after, err := os.ReadFile(u.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("usage changed after Close:\n before %s\n after %s", before, after)
	}
	if u.file != nil {
		t.Fatal("UsageTracker reopened its file after Close")
	}
	if err := u.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRunnerAccumulatesReasoningTokensAcrossModelCalls(t *testing.T) {
	fake := &fakeProvider{
		name:    "fake",
		content: "ok",
		usage:   llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3, ReasoningTokens: 7},
	}
	runner := NewRunner(
		NewExploreProfile(),
		model.NewGateway(fake, model.NewRoleThrottle(10)),
		nil, nil, nil,
	)
	out := &TurnOutcome{}
	for i := 0; i < 2; i++ {
		_, _, early := runner.callModel(
			context.Background(), []llm.Message{llm.UserMessage("inspect")}, nil,
			"m", "reasoning-usage", out,
		)
		if early != nil {
			t.Fatalf("call %d returned early outcome: %#v", i, early)
		}
	}
	if got := runner.turn.usage; got.ReasoningTokens != 14 || got.PromptTokens != 2 || got.CompletionTokens != 4 {
		t.Fatalf("turn usage was not accumulated across calls: %+v", got)
	}
}
