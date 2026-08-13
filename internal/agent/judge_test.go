package agent

import (
	"context"
	"strings"
	"testing"

	"go-code-agent/internal/gateway"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/prompt"
)

const validJudgeVerdictJSON = `{"approved":true,"score":9,"issues":[],"suggestions":["add one edge-case test"],"should_retry":false,"reason":"implementation matches the request"}`

func TestJudgeVerifyRequestsStructuredOutput(t *testing.T) {
	provider := &fakeProvider{name: "fake", content: validJudgeVerdictJSON}
	gw := gateway.NewGateway(provider, gateway.NewRoleThrottle(10))
	judge := NewJudge(true, "judge-model", 7, prompt.NewLoader(), gw)

	verdict, err := judge.Verify(
		context.Background(),
		"implement the feature",
		[]llm.Message{llm.UserMessage("implement the feature")},
		nil,
		"fallback-model",
	)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if verdict == nil || !verdict.Approved || verdict.Score != 9 {
		t.Fatalf("unexpected verdict: %#v", verdict)
	}
	if provider.lastParams == nil || provider.lastParams.StructuredOutput == nil {
		t.Fatal("judge request did not include a structured-output contract")
	}
	output := provider.lastParams.StructuredOutput
	if output.Name != "judge_verdict" {
		t.Fatalf("structured output name = %q", output.Name)
	}
	required, ok := output.Schema["required"].([]string)
	if !ok || len(required) != 6 {
		t.Fatalf("required schema fields = %#v", output.Schema["required"])
	}
	if additional, ok := output.Schema["additionalProperties"].(bool); !ok || additional {
		t.Fatalf("additionalProperties must be false, got %#v", output.Schema["additionalProperties"])
	}
}

func TestParseJudgeResponseStrict(t *testing.T) {
	verdict, err := parseJudgeResponse(validJudgeVerdictJSON)
	if err != nil {
		t.Fatalf("valid structured verdict rejected: %v", err)
	}
	if !verdict.Approved || verdict.Score != 9 || verdict.ShouldRetry {
		t.Fatalf("unexpected verdict: %#v", verdict)
	}

	invalid := map[string]string{
		"markdown wrapper": "```json\n" + validJudgeVerdictJSON + "\n```",
		"unknown field":    `{"approved":true,"score":9,"issues":[],"suggestions":[],"should_retry":false,"reason":"ok","extra":1}`,
		"missing field":    `{"approved":true,"score":9,"issues":[],"suggestions":[],"reason":"ok"}`,
		"score too high":   `{"approved":true,"score":11,"issues":[],"suggestions":[],"should_retry":false,"reason":"ok"}`,
		"trailing value":   validJudgeVerdictJSON + ` {}`,
	}
	for name, content := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := parseJudgeResponse(content); err == nil {
				t.Fatal("expected strict parser error")
			}
		})
	}
}

func TestJudgeVerifySchemaViolationIsExplicit(t *testing.T) {
	provider := &fakeProvider{name: "fake", content: "not-json"}
	gw := gateway.NewGateway(provider, gateway.NewRoleThrottle(10))
	judge := NewJudge(true, "judge-model", 7, prompt.NewLoader(), gw)

	verdict, err := judge.Verify(context.Background(), "task", nil, nil, "fallback-model")
	if err == nil {
		t.Fatal("expected schema violation error")
	}
	if verdict == nil || !strings.Contains(verdict.Reason, "schema violation") {
		t.Fatalf("degraded verdict must explain schema violation, got %#v", verdict)
	}
}

func TestRunnerJudgeUsesCapturedOriginalTask(t *testing.T) {
	provider := &fakeProvider{name: "fake", content: validJudgeVerdictJSON}
	gw := gateway.NewGateway(provider, gateway.NewRoleThrottle(10))
	runner := NewRunner(NewLeadProfile("test"), nil, nil, nil, nil)
	runner.SetJudge(NewJudge(true, "judge-model", 7, prompt.NewLoader(), gw))
	runner.turn.originalTask = "implement the requested feature"

	messages := []llm.Message{
		llm.UserMessage("implement the requested feature"),
		llm.UserMessage("Relevant memory:\nthis is context, not the task"),
		llm.AssistantMessage("done"),
	}
	out := TurnOutcome{}
	_, cont, _ := runner.handleNoToolCalls(context.Background(), messages, "fallback-model", "judge-original-task", &out)
	if cont {
		t.Fatal("approved judge verdict unexpectedly requested another loop")
	}
	if provider.lastParams == nil || len(provider.lastParams.Messages) != 1 {
		t.Fatal("judge model did not receive its request")
	}
	judgePrompt := provider.lastParams.Messages[0].Content
	if !strings.Contains(judgePrompt, "## Original Task\nimplement the requested feature\n") {
		t.Fatalf("judge prompt did not use captured original task:\n%s", judgePrompt)
	}
	if strings.Contains(judgePrompt, "## Original Task\nRelevant memory:") {
		t.Fatalf("judge treated injected memory as the original task:\n%s", judgePrompt)
	}
}
