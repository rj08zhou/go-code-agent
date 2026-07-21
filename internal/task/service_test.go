package task

import (
	"fmt"
	"os"
	"sync"
	"testing"
)

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
