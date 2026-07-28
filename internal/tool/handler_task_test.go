package tool

import (
	"encoding/json"
	"strings"
	"testing"
)

type recordingTaskService struct {
	createSubject string
	createDesc    string
	createDeps    []int
	updateID      int
	updateStatus  string
	claimID       int
	claimOwner    string
	addFrom       int
	addTo         int
}

func (f *recordingTaskService) Create(subject, desc string, deps []int) string {
	f.createSubject, f.createDesc, f.createDeps = subject, desc, deps
	return "Created task #1"
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
func (f *recordingTaskService) Reset()                         {}

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
		got := tool.Handler(scope, json.RawMessage(`{"items":[{"content":"do it","status":"pending"}]}`))
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
			"TodoWrite":   {"items", "When NOT to use", "5-node DAG", "status"},
			"task_create": {"depends_on", "When to use", "numeric ID", "task_dag"},
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
