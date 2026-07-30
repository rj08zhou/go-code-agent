package agent

import (
	"context"
	"testing"

	"go-code-agent/internal/config"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
)

func TestRunnerPropagatesReasoningConfigToModelCalls(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *config.Config
		wantNil bool
		wantEff string
	}{
		{name: "nil config disabled", wantNil: true},
		{name: "explicitly disabled", cfg: &config.Config{ReasoningEnabled: false, ReasoningEffort: "high"}, wantNil: true},
		{name: "enabled", cfg: &config.Config{ReasoningEnabled: true, ReasoningEffort: " high "}, wantEff: "high"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProvider{name: "reasoner", reasoning: true, content: "done"}
			runner := NewRunner(
				NewExploreProfile(),
				model.NewGateway(fake, model.NewRoleThrottle(10)),
				nil, nil, tc.cfg,
			)
			out := &TurnOutcome{}
			_, _, early := runner.callModel(
				context.Background(), []llm.Message{llm.UserMessage("inspect")}, nil,
				"reasoner-model", "reasoning-config", out,
			)
			if early != nil {
				t.Fatalf("unexpected early outcome: %#v", early)
			}
			if fake.lastParams == nil {
				t.Fatal("provider was not called")
			}
			if tc.wantNil {
				if fake.lastParams.Reasoning != nil {
					t.Fatalf("Reasoning = %#v, want nil", fake.lastParams.Reasoning)
				}
				return
			}
			if fake.lastParams.Reasoning == nil || !fake.lastParams.Reasoning.Enabled || fake.lastParams.Reasoning.Effort != tc.wantEff {
				t.Fatalf("Reasoning = %#v, want enabled effort %q", fake.lastParams.Reasoning, tc.wantEff)
			}
		})
	}
}

func TestTeammateReasoningConfigUsesSameMapping(t *testing.T) {
	tm := &TeammateManager{}
	tm.SetReasoningConfig(&config.Config{ReasoningEnabled: true, ReasoningEffort: "medium"})
	if tm.reasoning == nil || !tm.reasoning.Enabled || tm.reasoning.Effort != "medium" {
		t.Fatalf("teammate reasoning = %#v", tm.reasoning)
	}
	tm.SetReasoningConfig(&config.Config{ReasoningEnabled: false, ReasoningEffort: "high"})
	if tm.reasoning != nil {
		t.Fatalf("disabled teammate reasoning = %#v", tm.reasoning)
	}
}
