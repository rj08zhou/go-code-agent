package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// TaskManager - file-persisted task CRUD
//
// Pure CRUD; graph operations and scheduling are in DAGScheduler (task_scheduler.go).

type TaskManager struct {
	dir      string
	mu       sync.Mutex
	dagSched *DAGScheduler // set after construction via SetScheduler
}

func NewTaskManager(dir string) *TaskManager {
	os.MkdirAll(dir, 0o755)
	return &TaskManager{dir: dir}
}

// SetScheduler injects the DAGScheduler (breaks circular init dependency).
func (tm *TaskManager) SetScheduler(ds *DAGScheduler) {
	tm.dagSched = ds
}

func (tm *TaskManager) taskPath(id int) string {
	return filepath.Join(tm.dir, fmt.Sprintf("task_%d.json", id))
}

func (tm *TaskManager) nextID() int {
	entries, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	maxID := 0
	for _, e := range entries {
		base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(e), "task_"), ".json")
		if n, _ := strconv.Atoi(base); n > maxID {
			maxID = n
		}
	}
	return maxID + 1
}

func (tm *TaskManager) load(id int) (map[string]any, error) {
	data, err := os.ReadFile(tm.taskPath(id))
	if err != nil {
		return nil, fmt.Errorf("task %d not found", id)
	}
	var t map[string]any
	json.Unmarshal(data, &t)
	return t, nil
}

// loadLocked reads a task without acquiring tm.mu (caller must hold DAGScheduler.mu).
func (tm *TaskManager) loadLocked(id int) (map[string]any, error) {
	return tm.load(id)
}

func (tm *TaskManager) save(t map[string]any) {
	id := int(t["id"].(float64))
	data, _ := json.MarshalIndent(t, "", "  ")
	os.WriteFile(tm.taskPath(id), data, 0o644)
}

// loadAll loads all task files (sorted by ID). Caller must hold tm.mu.
func (tm *TaskManager) loadAll() []map[string]any {
	return tm.loadAllFromDisk()
}

// loadAllLocked reads all tasks without acquiring tm.mu (caller must hold DAGScheduler.mu).
func (tm *TaskManager) loadAllLocked() []map[string]any {
	return tm.loadAllFromDisk()
}

func (tm *TaskManager) loadAllFromDisk() []map[string]any {
	entries, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	sort.Strings(entries)
	var result []map[string]any
	for _, e := range entries {
		data, _ := os.ReadFile(e)
		var t map[string]any
		if json.Unmarshal(data, &t) == nil {
			result = append(result, t)
		}
	}
	return result
}

// CRUD operations

func (tm *TaskManager) Create(subject, desc string, dependsOn []int) string {
	tm.mu.Lock()
	id := tm.nextID()
	t := map[string]any{
		"id": float64(id), "subject": subject, "description": desc,
		"status": "pending", "owner": nil,
	}
	tm.save(t)
	tm.mu.Unlock()

	// Delegate edge creation to DAGScheduler.
	if tm.dagSched != nil && len(dependsOn) > 0 {
		tm.dagSched.AddEdgesForCreate(id, dependsOn)
	}

	data, _ := json.MarshalIndent(t, "", "  ")
	return string(data)
}

func (tm *TaskManager) Get(id int) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	t, err := tm.load(id)
	if err != nil {
		return err.Error()
	}
	data, _ := json.MarshalIndent(t, "", "  ")
	return string(data)
}

func (tm *TaskManager) Update(id int, status string) string {
	tm.mu.Lock()
	t, err := tm.load(id)
	if err != nil {
		tm.mu.Unlock()
		return err.Error()
	}
	if status != "" {
		t["status"] = status
		if status == "deleted" {
			os.Remove(tm.taskPath(id))
			tm.mu.Unlock()
			// Delegate edge cleanup to DAGScheduler.
			if tm.dagSched != nil {
				tm.dagSched.RemoveEdgesForTask(id)
			}
			return fmt.Sprintf("Task %d deleted", id)
		}
	}
	tm.save(t)
	tm.mu.Unlock()
	data, _ := json.MarshalIndent(t, "", "  ")
	return string(data)
}

func (tm *TaskManager) ListAll() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	entries, _ := filepath.Glob(filepath.Join(tm.dir, "task_*.json"))
	sort.Strings(entries)
	if len(entries) == 0 {
		return "No tasks."
	}

	// Load edges from scheduler for display.
	var edges []dagEdge
	if tm.dagSched != nil {
		edges = tm.dagSched.loadEdges()
	}
	dagPreds := make(map[int][]int)
	for _, e := range edges {
		dagPreds[e.To] = append(dagPreds[e.To], e.From)
	}

	markers := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]"}
	var lines []string
	for _, e := range entries {
		data, _ := os.ReadFile(e)
		var t map[string]any
		json.Unmarshal(data, &t)
		st, _ := t["status"].(string)
		id := int(t["id"].(float64))
		sub, _ := t["subject"].(string)
		mk := markers[st]
		if mk == "" {
			mk = "[?]"
		}
		owner := ""
		if o, _ := t["owner"].(string); o != "" {
			owner = " @" + o
		}
		depStr := ""
		if preds, ok := dagPreds[id]; ok && len(preds) > 0 {
			var parts []string
			for _, p := range preds {
				parts = append(parts, fmt.Sprintf("#%d", p))
			}
			depStr = fmt.Sprintf(" (depends on %s)", strings.Join(parts, ", "))
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s%s", mk, id, sub, owner, depStr))
	}
	return strings.Join(lines, "\n")
}

// Claim atomically assigns a task to owner. It returns (message, ok).
// The lock+load+check+save sequence is the only place where a task's
// owner transitions from "free" to "taken", so the mutex makes this a
// real CAS (no TOCTOU window between IsReady-check in callers and the
// claim itself). Callers MUST honor ok=false and not proceed as if they
// own the task.
//
// Re-claim by the same owner is allowed (idempotent retry) so a member
// recovering from a crash mid-task does not get locked out of work it
// already owns.
func (tm *TaskManager) Claim(id int, owner string) (string, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	t, err := tm.load(id)
	if err != nil {
		return err.Error(), false
	}
	// Reject if already owned by someone else, regardless of status.
	// Checking owner (not status) is intentional: a completed task with
	// an owner field set should not be silently re-claimed either.
	if existing, _ := t["owner"].(string); existing != "" && existing != owner {
		status, _ := t["status"].(string)
		return fmt.Sprintf("Error: task #%d already claimed by %s (status=%s)", id, existing, status), false
	}
	t["owner"] = owner
	t["status"] = "in_progress"
	tm.save(t)
	return fmt.Sprintf("Claimed task #%d for %s", id, owner), true
}
