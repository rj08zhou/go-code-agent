package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- parseJSON lenient coercion tests ---

func TestParseJSON_IntFromString(t *testing.T) {
	var s struct {
		TaskID int `json:"task_id"`
	}
	err := parseJSON(json.RawMessage(`{"task_id":"5"}`), &s)
	if err != "" {
		t.Fatalf("parseJSON should coerce string to int: %s", err)
	}
	if s.TaskID != 5 {
		t.Fatalf("expected 5, got %d", s.TaskID)
	}
}

func TestParseJSON_StrictInt(t *testing.T) {
	var s struct {
		TaskID int `json:"task_id"`
	}
	err := parseJSON(json.RawMessage(`{"task_id":5}`), &s)
	if err != "" {
		t.Fatalf("parseJSON strict int: %s", err)
	}
	if s.TaskID != 5 {
		t.Fatalf("expected 5, got %d", s.TaskID)
	}
}

func TestParseJSON_IntArrayFromStrings(t *testing.T) {
	var s struct {
		Deps []int `json:"deps"`
	}
	err := parseJSON(json.RawMessage(`{"deps":["1","2","3"]}`), &s)
	if err != "" {
		t.Fatalf("parseJSON should coerce string array to ints: %s", err)
	}
	if len(s.Deps) != 3 || s.Deps[0] != 1 || s.Deps[2] != 3 {
		t.Fatalf("unexpected deps: %v", s.Deps)
	}
}

func TestParseJSON_NestedInt(t *testing.T) {
	var s struct {
		Inner struct {
			ID int `json:"id"`
		} `json:"inner"`
	}
	err := parseJSON(json.RawMessage(`{"inner":{"id":"42"}}`), &s)
	if err != "" {
		t.Fatalf("parseJSON nested: %s", err)
	}
	if s.Inner.ID != 42 {
		t.Fatalf("expected 42, got %d", s.Inner.ID)
	}
}

func TestParseJSON_TaskAddDep(t *testing.T) {
	var a struct{ From, To int }
	err := parseJSON(json.RawMessage(`{"from":"1","to":"2"}`), &a)
	if err != "" {
		t.Fatalf("task_add_dep string args: %s", err)
	}
	if a.From != 1 || a.To != 2 {
		t.Fatalf("unexpected from/to: %d/%d", a.From, a.To)
	}
}

// --- Integration: real handler with string IDs ---

type fakeTaskService struct {
	edges []string
	deps  string
}

func (f *fakeTaskService) Create(sub, desc string, deps []int) string {
	if len(deps) > 0 {
		f.deps = "with-deps"
	}
	return "task created"
}
func (f *fakeTaskService) Get(int) string                      { return "got" }
func (f *fakeTaskService) Update(id int, status string) string { return "updated" }
func (f *fakeTaskService) ListAll() string                     { return "[]" }
func (f *fakeTaskService) AddEdge(from, to int) string {
	f.edges = append(f.edges, "edge")
	return "added"
}
func (f *fakeTaskService) RemoveEdge(from, to int) string   { return "removed" }
func (f *fakeTaskService) ReadyTasks() string               { return "[]" }
func (f *fakeTaskService) TopoView() string                 { return "" }
func (f *fakeTaskService) ProgressSummary() string          { return "" }
func (f *fakeTaskService) ClearCompleted() string           { return "" }
func (f *fakeTaskService) Reset()                           {}
func (f *fakeTaskService) Claim(int, string) (string, bool) { return "claimed", true }

func TestTaskAddDep_StringArgs(t *testing.T) {
	svc := &fakeTaskService{}
	def := makeTaskTool("task_add_dep", svc)
	dir := t.TempDir()
	scope := &ToolScope{Workdir: dir}

	result := def.Handler(scope, json.RawMessage(`{"from":"1","to":"2"}`))
	if result.Status != StatusSucceeded {
		t.Fatalf("expected success, got %s: %s", result.Status, result.Output)
	}
	if len(svc.edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(svc.edges))
	}
}

func TestTaskCreate_StringDepArray(t *testing.T) {
	svc := &fakeTaskService{}
	def := makeTaskTool("task_create", svc)
	dir := t.TempDir()
	scope := &ToolScope{Workdir: dir}

	result := def.Handler(scope, json.RawMessage(`{"subject":"test","description":"desc","depends_on":["1","2"]}`))
	if result.Status != StatusSucceeded {
		t.Fatalf("expected success, got %s: %s", result.Status, result.Output)
	}
	if svc.deps != "with-deps" {
		t.Fatalf("expected deps, got %q", svc.deps)
	}
}

func TestParseJSON_HashPrefixInt(t *testing.T) {
	var s struct {
		TaskID int `json:"task_id"`
	}
	err := parseJSON(json.RawMessage(`{"task_id":"#1"}`), &s)
	if err != "" {
		t.Fatalf("parseJSON should coerce '#1' to int: %s", err)
	}
	if s.TaskID != 1 {
		t.Fatalf("expected 1, got %d", s.TaskID)
	}
	// Also test without quotes: {"task_id":1} still works.
	err2 := parseJSON(json.RawMessage(`{"task_id":3}`), &s)
	if err2 != "" || s.TaskID != 3 {
		t.Fatalf("strict int failed: %s, val=%d", err2, s.TaskID)
	}
}

func TestTaskUpdate_StringID(t *testing.T) {
	svc := &fakeTaskService{}
	def := makeTaskTool("task_update", svc)
	dir := t.TempDir()
	scope := &ToolScope{Workdir: dir}

	result := def.Handler(scope, json.RawMessage(`{"task_id":"1","status":"completed"}`))
	if result.Status != StatusSucceeded {
		t.Fatalf("expected success, got %s: %s", result.Status, result.Output)
	}
}

// --- write / edit file paths ---

func TestWriteFile_Path(t *testing.T) {
	defs := BuiltinTools(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	dir := t.TempDir()
	scope := &ToolScope{Workdir: dir}

	var writeDef *ToolDefinition
	for i := range defs {
		if defs[i].Name == "write_file" {
			writeDef = &defs[i]
			break
		}
	}
	if writeDef == nil {
		t.Fatal("write_file not found")
	}

	result := writeDef.Handler(scope, json.RawMessage(`{"path":"hello.txt","content":"hello"}`))
	if result.Status != StatusSucceeded {
		t.Fatalf("write_file failed: %s", result.Output)
	}
	data, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("file content mismatch: %q %v", string(data), err)
	}
}

func TestEditFile_Basic(t *testing.T) {
	defs := BuiltinTools(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	dir := t.TempDir()
	scope := &ToolScope{Workdir: dir}

	// find tools
	var writeDef, editDef *ToolDefinition
	for i := range defs {
		switch defs[i].Name {
		case "write_file":
			c := defs[i]
			writeDef = &c
		case "edit_file":
			c := defs[i]
			editDef = &c
		}
	}
	if writeDef == nil || editDef == nil {
		t.Fatal("write_file or edit_file not found")
	}

	writeDef.Handler(scope, json.RawMessage(`{"path":"x.txt","content":"hello world"}`))

	result := editDef.Handler(scope, json.RawMessage(`{"path":"x.txt","old_text":"hello","new_text":"hi"}`))
	if result.Status != StatusSucceeded {
		t.Fatalf("edit_file failed: %s", result.Output)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "x.txt"))
	if string(data) != "hi world" {
		t.Fatalf("unexpected content: %q", string(data))
	}
}
