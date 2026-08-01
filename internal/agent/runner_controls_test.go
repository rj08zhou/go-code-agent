package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go-code-agent/internal/event"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/prompt"
	"go-code-agent/internal/tool"
)

func TestToolEventPayloadOnlyExposesInvestigationMetadata(t *testing.T) {
	read := toolEventPayload(llm.ToolCall{
		Name:      "read_file",
		Arguments: `{"path":"internal/event/sinks.go","offset":60,"limit":12}`,
	})
	if read["path"] != "internal/event/sinks.go" || read["offset"] != "60" || read["limit"] != "12" {
		t.Fatalf("read_file payload = %#v", read)
	}
	search := toolEventPayload(llm.ToolCall{
		Name:      "search_content",
		Arguments: `{"pattern":"ConsoleSink","path":"internal/event"}`,
	})
	if search["path"] != "internal/event" || search["pattern"] != "ConsoleSink" {
		t.Fatalf("search_content payload = %#v", search)
	}
	list := toolEventPayload(llm.ToolCall{Name: "list_dir", Arguments: `{}`})
	if list["path"] != "." {
		t.Fatalf("list_dir payload = %#v", list)
	}
	if payload := toolEventPayload(llm.ToolCall{
		Name:      "write_file",
		Arguments: `{"path":"secret.txt","content":"do not expose"}`,
	}); payload != nil {
		t.Fatalf("write arguments leaked into event payload: %#v", payload)
	}
}

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
	}, nil)
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

type planningEventRecorder struct {
	events []event.Event
}

func (s *planningEventRecorder) Emit(e event.Event) {
	s.events = append(s.events, e)
}

func newPlanningGuardRunner(definitions []tool.ToolDefinition, originalTask string) *Runner {
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll(definitions)
	runner := NewRunner(
		NewLeadProfile("test"),
		nil,
		tool.NewExecutor(catalog, nil, nil),
		&tool.ToolScope{
			Role:       "lead",
			CanRead:    true,
			CanWrite:   true,
			CanExecute: true,
			CanNetwork: true,
			CanMemory:  true,
			CanTeam:    true,
		},
		nil,
	)
	runner.SetPlanGate(NewPlanGate(prompt.NewLoader(), nil))
	runner.turn.originalTask = originalTask
	return runner
}

func TestRunnerPlanningGuardBlocksSideEffectsAndUnclassifiedTools(t *testing.T) {
	tests := []struct {
		name           string
		toolName       string
		effects        tool.EffectSet
		arguments      string
		classification string
	}{
		{name: "write", toolName: "write_file", effects: tool.Effects(tool.EffectWriteFile), arguments: `{}`, classification: "file_mutation"},
		{name: "delete", toolName: "delete_file", effects: tool.Effects(tool.EffectDeleteFile), arguments: `{}`, classification: "file_mutation"},
		{name: "read-only bash", toolName: "bash", effects: tool.Effects(tool.EffectExecuteProcess), arguments: `{"command":"git status"}`, classification: "process_execution"},
		{name: "spawn teammate delegation", toolName: "spawn_teammate", effects: tool.Effects(tool.EffectTeamMutation, tool.EffectDelegation), arguments: `{"name":"w","prompt":"edit"}`, classification: "delegation"},
		{name: "plan approval delegation", toolName: "plan_approval", effects: tool.Effects(tool.EffectTeamMutation, tool.EffectDelegation), arguments: `{"request_id":"1","approve":true}`, classification: "delegation"},
		{name: "unclassified dynamic tool", toolName: "dynamic_tool", arguments: `{}`, classification: "unclassified_effects"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			executed := false
			runner := newPlanningGuardRunner([]tool.ToolDefinition{{
				Name:    tc.toolName,
				Effects: tc.effects,
				Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
					executed = true
					return tool.Succeeded("executed")
				},
			}}, "fix the bug")
			recorder := &planningEventRecorder{}
			runner.SetEventSink(recorder)

			out := &TurnOutcome{}
			runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{{
				ID: "blocked", Name: tc.toolName, Arguments: tc.arguments,
			}}, "planning-guard", out)

			if executed {
				t.Fatal("blocked tool handler executed")
			}
			if len(out.ToolResults) != 1 || out.ToolResults[0].Status != tool.StatusDenied {
				t.Fatalf("tool results = %#v, want one denied result", out.ToolResults)
			}
			if !strings.Contains(out.ToolResults[0].Output, tc.classification) ||
				!strings.Contains(out.ToolResults[0].Output, "next model round") {
				t.Fatalf("denial output = %q", out.ToolResults[0].Output)
			}

			var decision *event.Event
			for i := range recorder.events {
				if recorder.events[i].Type == event.PlanningDecision && recorder.events[i].ToolName == tc.toolName {
					decision = &recorder.events[i]
					break
				}
			}
			if decision == nil {
				t.Fatal("missing PlanningDecision event")
			}
			if decision.Status != "blocked" || !strings.Contains(decision.Error, tc.classification) ||
				!strings.Contains(decision.Output, "round=0") {
				t.Fatalf("planning event = %#v", *decision)
			}
		})
	}
}

func TestRunnerPlanningGuardAllowsReadOnlyAndExplicitlySafeTools(t *testing.T) {
	for _, tc := range []struct {
		name    string
		effects tool.EffectSet
	}{
		{name: "read_file", effects: tool.Effects(tool.EffectReadFile)},
		{name: "search_content", effects: tool.Effects()},
		{name: "send_message", effects: tool.Effects(tool.EffectTeamMutation)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executed := false
			runner := newPlanningGuardRunner([]tool.ToolDefinition{{
				Name:    tc.name,
				Effects: tc.effects,
				Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
					executed = true
					return tool.Succeeded("ok")
				},
			}}, "analyze the architecture and explain how the complete request flow works")
			out := &TurnOutcome{}
			runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{{
				ID: "safe", Name: tc.name, Arguments: `{}`,
			}}, "planning-safe", out)
			if !executed || len(out.ToolResults) != 1 || out.ToolResults[0].Status != tool.StatusSucceeded {
				t.Fatalf("safe tool did not execute: executed=%v results=%#v", executed, out.ToolResults)
			}
		})
	}
}

func TestRunnerPlanningGuardUsesBatchStartSnapshot(t *testing.T) {
	writeExecuted := false
	runner := newPlanningGuardRunner([]tool.ToolDefinition{
		{
			Name:    "TodoWrite",
			Effects: tool.Effects(tool.EffectSessionMutation),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result { return tool.Succeeded("planned") },
		},
		{
			Name:    "write_file",
			Effects: tool.Effects(tool.EffectWriteFile),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				writeExecuted = true
				return tool.Succeeded("written")
			},
		},
	}, "implement the feature")

	first := &TurnOutcome{}
	runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{
		{ID: "plan", Name: "TodoWrite", Arguments: `{"items":[{"content":"edit","status":"pending"}]}`},
		{ID: "write-same-batch", Name: "write_file", Arguments: `{}`},
	}, "same-batch", first)
	if !runner.turn.planning.PlanEstablished {
		t.Fatal("successful TodoWrite did not establish a plan")
	}
	if writeExecuted || len(first.ToolResults) != 2 || first.ToolResults[1].Status != tool.StatusDenied {
		t.Fatalf("same-batch write was not blocked: executed=%v results=%#v", writeExecuted, first.ToolResults)
	}

	second := &TurnOutcome{}
	runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{{
		ID: "write-next-batch", Name: "write_file", Arguments: `{}`,
	}}, "next-batch", second)
	if !writeExecuted || len(second.ToolResults) != 1 || second.ToolResults[0].Status != tool.StatusSucceeded {
		t.Fatalf("next-batch write was not allowed: executed=%v results=%#v", writeExecuted, second.ToolResults)
	}
}

func TestRunnerPlanningGuardDoesNotUnlockAfterFailedPlan(t *testing.T) {
	writeExecuted := false
	runner := newPlanningGuardRunner([]tool.ToolDefinition{
		{
			Name:    "task_create",
			Effects: tool.Effects(tool.EffectSessionMutation),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result { return tool.Failed("invalid plan") },
		},
		{
			Name:    "write_file",
			Effects: tool.Effects(tool.EffectWriteFile),
			Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
				writeExecuted = true
				return tool.Succeeded("written")
			},
		},
	}, "implement the feature")

	failedPlan := &TurnOutcome{}
	runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{{
		ID: "plan", Name: "task_create", Arguments: `{"subject":"edit"}`,
	}}, "failed-plan", failedPlan)
	if runner.turn.planning.PlanEstablished {
		t.Fatal("failed task_create established a plan")
	}

	write := &TurnOutcome{}
	runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{{
		ID: "write", Name: "write_file", Arguments: `{}`,
	}}, "after-failed-plan", write)
	if writeExecuted || len(write.ToolResults) != 1 || write.ToolResults[0].Status != tool.StatusDenied {
		t.Fatalf("write after failed plan was not blocked: executed=%v results=%#v", writeExecuted, write.ToolResults)
	}
}

func TestRunnerPlanningGuardLeavesTrivialRunsUnchanged(t *testing.T) {
	executed := false
	runner := newPlanningGuardRunner([]tool.ToolDefinition{{
		Name:    "write_file",
		Effects: tool.Effects(tool.EffectWriteFile),
		Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
			executed = true
			return tool.Succeeded("written")
		},
	}}, "hello")
	out := &TurnOutcome{}
	runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{{
		ID: "trivial", Name: "write_file", Arguments: `{}`,
	}}, "trivial", out)
	if !executed || len(out.ToolResults) != 1 || out.ToolResults[0].Status != tool.StatusSucceeded {
		t.Fatalf("trivial run changed: executed=%v results=%#v", executed, out.ToolResults)
	}
}

func TestRunnerPlanningGuardEnforcesWithoutPlanGateWired(t *testing.T) {
	executed := false
	runner := newPlanningGuardRunner([]tool.ToolDefinition{{
		Name:    "write_file",
		Effects: tool.Effects(tool.EffectWriteFile),
		Handler: func(*tool.ToolScope, json.RawMessage) tool.Result {
			executed = true
			return tool.Succeeded("written")
		},
	}}, "implement the feature")
	runner.planGate = nil // simulate a caller that forgot SetPlanGate

	out := &TurnOutcome{}
	runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{{
		ID: "no-gate", Name: "write_file", Arguments: `{}`,
	}}, "no-plan-gate", out)
	if executed || len(out.ToolResults) != 1 || out.ToolResults[0].Status != tool.StatusDenied {
		t.Fatalf("hard guard must not depend on planGate wiring: executed=%v results=%#v", executed, out.ToolResults)
	}
}

func TestRunnerPlanningGuardRecordsSessionLogDecision(t *testing.T) {
	runner := newPlanningGuardRunner([]tool.ToolDefinition{{
		Name:    "bash",
		Effects: tool.Effects(tool.EffectExecuteProcess),
		Handler: func(*tool.ToolScope, json.RawMessage) tool.Result { return tool.Succeeded("unexpected") },
	}}, "implement the feature")
	logPath := t.TempDir() + "/session.log"
	logSink, err := event.NewSessionLogSink(logPath)
	if err != nil {
		t.Fatal(err)
	}
	runner.SetEventSink(logSink)

	out := &TurnOutcome{}
	runner.executeToolBatch(context.Background(), nil, []llm.ToolCall{{
		ID: "bash", Name: "bash", Arguments: `{"command":"git status"}`,
	}}, "session-log-planning", out)
	if err := logSink.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(data)
	for _, want := range []string{
		`"type":"planning"`,
		`"tool_call_id":"bash"`,
		`"tool_name":"bash"`,
		`"status":"blocked"`,
		`"payload":{"action":"block_unplanned_side_effect","classification":"process_execution","round":"0"}`,
		`round=0`,
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("session.log missing %q:\n%s", want, logText)
		}
	}
}
