package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
	"go-code-agent/internal/task"
)

type recordingTaskService struct {
	createSubject string
	createDesc    string
	createDeps    []int
	createBatch   string
	resolvedFor   string
	startedNewFor string
	updateID      int
	updateStatus  string
	claimID       int
	claimOwner    string
	addFrom       int
	addTo         int
}

func (f *recordingTaskService) CreateForBatch(batchID, subject, desc string, deps []int) string {
	f.createBatch = batchID
	f.createSubject, f.createDesc, f.createDeps = subject, desc, deps
	return "Created task #1"
}
func (f *recordingTaskService) ResolveActiveBatch(prefix string) string {
	f.resolvedFor = prefix
	return prefix + "-active"
}
func (f *recordingTaskService) StartNewBatch(prefix string) string {
	f.startedNewFor = prefix
	return prefix + "-fresh"
}
func (f *recordingTaskService) Get(id int) string { return "task" }
func (f *recordingTaskService) Update(id int, status string) string {
	f.updateID, f.updateStatus = id, status
	return "updated"
}
func (f *recordingTaskService) ListAll() string { return "[]" }
func (f *recordingTaskService) Claim(id int, owner string) (string, bool) {
	f.claimID, f.claimOwner = id, owner
	return "claimed", true
}
func (f *recordingTaskService) AddEdge(from, to int) string {
	f.addFrom, f.addTo = from, to
	return "edge added"
}
func (f *recordingTaskService) RemoveEdge(from, to int) string { return "edge removed" }
func (f *recordingTaskService) ReadyTasks() string             { return "ready" }
func (f *recordingTaskService) TopoView() string               { return "topo" }
func (f *recordingTaskService) ProgressSummary() string        { return "" }
func (f *recordingTaskService) ClearCompleted() string         { return "cleared" }
func (f *recordingTaskService) Reset() string                  { return "reset" }

type recordingTodoService struct {
	items []map[string]string
}

func (f *recordingTodoService) Update(items []map[string]string) (string, error) {
	f.items = items
	return "todos updated", nil
}
func (f *recordingTodoService) Render() string     { return "todos" }
func (f *recordingTodoService) HasOpenItems() bool { return false }

func TestTaskTools(t *testing.T) {
	scope := &ToolScope{AgentID: "lead"}

	t.Run("task_create delegates", func(t *testing.T) {
		svc := &recordingTaskService{}
		tool := mustTool(t, taskTools(builtinDeps{taskSvc: svc}), "task_create")
		got := tool.Handler(scope, json.RawMessage(`{"subject":"fix bug","description":"details","depends_on":[2]}`))
		if got.Status != StatusSucceeded || !strings.Contains(got.Output, "Created task #1") {
			t.Fatalf("got %#v", got)
		}
		if svc.createSubject != "fix bug" || svc.createDesc != "details" || len(svc.createDeps) != 1 || svc.createDeps[0] != 2 {
			t.Fatalf("create args: %+v", svc)
		}
	})

	t.Run("task_create honours an explicit scope batch", func(t *testing.T) {
		svc := task.NewService(t.TempDir())
		def := mustTool(t, taskTools(builtinDeps{taskSvc: svc}), "task_create")
		result := def.Handler(&ToolScope{TaskBatch: NewTaskBatch("run-test")}, json.RawMessage(`{"subject":"isolated"}`))
		if !result.Succeeded() || !strings.Contains(result.Output, "run-test") {
			t.Fatalf("result = %#v", result)
		}
		if got := svc.ListAll(); !strings.Contains(got, "[run-test]") {
			t.Fatalf("ListAll = %q", got)
		}
	})

	t.Run("task_create resolves the run batch once", func(t *testing.T) {
		svc := &recordingTaskService{}
		def := mustTool(t, taskTools(builtinDeps{taskSvc: svc}), "task_create")
		runScope := &ToolScope{Role: "lead", TaskBatch: NewTaskBatch("")}
		if got := def.Handler(runScope, json.RawMessage(`{"subject":"first"}`)); !got.Succeeded() {
			t.Fatalf("got %#v", got)
		}
		if svc.resolvedFor != "lead" || svc.createBatch != "lead-active" {
			t.Fatalf("resolve: prefix=%q batch=%q", svc.resolvedFor, svc.createBatch)
		}
		if runScope.TaskBatch.ID() != "lead-active" {
			t.Fatalf("batch not remembered: %q", runScope.TaskBatch.ID())
		}

		// The executor copies the scope per call, so a later task_create in the
		// same run must still reuse the batch rather than resolving again.
		svc.resolvedFor = ""
		copied := *runScope
		def.Handler(&copied, json.RawMessage(`{"subject":"second"}`))
		if svc.resolvedFor != "" || svc.createBatch != "lead-active" {
			t.Fatalf("re-resolved mid-run: prefix=%q batch=%q", svc.resolvedFor, svc.createBatch)
		}
	})

	t.Run("task_create new_plan seals the old batch and switches", func(t *testing.T) {
		svc := &recordingTaskService{}
		def := mustTool(t, taskTools(builtinDeps{taskSvc: svc}), "task_create")
		runScope := &ToolScope{Role: "lead", TaskBatch: NewTaskBatch("lead-active")}
		if got := def.Handler(runScope, json.RawMessage(`{"subject":"other request","new_plan":true}`)); !got.Succeeded() {
			t.Fatalf("got %#v", got)
		}
		if svc.startedNewFor != "lead" || svc.createBatch != "lead-fresh" {
			t.Fatalf("new_plan: prefix=%q batch=%q", svc.startedNewFor, svc.createBatch)
		}
		if runScope.TaskBatch.ID() != "lead-fresh" {
			t.Fatalf("batch not switched: %q", runScope.TaskBatch.ID())
		}
	})

	t.Run("batch survives the executor's per-call scope copy", func(t *testing.T) {
		svc := &recordingTaskService{}
		catalog := NewToolCatalog()
		catalog.RegisterAll(taskTools(builtinDeps{taskSvc: svc}))
		exec := NewExecutor(catalog, nil, nil)
		runScope := &ToolScope{Role: "lead", CanRead: true, TaskBatch: NewTaskBatch("")}

		call := llm.ToolCall{Name: "task_create", Arguments: `{"subject":"planned"}`}
		if got := exec.Execute(context.Background(), runScope, call); got.Status != StatusSucceeded {
			t.Fatalf("execute = %#v", got)
		}
		// The plan gate reads the batch off the runner's scope, so a resolution
		// that only lands on the copy would silently disable it.
		if runScope.TaskBatch.ID() != "lead-active" {
			t.Fatalf("batch lost in scope copy: %q", runScope.TaskBatch.ID())
		}
	})

	t.Run("task_create rejects blank subject", func(t *testing.T) {
		svc := &recordingTaskService{}
		tool := mustTool(t, taskTools(builtinDeps{taskSvc: svc}), "task_create")
		got := tool.Handler(scope, json.RawMessage(`{"subject":"  "}`))
		if got.Status != StatusFailed || !strings.Contains(got.Output, "subject is required") {
			t.Fatalf("got %#v", got)
		}
		if svc.createSubject != "" {
			t.Fatalf("invalid task reached service: %q", svc.createSubject)
		}
	})

	t.Run("task_create nil service", func(t *testing.T) {
		tool := mustTool(t, taskTools(builtinDeps{}), "task_create")
		got := tool.Handler(scope, json.RawMessage(`{"subject":"x"}`))
		if got.Status != StatusFailed || !strings.Contains(got.Output, "task service unavailable") {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("task_update requires positive id", func(t *testing.T) {
		tool := mustTool(t, taskTools(builtinDeps{taskSvc: &recordingTaskService{}}), "task_update")
		got := tool.Handler(scope, json.RawMessage(`{"task_id":0,"status":"completed"}`))
		if got.Status != StatusFailed || !strings.Contains(got.Output, "task_id is required") {
			t.Fatalf("got %#v", got)
		}
	})

	t.Run("task_update accepts id alias", func(t *testing.T) {
		svc := &recordingTaskService{}
		tool := mustTool(t, taskTools(builtinDeps{taskSvc: svc}), "task_update")
		got := tool.Handler(scope, json.RawMessage(`{"id":3,"status":"in_progress"}`))
		if got.Status != StatusSucceeded {
			t.Fatalf("got %#v", got)
		}
		if svc.updateID != 3 || svc.updateStatus != "in_progress" {
			t.Fatalf("update = %d/%s", svc.updateID, svc.updateStatus)
		}
	})

	t.Run("claim_task uses AgentID", func(t *testing.T) {
		svc := &recordingTaskService{}
		tool := mustTool(t, taskTools(builtinDeps{taskSvc: svc}), "claim_task")
		got := tool.Handler(scope, json.RawMessage(`{"task_id":9}`))
		if got.Status != StatusSucceeded {
			t.Fatalf("got %#v", got)
		}
		if svc.claimID != 9 || svc.claimOwner != "lead" {
			t.Fatalf("claim = %d/%s", svc.claimID, svc.claimOwner)
		}
	})

	t.Run("task_add_dep delegates", func(t *testing.T) {
		svc := &recordingTaskService{}
		tool := mustTool(t, taskTools(builtinDeps{taskSvc: svc}), "task_add_dep")
		got := tool.Handler(scope, json.RawMessage(`{"from":1,"to":2}`))
		if got.Status != StatusSucceeded || svc.addFrom != 1 || svc.addTo != 2 {
			t.Fatalf("got %#v svc=%+v", got, svc)
		}
	})

	t.Run("TodoWrite delegates", func(t *testing.T) {
		todo := &recordingTodoService{}
		tool := mustTool(t, taskTools(builtinDeps{todoSvc: todo}), "TodoWrite")
		got := tool.Handler(scope, json.RawMessage(`{"items":[{"content":"do it","status":"pending","activeForm":"doing it"}]}`))
		if got.Status != StatusSucceeded || !strings.Contains(got.Output, "todos updated") {
			t.Fatalf("got %#v", got)
		}
		if len(todo.items) != 1 || todo.items[0]["content"] != "do it" {
			t.Fatalf("items = %#v", todo.items)
		}
	})

	t.Run("tool descriptions carry usage guidance", func(t *testing.T) {
		defs := taskTools(builtinDeps{})
		cases := map[string][]string{
			"TodoWrite":   {"items", "When NOT to use", "5-node DAG", "status", "activeForm"},
			"task_create": {"depends_on", "When to use", "numeric ID", "task_dag", "DAG batches", "new_plan"},
			"task_update": {"task_id", "never use 0", "status only"},
			"task_get":    {"numeric task_id", "Never pass 0"},
			"task_list":   {"task_create"},
			"task_dag":    {"task_create"},
		}
		for name, needles := range cases {
			desc := mustTool(t, defs, name).Description
			for _, n := range needles {
				if !strings.Contains(desc, n) {
					t.Errorf("%s description missing %q\n%s", name, n, desc)
				}
			}
		}
	})

	t.Run("TodoWrite nil service", func(t *testing.T) {
		tool := mustTool(t, taskTools(builtinDeps{}), "TodoWrite")
		got := tool.Handler(scope, json.RawMessage(`{"items":[{"content":"x","status":"pending"}]}`))
		if got.Status != StatusFailed || !strings.Contains(got.Output, "todo service unavailable") {
			t.Fatalf("got %#v", got)
		}
	})
}
