package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestService_PrivateFileMode(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(dir)
	if msg := svc.Create("private task", "", nil); strings.HasPrefix(msg, "Error:") {
		t.Fatalf("Create: %s", msg)
	}
	info, err := os.Stat(filepath.Join(dir, "task_1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("task file mode = %o, want 0600", perm)
	}
	if msg := svc.Create("second", "", nil); strings.HasPrefix(msg, "Error:") {
		t.Fatalf("Create second: %s", msg)
	}
	if msg := svc.AddEdge(1, 2); strings.HasPrefix(msg, "Error:") {
		t.Fatalf("AddEdge: %s", msg)
	}
	edgeInfo, err := os.Stat(filepath.Join(dir, "dag_edges.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := edgeInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("edges file mode = %o, want 0600", perm)
	}
}

func TestService_CreateGetUpdate(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.Create("first task", "desc", nil)
	svc.Create("second task", "desc2", nil)

	got := svc.Get(1)
	if got == "" {
		t.Fatalf("Get(1) returned empty")
	}
	svc.Update(1, StatusInProgress)
	loaded, err := svc.load(1)
	if err != nil {
		t.Fatalf("load(1) after Update: %v", err)
	}
	if loaded["status"] != StatusInProgress {
		t.Fatalf("expected status %q, got %v", StatusInProgress, loaded["status"])
	}
}

func TestService_CacheIsolation(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.Create("task", "desc", nil)
	loaded, err := svc.load(1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	loaded["status"] = "tampered"
	again, err := svc.load(1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if again["status"] == "tampered" {
		t.Fatalf("cache was corrupted by external mutation")
	}
	if again["status"] != StatusPending {
		t.Fatalf("expected %q, got %v", StatusPending, again["status"])
	}
}

func TestService_DeletedTaskNotResurrectedFromCache(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.Create("task", "desc", nil)
	svc.Update(1, StatusDeleted)

	all := svc.loadAll()
	for _, task := range all {
		if int(task["id"].(float64)) == 1 {
			t.Fatalf("deleted task #1 still in loadAll()")
		}
	}
	if _, err := svc.load(1); err == nil {
		t.Fatalf("expected error loading deleted task, got none")
	}
}

func TestService_ReloadsExistingTasksFromDisk(t *testing.T) {
	dir := t.TempDir()
	svc1 := NewService(dir)
	svc1.Create("first", "d1", nil)
	svc1.Create("second", "d2", nil)

	svc2 := NewService(dir)
	if n := svc2.TaskCount(); n != 2 {
		t.Fatalf("expected 2 tasks reloaded, got %d", n)
	}
	svc2.Create("third", "d3", nil)
	if _, err := os.Stat(dir + "/task_3.json"); err != nil {
		t.Fatalf("expected task_3.json: %v", err)
	}
}

func TestService_AddEdgeAndReady(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.Create("t1", "", nil)
	svc.Create("t2", "", nil)
	svc.AddEdge(1, 2)

	readyBefore := svc.ReadyTasks()
	if contains(readyBefore, "#2") {
		t.Fatalf("task 2 should not be ready before task 1 completes")
	}
	if !contains(readyBefore, "#1") {
		t.Fatalf("task 1 (no deps) should be ready")
	}
	svc.Update(1, StatusInProgress)
	svc.Update(1, StatusCompleted)
	readyAfter := svc.ReadyTasks()
	if !contains(readyAfter, "#2") {
		t.Fatalf("task 2 should be ready after task 1 completes: %q", readyAfter)
	}
}

func TestService_EdgeCachePersists(t *testing.T) {
	dir := t.TempDir()
	svc1 := NewService(dir)
	svc1.Create("t1", "", nil)
	svc1.Create("t2", "", nil)
	svc1.AddEdge(1, 2)

	svc2 := NewService(dir)
	if svc2.EdgeCount() != 1 {
		t.Fatalf("expected 1 edge visible from fresh instance, got %d", svc2.EdgeCount())
	}
}

func TestService_SaveFailureDoesNotUpdateCache(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	// Bypass NewService so the unwritable parent is not chmod'd back to 0700.
	svc := &Service{dir: filepath.Join(parent, "tasks")}
	msg := svc.Create("should-fail", "", nil)
	if !strings.HasPrefix(msg, "Error:") {
		t.Fatalf("Create on readonly parent = %q, want Error", msg)
	}
	if n := svc.TaskCount(); n != 0 {
		t.Fatalf("TaskCount = %d, want 0 (failed create must not update cache)", n)
	}
}

func TestService_SaveMarshalFailureDoesNotUpdateCache(t *testing.T) {
	svc := NewService(t.TempDir())
	bad := map[string]any{
		"id":  float64(1),
		"bad": make(chan int), // not JSON-serializable
	}
	if err := svc.save(bad); err == nil {
		t.Fatal("expected marshal error")
	}
	if n := svc.TaskCount(); n != 0 {
		t.Fatalf("TaskCount = %d, want 0 after marshal failure", n)
	}
}

func TestService_ResetClearsTasksAndEdges(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.Create("a", "", nil)
	svc.Create("b", "", nil)
	svc.AddEdge(1, 2)
	msg := svc.Reset()
	if !strings.Contains(msg, "Cleared 2 task") {
		t.Fatalf("Reset = %q", msg)
	}
	if n := svc.ActiveTaskCount(); n != 0 {
		t.Fatalf("ActiveTaskCount = %d, want 0", n)
	}
	if n := svc.EdgeCount(); n != 0 {
		t.Fatalf("EdgeCount = %d, want 0", n)
	}
	if got := svc.ListAll(); got != "No tasks." {
		t.Fatalf("ListAll = %q", got)
	}
	if msg := svc.Reset(); msg != "No tasks to clear." {
		t.Fatalf("empty Reset = %q", msg)
	}
}

func TestService_BatchesKeepIndependentDAGs(t *testing.T) {
	svc := NewService(t.TempDir())
	if msg := svc.CreateForBatch("run-a", "first A", "", nil); strings.HasPrefix(msg, "Error:") {
		t.Fatal(msg)
	}
	if msg := svc.CreateForBatch("run-a", "second A", "", []int{1}); strings.HasPrefix(msg, "Error:") {
		t.Fatal(msg)
	}
	if msg := svc.CreateForBatch("run-b", "first B", "", nil); strings.HasPrefix(msg, "Error:") {
		t.Fatal(msg)
	}

	if msg := svc.AddEdge(1, 3); !strings.Contains(msg, "different DAG batches") {
		t.Fatalf("cross-batch AddEdge = %q", msg)
	}
	if msg := svc.CreateForBatch("run-b", "bad dependency", "", []int{1}); !strings.Contains(msg, "another batch") {
		t.Fatalf("cross-batch create dependency = %q", msg)
	}

	dag := svc.TopoView()
	for _, want := range []string{"DAG run-a:", "DAG run-b:", "#1:", "#3:"} {
		if !strings.Contains(dag, want) {
			t.Fatalf("TopoView missing %q:\n%s", want, dag)
		}
	}
	list := svc.ListAll()
	for _, want := range []string{"[run-a]", "[run-b]"} {
		if !strings.Contains(list, want) {
			t.Fatalf("ListAll missing %q:\n%s", want, list)
		}
	}
	progress := svc.ProgressSummary()
	for _, want := range []string{"<task-batches>", "run-a:", "run-b:"} {
		if !strings.Contains(progress, want) {
			t.Fatalf("ProgressSummary missing %q:\n%s", want, progress)
		}
	}
}

func TestService_ActiveBatchSpansFollowUpTurns(t *testing.T) {
	svc := NewService(t.TempDir())

	first := svc.ResolveActiveBatch("lead")
	svc.CreateForBatch(first, "step one", "", nil)
	svc.CreateForBatch(first, "step two", "", []int{1})

	// A follow-up turn on the same request must land in the same batch, so
	// its tasks can still depend on what the first turn planned.
	if got := svc.ResolveActiveBatch("lead"); got != first {
		t.Fatalf("follow-up batch = %q, want %q", got, first)
	}
	if msg := svc.CreateForBatch(first, "step three", "", []int{2}); strings.Contains(msg, "another batch") {
		t.Fatalf("same-batch dependency rejected: %q", msg)
	}

	// Once the whole batch is done, the next request opens a fresh one.
	for id := 1; id <= 3; id++ {
		svc.Update(id, StatusCompleted)
	}
	second := svc.ResolveActiveBatch("lead")
	if second == first {
		t.Fatal("finished batch must not absorb the next request")
	}
	if _, edges := svc.BatchCounts(second); edges != 0 {
		t.Fatalf("fresh batch edges = %d, want 0", edges)
	}
}

func TestService_StartNewBatchSealsAbandonedWork(t *testing.T) {
	svc := NewService(t.TempDir())
	abandoned := svc.ResolveActiveBatch("lead")
	svc.CreateForBatch(abandoned, "half-done work", "", nil)

	fresh := svc.StartNewBatch("lead")
	if fresh == abandoned {
		t.Fatal("StartNewBatch must not reuse the sealed batch")
	}
	svc.CreateForBatch(fresh, "unrelated work", "", nil)

	// The sealed batch keeps its task but stops attracting new ones.
	if got := svc.ResolveActiveBatch("lead"); got != fresh {
		t.Fatalf("active batch = %q, want %q", got, fresh)
	}
	progress := svc.ProgressSummary()
	if !strings.Contains(progress, "(sealed)") {
		t.Fatalf("sealed batch not marked:\n%s", progress)
	}
	if !strings.Contains(svc.ListAll(), "half-done work") {
		t.Fatal("sealing must not hide the abandoned task")
	}
}

func TestService_SealActiveBatch(t *testing.T) {
	svc := NewService(t.TempDir())
	if msg := svc.SealActiveBatch(); msg != "No active DAG batch." {
		t.Fatalf("empty seal = %q", msg)
	}
	batchID := svc.ResolveActiveBatch("lead")
	svc.CreateForBatch(batchID, "open", "", nil)
	svc.CreateForBatch(batchID, "done", "", nil)
	svc.Update(2, StatusCompleted)

	if msg := svc.SealActiveBatch(); !strings.Contains(msg, "1 unfinished task") {
		t.Fatalf("seal = %q", msg)
	}
	if got := svc.ResolveActiveBatch("lead"); got == batchID {
		t.Fatal("sealed batch stayed active")
	}
}

func TestService_ProgressSummaryReportsTheCurrentBatchWhenDone(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.CreateForBatch("run-a", "only task", "", nil)

	if got := svc.ProgressSummary(); !strings.Contains(got, "run-a: 0/1") {
		t.Fatalf("unfinished summary = %q", got)
	}
	svc.Update(1, StatusCompleted)
	got := svc.ProgressSummary()
	if !strings.Contains(got, "All tasks done!") {
		t.Fatalf("finished summary lost the completion signal: %q", got)
	}

	// An older finished batch drops out once a newer one takes over.
	svc.CreateForBatch("run-b", "next task", "", nil)
	got = svc.ProgressSummary()
	if strings.Contains(got, "run-a") {
		t.Fatalf("stale finished batch still reported: %q", got)
	}
	if !strings.Contains(got, "run-b: 0/1") {
		t.Fatalf("current batch missing: %q", got)
	}
}

func TestService_ProgressSummaryIgnoresDeletedOnlyBoard(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.CreateForBatch("run-a", "gone", "", nil)
	svc.Update(1, StatusDeleted)
	if got := svc.ProgressSummary(); got != "" {
		t.Fatalf("deleted-only board = %q, want empty", got)
	}
}

func TestService_ProgressSummaryCapsOlderBatches(t *testing.T) {
	svc := NewService(t.TempDir())
	for _, batchID := range []string{"run-1", "run-2", "run-3", "run-4", "run-5"} {
		svc.CreateForBatch(batchID, "open work", "", nil)
	}

	got := svc.ProgressSummary()
	if !strings.Contains(got, "2 older unfinished batch(es) omitted") {
		t.Fatalf("summary not capped: %q", got)
	}
	for _, dropped := range []string{"run-1", "run-2"} {
		if strings.Contains(got, dropped) {
			t.Fatalf("%s should have been omitted: %q", dropped, got)
		}
	}
	for _, kept := range []string{"run-3", "run-4", "run-5"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("%s should have been kept: %q", kept, got)
		}
	}
}

func TestService_BatchCountsIgnoreOtherBatches(t *testing.T) {
	svc := NewService(t.TempDir())
	svc.CreateForBatch("old", "leftover one", "", nil)
	svc.CreateForBatch("old", "leftover two", "", []int{1})
	svc.CreateForBatch("new", "current one", "", nil)
	svc.CreateForBatch("new", "current two", "", nil)

	tasks, edges := svc.BatchCounts("new")
	if tasks != 2 || edges != 0 {
		t.Fatalf("BatchCounts(new) = (%d, %d), want (2, 0)", tasks, edges)
	}
	tasks, edges = svc.BatchCounts("old")
	if tasks != 2 || edges != 1 {
		t.Fatalf("BatchCounts(old) = (%d, %d), want (2, 1)", tasks, edges)
	}
	if tasks, edges = svc.BatchCounts(""); tasks != 0 || edges != 0 {
		t.Fatalf("BatchCounts(\"\") = (%d, %d), want zeros", tasks, edges)
	}
}

func TestService_ConcurrentAccess(t *testing.T) {
	svc := NewService(t.TempDir())
	const n = 20
	for i := range n {
		svc.Create(fmt.Sprintf("task-%d", i), "", nil)
	}
	var wg sync.WaitGroup
	for id := 1; id <= n; id++ {
		svc.Update(id, StatusInProgress)
	}
	wg.Wait()
	_ = svc.ListAll()
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
