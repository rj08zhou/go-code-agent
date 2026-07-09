package task

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// DAGScheduler - graph operations + scheduling decisions
//
// Inspired by Apache Spark's DAGScheduler, this component owns:
//   - Edge management (add/remove/persist, cycle detection)
//   - Readiness computation (which pending tasks can start)
//   - Completion events (propagate readiness to downstream)
//   - Topological sort + visualization
//   - Progress reporting
//   - Session resume context
//
// It delegates task CRUD to TaskManager (passed as a dependency).

// dagEdge represents a dependency: task From must complete before task To can start.
// Edges are stored in {dir}/dag_edges.json as [{from, to}, ...].
type dagEdge struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type DAGScheduler struct {
	dir     string       // directory containing dag_edges.json
	taskMgr *TaskManager // back-reference for loading tasks
	mu      sync.Mutex

	// In-memory cache of dag_edges.json. loadEdges() used to re-read
	// and re-parse this file on every single DAG query (ReadyTasks,
	// IsReady, TopoView, ProgressSummary, ...); with it warm this drops
	// to one read for the lifetime of the process (until an edge
	// mutates, which writes through immediately).
	//
	// Guarded by its own mutex (edgesMu) rather than ds.mu: loadEdges
	// is also called directly by TaskManager.ListAll without holding
	// ds.mu, so the cache must be safe under that access pattern too.
	//
	// loadEdges always hands back a freshly-allocated slice (copy of
	// the cache) so a caller doing `edges = append(edges, ...)` before
	// saveEdges can never alias — and corrupt — the cached backing
	// array.
	edgesMu     sync.RWMutex
	edgesCache  []dagEdge
	edgesLoaded bool
}

func NewDAGScheduler(dir string, tm *TaskManager) *DAGScheduler {
	return &DAGScheduler{dir: dir, taskMgr: tm}
}

func (ds *DAGScheduler) edgesPath() string {
	return ds.dir + "/dag_edges.json"
}

// loadEdges returns a defensive copy of the cached edge list, lazily
// warming the cache from disk on first access.
func (ds *DAGScheduler) loadEdges() []dagEdge {
	ds.edgesMu.RLock()
	if ds.edgesLoaded {
		out := make([]dagEdge, len(ds.edgesCache))
		copy(out, ds.edgesCache)
		ds.edgesMu.RUnlock()
		return out
	}
	ds.edgesMu.RUnlock()

	ds.edgesMu.Lock()
	defer ds.edgesMu.Unlock()
	if !ds.edgesLoaded { // re-check: another goroutine may have won the race
		data, err := os.ReadFile(ds.edgesPath())
		var edges []dagEdge
		if err == nil {
			json.Unmarshal(data, &edges)
		}
		ds.edgesCache = edges
		ds.edgesLoaded = true
	}
	out := make([]dagEdge, len(ds.edgesCache))
	copy(out, ds.edgesCache)
	return out
}

// saveEdges persists edges to disk and writes through to the cache.
func (ds *DAGScheduler) saveEdges(edges []dagEdge) {
	data, _ := json.MarshalIndent(edges, "", "  ")
	os.WriteFile(ds.edgesPath(), data, 0o644)

	stored := make([]dagEdge, len(edges))
	copy(stored, edges)
	ds.edgesMu.Lock()
	ds.edgesCache = stored
	ds.edgesLoaded = true
	ds.edgesMu.Unlock()
}

// AddEdge adds a dependency: `from` must finish before `to` can start.
func (ds *DAGScheduler) AddEdge(from, to int) string {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	fromTask, err := ds.taskMgr.loadLocked(from)
	if err != nil {
		// 返回用户友好的错误信息，便于 Agent 解析
		return fmt.Sprintf("Error: task #%d does not exist", from)
	}
	toTask, err := ds.taskMgr.loadLocked(to)
	if err != nil {
		return fmt.Sprintf("Error: task #%d does not exist", to)
	}
	if from == to {
		return "Error: a task cannot depend on itself"
	}
	if st, _ := toTask["status"].(string); st == "completed" {
		return fmt.Sprintf("Error: task #%d is already completed, cannot add dependency to it", to)
	}
	if st, _ := fromTask["status"].(string); st == "deleted" {
		return fmt.Sprintf("Error: task #%d is deleted", from)
	}
	if st, _ := toTask["status"].(string); st == "deleted" {
		return fmt.Sprintf("Error: task #%d is deleted", to)
	}

	edges := ds.loadEdges()
	for _, e := range edges {
		if e.From == from && e.To == to {
			return fmt.Sprintf("Edge %d → %d already exists", from, to)
		}
	}
	if ds.hasPath(edges, to, from) {
		return fmt.Sprintf("Error: adding %d → %d would create a cycle", from, to)
	}

	edges = append(edges, dagEdge{From: from, To: to})
	ds.saveEdges(edges)
	return fmt.Sprintf("Added dependency: task #%d must complete before task #%d", from, to)
}

// RemoveEdge removes a dependency edge.
func (ds *DAGScheduler) RemoveEdge(from, to int) string {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	// 验证任务是否存在
	if _, err := ds.taskMgr.loadLocked(from); err != nil {
		return fmt.Sprintf("Error: task #%d does not exist", from)
	}
	if _, err := ds.taskMgr.loadLocked(to); err != nil {
		return fmt.Sprintf("Error: task #%d does not exist", to)
	}

	edges := ds.loadEdges()
	found := false
	filtered := make([]dagEdge, 0)
	for _, e := range edges {
		if e.From == from && e.To == to {
			found = true
			continue
		}
		filtered = append(filtered, e)
	}
	if !found {
		return fmt.Sprintf("Edge %d → %d not found", from, to)
	}
	ds.saveEdges(filtered)
	return fmt.Sprintf("Removed dependency: %d → %d", from, to)
}

// RemoveEdgesForTask removes all edges referencing the given task ID.
func (ds *DAGScheduler) RemoveEdgesForTask(taskID int) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	edges := ds.loadEdges()
	filtered := make([]dagEdge, 0, len(edges))
	for _, e := range edges {
		if e.From != taskID && e.To != taskID {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) != len(edges) {
		ds.saveEdges(filtered)
	}
}

// AddEdgesForCreate adds initial dependency edges when creating a task.
func (ds *DAGScheduler) AddEdgesForCreate(taskID int, dependsOn []int) {
	if len(dependsOn) == 0 {
		return
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()

	edges := ds.loadEdges()
	for _, dep := range dependsOn {
		if _, err := ds.taskMgr.loadLocked(dep); err != nil {
			continue
		}
		if dep == taskID {
			continue
		}
		edges = append(edges, dagEdge{From: dep, To: taskID})
	}
	ds.saveEdges(edges)
}

// ReadyTasks returns a formatted list of pending tasks whose predecessors are all completed.
func (ds *DAGScheduler) ReadyTasks() string {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	tasks := ds.taskMgr.loadAllLocked()
	edges := ds.loadEdges()
	completed := completedSet(tasks)
	readyIDs := readyTaskIDs(tasks, edges, completed)

	if len(readyIDs) == 0 {
		return "No tasks are ready (all pending tasks have unfinished dependencies)."
	}
	var lines []string
	for _, id := range readyIDs {
		lines = append(lines, fmt.Sprintf("#%d: %s", id, taskSubject(tasks, id)))
	}
	return "Ready tasks:\n" + strings.Join(lines, "\n")
}

// IsReady checks whether a task's DAG predecessors are all completed.
func (ds *DAGScheduler) IsReady(taskID int) bool {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	tasks := ds.taskMgr.loadAllLocked()
	edges := ds.loadEdges()
	completed := completedSet(tasks)

	for _, e := range edges {
		if e.To == taskID && !completed[e.From] {
			return false
		}
	}
	return true
}

// OnComplete is called when a task is marked completed. Returns info about
// which downstream tasks are now ready.
func (ds *DAGScheduler) OnComplete(completedID int) string {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	tasks := ds.taskMgr.loadAllLocked()
	edges := ds.loadEdges()
	completed := completedSet(tasks)
	completed[completedID] = true

	readyIDs := readyTaskIDs(tasks, edges, completed)

	var nowReady []string
	for _, id := range readyIDs {
		hasDep := false
		for _, e := range edges {
			if e.To == id {
				hasDep = true
				break
			}
		}
		if hasDep {
			nowReady = append(nowReady, fmt.Sprintf("#%d: %s", id, taskSubject(tasks, id)))
		}
	}

	if len(nowReady) > 0 {
		return fmt.Sprintf("Task #%d completed. Now ready: %s", completedID, strings.Join(nowReady, ", "))
	}
	return ""
}

// TopoView returns a topological ordering of all tasks + the full edge list.
func (ds *DAGScheduler) TopoView() string {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	edges := ds.loadEdges()
	tasks := ds.taskMgr.loadAllLocked()

	if len(tasks) == 0 {
		return "No tasks."
	}

	inDeg := make(map[int]int)
	adj := make(map[int][]int)
	taskInfo := make(map[int]map[string]any)

	for _, t := range tasks {
		id := int(t["id"].(float64))
		inDeg[id] = 0
		taskInfo[id] = t
	}
	for _, e := range edges {
		if _, ok := taskInfo[e.From]; !ok {
			continue
		}
		if _, ok := taskInfo[e.To]; !ok {
			continue
		}
		adj[e.From] = append(adj[e.From], e.To)
		inDeg[e.To]++
	}

	// Kahn's algorithm.
	var queue []int
	for id, deg := range inDeg {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	sort.Ints(queue)

	markers := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]"}
	var sections []string
	var order []int
	stage := 0
	prevStageAllDone := true

	for len(queue) > 0 {
		stage++
		var nextQueue []int

		// Classify tasks in this stage by status.
		var runnable, done int
		taskLines := make([]string, 0, len(queue))
		for _, id := range queue {
			order = append(order, id)
			t := taskInfo[id]
			st, _ := t["status"].(string)
			sub, _ := t["subject"].(string)
			// 修复：处理空 subject 的情况
			if strings.TrimSpace(sub) == "" {
				sub = "(no subject)"
			}
			mk := markers[st]
			if mk == "" {
				mk = "[?]"
			}
			switch st {
			case "completed":
				done++
			case "pending", "in_progress":
				runnable++
			}

			var preds []string
			for _, e := range edges {
				if e.To == id {
					preds = append(preds, fmt.Sprintf("#%d", e.From))
				}
			}
			depStr := ""
			if len(preds) > 0 {
				depStr = fmt.Sprintf(" ← depends on %s", strings.Join(preds, ", "))
			}

			taskLines = append(taskLines, fmt.Sprintf("  %s #%d: %s%s", mk, id, sub, depStr))

			for _, next := range adj[id] {
				inDeg[next]--
				if inDeg[next] == 0 {
					nextQueue = append(nextQueue, next)
				}
			}
		}

		// Stage header: show parallelism hint and blocking status.
		var header string
		switch {
		case runnable == 0:
			header = fmt.Sprintf("Stage %d (%d tasks, all done):", stage, len(queue))
		case !prevStageAllDone:
			header = fmt.Sprintf("Stage %d (%d tasks, blocked until Stage %d completes):", stage, runnable, stage-1)
		case runnable == 1:
			header = fmt.Sprintf("Stage %d (1 task, sequential):", stage)
		default:
			header = fmt.Sprintf("Stage %d (%d tasks parallelizable):", stage, runnable)
		}

		sections = append(sections, header+"\n"+strings.Join(taskLines, "\n"))
		prevStageAllDone = (runnable == 0 && done == len(queue))

		sort.Ints(nextQueue)
		queue = nextQueue
	}

	body := strings.Join(sections, "\n\n")
	if len(order) < len(taskInfo) {
		body += "\n\n  ⚠ WARNING: cycle detected - some tasks unreachable!"
	}

	var edgeLines []string
	for _, e := range edges {
		edgeLines = append(edgeLines, fmt.Sprintf("  #%d → #%d", e.From, e.To))
	}
	edgeSection := ""
	if len(edgeLines) > 0 {
		edgeSection = "\n\nDependency edges:\n" + strings.Join(edgeLines, "\n")
	}

	return "DAG execution plan (tasks in the same Stage can run in parallel):\n\n" + body + edgeSection
}

// ProgressSummary returns a brief progress report.
func (ds *DAGScheduler) ProgressSummary() string {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	tasks := ds.taskMgr.loadAllLocked()
	if len(tasks) == 0 {
		return ""
	}

	total, done, inProg, pending := 0, 0, 0, 0
	for _, t := range tasks {
		total++
		switch st, _ := t["status"].(string); st {
		case "completed":
			done++
		case "in_progress":
			inProg++
		default:
			pending++
		}
	}

	edges := ds.loadEdges()
	completed := completedSet(tasks)
	readyIDs := readyTaskIDs(tasks, edges, completed)

	var nextUp []string
	for _, id := range readyIDs {
		nextUp = append(nextUp, fmt.Sprintf("#%d: %s", id, taskSubject(tasks, id)))
	}

	summary := fmt.Sprintf("<progress>%d/%d tasks completed", done, total)
	if inProg > 0 {
		summary += fmt.Sprintf(", %d in progress", inProg)
	}
	if len(nextUp) > 0 {
		summary += ". Next ready: " + strings.Join(nextUp, ", ")
	} else if pending > 0 {
		summary += ". Remaining tasks are blocked by dependencies."
	} else {
		summary += ". All tasks done!"
	}
	summary += "</progress>"
	return summary
}

// TaskCount returns the number of existing tasks.
func (ds *DAGScheduler) TaskCount() int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return len(ds.taskMgr.loadAllLocked())
}

// EdgeCount returns the number of DAG edges.
func (ds *DAGScheduler) EdgeCount() int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return len(ds.loadEdges())
}

// ResumeContext checks for unfinished tasks and builds a resume prompt.
func (ds *DAGScheduler) ResumeContext() string {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	tasks := ds.taskMgr.loadAllLocked()
	if len(tasks) == 0 {
		return ""
	}

	edges := ds.loadEdges()
	completed := completedSet(tasks)

	var inProgress, pending []map[string]any
	for _, t := range tasks {
		switch st, _ := t["status"].(string); st {
		case "in_progress":
			inProgress = append(inProgress, t)
		case "pending":
			pending = append(pending, t)
		}
	}

	if len(inProgress) == 0 && len(pending) == 0 {
		return ""
	}

	readySet := make(map[int]bool)
	for _, id := range readyTaskIDs(tasks, edges, completed) {
		readySet[id] = true
	}

	var sb strings.Builder
	sb.WriteString("## Resuming Previous Session\n\n")
	sb.WriteString("There are unfinished tasks from a previous session.\n\n")

	if len(inProgress) > 0 {
		sb.WriteString("**In progress (was interrupted):**\n")
		for _, t := range inProgress {
			id := int(t["id"].(float64))
			sub, _ := t["subject"].(string)
			sb.WriteString(fmt.Sprintf("- #%d: %s\n", id, sub))
		}
		sb.WriteString("\n")
	}

	var ready, blocked []map[string]any
	for _, t := range pending {
		id := int(t["id"].(float64))
		if readySet[id] {
			ready = append(ready, t)
		} else {
			blocked = append(blocked, t)
		}
	}

	if len(ready) > 0 {
		sb.WriteString("**Ready to start (predecessors completed):**\n")
		for _, t := range ready {
			id := int(t["id"].(float64))
			sub, _ := t["subject"].(string)
			sb.WriteString(fmt.Sprintf("- #%d: %s\n", id, sub))
		}
		sb.WriteString("\n")
	}

	if len(blocked) > 0 {
		sb.WriteString("**Blocked (waiting on dependencies):**\n")
		for _, t := range blocked {
			id := int(t["id"].(float64))
			sub, _ := t["subject"].(string)
			var deps []string
			for _, e := range edges {
				if e.To == id && !completed[e.From] {
					deps = append(deps, fmt.Sprintf("#%d", e.From))
				}
			}
			sb.WriteString(fmt.Sprintf("- #%d: %s (waiting on %s)\n", id, sub, strings.Join(deps, ", ")))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Use task_dag to see the full execution plan. Resume from where you left off.\n")
	return sb.String()
}

// hasPath returns true if there's a path from src to dst (DFS cycle detection).
func (ds *DAGScheduler) hasPath(edges []dagEdge, src, dst int) bool {
	adj := make(map[int][]int)
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	visited := make(map[int]bool)
	var dfs func(int) bool
	dfs = func(node int) bool {
		if node == dst {
			return true
		}
		if visited[node] {
			return false
		}
		visited[node] = true
		for _, next := range adj[node] {
			if dfs(next) {
				return true
			}
		}
		return false
	}
	return dfs(src)
}

// completedSet returns a set of completed task IDs.
func completedSet(tasks []map[string]any) map[int]bool {
	completed := make(map[int]bool)
	for _, t := range tasks {
		if st, _ := t["status"].(string); st == "completed" {
			completed[int(t["id"].(float64))] = true
		}
	}
	return completed
}

// readyTaskIDs returns IDs of pending tasks whose DAG predecessors are all completed.
func readyTaskIDs(tasks []map[string]any, edges []dagEdge, completed map[int]bool) []int {
	var ready []int
	for _, t := range tasks {
		id := int(t["id"].(float64))
		if st, _ := t["status"].(string); st != "pending" {
			continue
		}
		allDone := true
		for _, e := range edges {
			if e.To == id && !completed[e.From] {
				allDone = false
				break
			}
		}
		if allDone {
			ready = append(ready, id)
		}
	}
	return ready
}

// taskSubject returns the subject of a task by ID.
func taskSubject(tasks []map[string]any, id int) string {
	for _, t := range tasks {
		if int(t["id"].(float64)) == id {
			sub, _ := t["subject"].(string)
			return sub
		}
	}
	return ""
}
