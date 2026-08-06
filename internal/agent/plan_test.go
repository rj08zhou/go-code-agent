package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/prompt"
	taskpkg "go-code-agent/internal/task"
	"go-code-agent/internal/tool"
)

func TestPlanGateUsesDerivedPlanningState(t *testing.T) {
	gate := NewPlanGate(prompt.NewLoader(), nil)
	tests := []struct {
		name            string
		round           int
		task            string
		planEstablished bool
		wantPrompt      bool
	}{
		{name: "non-trivial run needs plan", task: "implement the feature", wantPrompt: true},
		{name: "short English mutation needs plan", task: "delete file", wantPrompt: true},
		{name: "short English add needs plan", task: "please add cache", wantPrompt: true},
		{name: "short English patch needs plan", task: "patch auth middleware", wantPrompt: true},
		{name: "short English purge needs plan", task: "purge generated artifacts", wantPrompt: true},
		{name: "short Chinese mutation needs plan", task: "删除文件", wantPrompt: true},
		{name: "short Chinese write needs plan", task: "写个测试", wantPrompt: true},
		{name: "short Chinese clear needs plan", task: "清空构建目录", wantPrompt: true},
		{name: "established plan needs no nudge", task: "implement the feature", planEstablished: true},
		{name: "trivial run needs no plan", task: "hello"},
		{name: "planning nudge is round zero only", round: 1, task: "implement the feature"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, action := gate.Eval(tc.round, tc.planEstablished, tc.task, "")
			if tc.wantPrompt != strings.Contains(got, "<planning-required>") {
				t.Fatalf("Eval() = %q, wantPrompt = %v", got, tc.wantPrompt)
			}
			if tc.wantPrompt && action != "require_plan" {
				t.Fatalf("action = %q, want require_plan", action)
			}
			if !tc.wantPrompt && action != "" {
				t.Fatalf("action = %q, want empty", action)
			}
		})
	}
}

func TestPlanGateDAGCheckIsBatchScoped(t *testing.T) {
	svc := taskpkg.NewService(t.TempDir())
	svc.CreateForBatch("old", "leftover one", "", nil)
	svc.CreateForBatch("old", "leftover two", "", nil)
	gate := NewPlanGate(prompt.NewLoader(), svc)

	nudged := func(batchID string) bool {
		msg, _ := gate.Eval(1, true, "implement the feature", batchID)
		return strings.Contains(msg, "NO dependencies")
	}

	if nudged("") {
		t.Fatal("a run that created no tasks must not be judged on leftover batches")
	}
	if !nudged("old") {
		t.Fatal("two edgeless tasks in the active batch should be nudged")
	}

	svc.CreateForBatch("current", "only one", "", nil)
	if nudged("current") {
		t.Fatal("a single task needs no dependency edge")
	}
	svc.CreateForBatch("current", "second one", "", nil)
	if !nudged("current") {
		t.Fatal("edgeless pair in the current batch should be nudged")
	}
	// The edge belongs to another batch, so it must not silence the nudge.
	svc.AddEdge(1, 2)
	if !nudged("current") {
		t.Fatal("an edge in a different batch must not count")
	}
	svc.AddEdge(3, 4)
	if nudged("current") {
		t.Fatal("nudge should stop once the current batch has an edge")
	}
}

func TestRunnerPlanEstablishedOnlyBySuccessfulPlanningMutations(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		result    tool.Result
		wantPlan  bool
		arguments string
	}{
		{name: "successful TodoWrite", toolName: "TodoWrite", result: tool.Succeeded("ok"), wantPlan: true, arguments: `{"items":[{"content":"a","status":"pending","activeForm":"doing a"}]}`},
		{name: "failed TodoWrite", toolName: "TodoWrite", result: tool.Failed("no"), arguments: `{"items":[{"content":"a","status":"pending","activeForm":"doing a"}]}`},
		{name: "empty TodoWrite", toolName: "TodoWrite", result: tool.Succeeded("No todos."), arguments: `{"items":[]}`},
		{name: "successful task_create", toolName: "task_create", result: tool.Succeeded("ok"), wantPlan: true, arguments: `{"subject":"a"}`},
		{name: "failed task_create", toolName: "task_create", result: tool.Failed("no"), arguments: `{"subject":"a"}`},
		{name: "task_list query", toolName: "task_list", result: tool.Succeeded("ok"), arguments: `{}`},
		{name: "task_dag query", toolName: "task_dag", result: tool.Succeeded("ok"), arguments: `{}`},
		{name: "task_ready query", toolName: "task_ready", result: tool.Succeeded("ok"), arguments: `{}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProvider{name: "fake", content: "done"}
			gateway := model.NewGateway(fake, model.NewRoleThrottle(10))
			catalog := tool.NewToolCatalog()
			catalog.RegisterAll([]tool.ToolDefinition{{
				Name:    tc.toolName,
				Effects: tool.Effects(),
				Handler: func(*tool.ToolScope, json.RawMessage) tool.Result { return tc.result },
			}})
			runner := NewRunner(
				NewLeadProfile("test"),
				gateway,
				tool.NewExecutor(catalog, nil, nil),
				&tool.ToolScope{Role: "lead"},
				nil,
			)
			fake.toolCalls = []llm.ToolCall{{ID: "plan-call", Name: tc.toolName, Arguments: tc.arguments}}
			fake.withOneShot()

			outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("implement the feature")}, "plan-state")
			if outcome.Error != nil {
				t.Fatalf("Run() error = %v", outcome.Error)
			}
			if got := runner.turn.planning.PlanEstablished; got != tc.wantPlan {
				t.Fatalf("PlanEstablished = %v, want %v", got, tc.wantPlan)
			}
		})
	}
}

func TestRunnerPlanStateMatchesProductionPlanningTools(t *testing.T) {
	tests := []struct {
		name       string
		toolName   string
		arguments  string
		wantPlan   bool
		wantStatus tool.Status
	}{
		{name: "blank task subject", toolName: "task_create", arguments: `{}`, wantStatus: tool.StatusFailed},
		{name: "valid task", toolName: "task_create", arguments: `{"subject":"edit feature"}`, wantPlan: true, wantStatus: tool.StatusSucceeded},
		{name: "empty todo", toolName: "TodoWrite", arguments: `{"items":[]}`, wantStatus: tool.StatusSucceeded},
		{name: "completed todo", toolName: "TodoWrite", arguments: `{"items":[{"content":"done","status":"completed","activeForm":"finishing"}]}`, wantStatus: tool.StatusSucceeded},
		{name: "open todo", toolName: "TodoWrite", arguments: `{"items":[{"content":"edit feature","status":"pending","activeForm":"editing feature"}]}`, wantPlan: true, wantStatus: tool.StatusSucceeded},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeProvider{name: "fake", content: "done", toolCalls: []llm.ToolCall{{
				ID: "plan", Name: tc.toolName, Arguments: tc.arguments,
			}}}
			fake.withOneShot()
			gateway := model.NewGateway(fake, model.NewRoleThrottle(10))
			catalog := tool.NewToolCatalog()
			catalog.RegisterAll(tool.BuiltinTools(
				taskpkg.NewService(t.TempDir()), &taskpkg.TodoManager{}, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			))
			runner := NewRunner(
				NewLeadProfile("test"), gateway, tool.NewExecutor(catalog, nil, nil),
				&tool.ToolScope{Role: "lead", CanRead: true, CanWrite: true, CanExecute: true}, nil,
			)

			outcome := runner.Run(context.Background(), []llm.Message{llm.UserMessage("implement the feature")}, "production-plan")
			if outcome.Error != nil {
				t.Fatalf("Run() error = %v", outcome.Error)
			}
			if len(outcome.ToolResults) != 1 || outcome.ToolResults[0].Status != tc.wantStatus {
				t.Fatalf("tool results = %#v, want status %s", outcome.ToolResults, tc.wantStatus)
			}
			if runner.turn.planning.PlanEstablished != tc.wantPlan {
				t.Fatalf("PlanEstablished = %v, want %v", runner.turn.planning.PlanEstablished, tc.wantPlan)
			}
		})
	}
}

func TestRunnerResetsPlanEstablishedForEveryRun(t *testing.T) {
	fake := &fakeProvider{name: "fake", content: "done"}
	gateway := model.NewGateway(fake, model.NewRoleThrottle(10))
	catalog := tool.NewToolCatalog()
	catalog.RegisterAll([]tool.ToolDefinition{{
		Name:    "TodoWrite",
		Effects: tool.Effects(),
		Handler: func(*tool.ToolScope, json.RawMessage) tool.Result { return tool.Succeeded("ok") },
	}})
	runner := NewRunner(
		NewLeadProfile("test"),
		gateway,
		tool.NewExecutor(catalog, nil, nil),
		&tool.ToolScope{Role: "lead"},
		nil,
	)
	fake.toolCalls = []llm.ToolCall{{
		ID:        "plan-call",
		Name:      "TodoWrite",
		Arguments: `{"items":[{"content":"a","status":"pending"}]}`,
	}}
	fake.withOneShot()

	first := runner.Run(context.Background(), []llm.Message{llm.UserMessage("implement the feature")}, "first-run")
	if first.Error != nil {
		t.Fatalf("first Run() error = %v", first.Error)
	}
	if !runner.turn.planning.PlanEstablished {
		t.Fatal("first Run() did not establish a plan")
	}

	fake.callCount = 0
	fake.toolCalls = nil
	fake.oneShot = false
	second := runner.Run(context.Background(), []llm.Message{llm.UserMessage("continue the implementation")}, "second-run")
	if second.Error != nil {
		t.Fatalf("second Run() error = %v", second.Error)
	}
	if runner.turn.planning.PlanEstablished {
		t.Fatal("second Run() inherited the previous Run's plan authorization")
	}
}
