package task

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newTestManagers wires up a TaskManager + DAGScheduler pair under a
// fresh temp directory, mirroring how session.go constructs them.
func newTestManagers(t *testing.T) (*TaskManager, *DAGScheduler) {
	t.Helper()
	dir := t.TempDir()
	tm := NewTaskManager(dir)
	ds := NewDAGScheduler(dir, tm)
	tm.SetScheduler(ds)
	return tm, ds
}

func TestTaskManager_CreateGetUpdate(t *testing.T) {
	tm, _ := newTestManagers(t)

	tm.Create("first task", "desc", nil)
	tm.Create("second task", "desc2", nil)

	got := tm.Get(1)
	if got == "" {
		t.Fatalf("Get(1) returned empty")
	}

	res := tm.Update(1, StatusInProgress)
	if res == "" {
		t.Fatalf("Update returned empty")
	}
	got2, err := tm.load(1)
	if err != nil {
		t.Fatalf("load(1) after Update: %v", err)
	}
	if got2["status"] != StatusInProgress {
		t.Fatalf("expected status %q, got %v", StatusInProgress, got2["status"])
	}
}

// TestTaskManager_CacheIsolation guards against the class of bug a
// write-through cache can introduce: a caller mutating a map returned
// by load()/loadAllLocked() must never corrupt the manager's cached
// copy (Update/Claim both mutate their loaded map in place before
// calling save()).
func TestTaskManager_CacheIsolation(t *testing.T) {
	tm, _ := newTestManagers(t)
	tm.Create("task", "desc", nil)

	loaded, err := tm.load(1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Mutate the caller's copy directly, bypassing save().
	loaded["status"] = "tampered"

	again, err := tm.load(1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if again["status"] == "tampered" {
		t.Fatalf("cache was corrupted by external mutation of a loaded map")
	}
	if again["status"] != StatusPending {
		t.Fatalf("expected status %q, got %v", StatusPending, again["status"])
	}
}

// TestTaskManager_DeletedTaskNotResurrectedFromCache ensures a
// deleted task's cache entry is actually evicted, not just its file.
func TestTaskManager_DeletedTaskNotResurrectedFromCache(t *testing.T) {
	tm, _ := newTestManagers(t)
	tm.Create("task", "desc", nil)

	tm.Update(1, StatusDeleted)

	all := tm.loadAllLocked()
	for _, task := range all {
		if int(task["id"].(float64)) == 1 {
			t.Fatalf("deleted task #1 still present in cached loadAllLocked()")
		}
	}
	if _, err := tm.load(1); err == nil {
		t.Fatalf("expected error loading deleted task, got none")
	}
}

// TestTaskManager_ReloadsExistingTasksFromDisk simulates a process
// restart: a fresh TaskManager pointed at a directory that already has
// task files on disk must see them (i.e. ensureCache's first load
// picks up ground truth), not start from an empty cache.
func TestTaskManager_ReloadsExistingTasksFromDisk(t *testing.T) {
	dir := t.TempDir()
	tm1 := NewTaskManager(dir)
	tm1.Create("first", "d1", nil)
	tm1.Create("second", "d2", nil)

	tm2 := NewTaskManager(dir) // fresh instance, cold cache
	all := tm2.loadAllLocked()
	if len(all) != 2 {
		t.Fatalf("expected 2 tasks reloaded from disk, got %d", len(all))
	}

	// nextID must also account for pre-existing files, not just what
	// this instance created itself.
	res := tm2.Create("third", "d3", nil)
	if res == "" {
		t.Fatalf("Create failed")
	}
	// The new task must land on disk with id 3 (max existing + 1).
	if _, statErr := os.Stat(filepath.Join(dir, "task_3.json")); statErr != nil {
		t.Fatalf("expected task_3.json to exist: %v", statErr)
	}
}

func TestDAGScheduler_AddEdgeAndReady(t *testing.T) {
	tm, ds := newTestManagers(t)
	tm.Create("t1", "", nil)
	tm.Create("t2", "", nil)

	if msg := ds.AddEdge(1, 2); msg == "" || !containsSubstr(msg, "Added dependency") {
		t.Fatalf("unexpected AddEdge result: %q", msg)
	}
	if ds.IsReady(2) {
		t.Fatalf("task 2 should not be ready before task 1 completes")
	}
	if !ds.IsReady(1) {
		t.Fatalf("task 1 (no deps) should be ready")
	}

	tm.Update(1, StatusInProgress)
	tm.Update(1, StatusCompleted)

	if !ds.IsReady(2) {
		t.Fatalf("task 2 should be ready after task 1 completes")
	}
}

// TestDAGScheduler_EdgeCachePersistsAcrossInstances ensures the edge
// cache write-through actually reaches disk (not just memory), so a
// second scheduler instance over the same dir sees the same edges.
func TestDAGScheduler_EdgeCachePersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	tm1 := NewTaskManager(dir)
	ds1 := NewDAGScheduler(dir, tm1)
	tm1.SetScheduler(ds1)
	tm1.Create("t1", "", nil)
	tm1.Create("t2", "", nil)
	ds1.AddEdge(1, 2)

	tm2 := NewTaskManager(dir)
	ds2 := NewDAGScheduler(dir, tm2)
	tm2.SetScheduler(ds2)
	if ds2.EdgeCount() != 1 {
		t.Fatalf("expected 1 edge visible from a fresh scheduler instance, got %d", ds2.EdgeCount())
	}
}

// TestConcurrentAccess exercises the exact cross-lock pattern the
// cache was designed around: DAGScheduler methods (holding ds.mu only)
// and TaskManager.ListAll (holding tm.mu only, but calling
// ds.loadEdges() with neither) racing against TaskManager writers
// (holding tm.mu). Run with -race to catch any unsynchronized access
// to the caches.
func TestConcurrentAccess(t *testing.T) {
	tm, ds := newTestManagers(t)
	const n = 20
	for i := range n {
		tm.Create(fmt.Sprintf("task-%d", i), "", nil)
	}

	var wg sync.WaitGroup
	for id := 1; id <= n; id++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			tm.Update(id, StatusInProgress)
		}()
		go func() {
			defer wg.Done()
			ds.IsReady(id)
			ds.ReadyTasks()
		}()
		go func() {
			defer wg.Done()
			tm.ListAll()
		}()
	}
	wg.Wait()
}

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
