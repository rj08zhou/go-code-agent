// Package task provides persistent task CRUD and DAG scheduling.
package task

import (
	"encoding/json"
	"fmt"
	"go-code-agent-refactor/internal/store"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusDeleted    = "deleted"
)

var validStatuses = map[string]bool{
	StatusPending: true, StatusInProgress: true, StatusCompleted: true, StatusDeleted: true,
}

var validTransitions = map[string]map[string]bool{
	StatusPending:    {StatusInProgress: true, StatusCompleted: true, StatusDeleted: true},
	StatusInProgress: {StatusCompleted: true, StatusPending: true, StatusDeleted: true},
	StatusCompleted:  {StatusDeleted: true},
}

type dagEdge struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// Service manages task CRUD and DAG edges. Replaces the old TaskManager + DAGScheduler pair.
type Service struct {
	dir     string
	mu      sync.Mutex
	cacheMu sync.RWMutex
	cache   map[int]map[string]any
	loaded  bool

	edgesMu     sync.RWMutex
	edges       []dagEdge
	edgesLoaded bool
}

func NewService(dir string) *Service {
	os.MkdirAll(dir, 0o755)
	return &Service{dir: dir}
}

func (s *Service) edgesPath() string { return filepath.Join(s.dir, "dag_edges.json") }

func (s *Service) taskPath(id int) string {
	return filepath.Join(s.dir, fmt.Sprintf("task_%d.json", id))
}

func (s *Service) loadEdges() []dagEdge {
	s.edgesMu.RLock()
	if s.edgesLoaded {
		out := make([]dagEdge, len(s.edges))
		copy(out, s.edges)
		s.edgesMu.RUnlock()
		return out
	}
	s.edgesMu.RUnlock()
	s.edgesMu.Lock()
	defer s.edgesMu.Unlock()
	if !s.edgesLoaded {
		data, err := os.ReadFile(s.edgesPath())
		if err == nil {
			json.Unmarshal(data, &s.edges)
		}
		s.edgesLoaded = true
	}
	out := make([]dagEdge, len(s.edges))
	copy(out, s.edges)
	return out
}

func (s *Service) saveEdges(edges []dagEdge) {
	data, _ := json.MarshalIndent(edges, "", "  ")
	store.AtomicWrite(s.edgesPath(), data)
	s.edgesMu.Lock()
	s.edges = make([]dagEdge, len(edges))
	copy(s.edges, edges)
	s.edgesLoaded = true
	s.edgesMu.Unlock()
}

func (s *Service) ensureCache() {
	s.cacheMu.RLock()
	if s.loaded {
		s.cacheMu.RUnlock()
		return
	}
	s.cacheMu.RUnlock()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.loaded {
		return
	}
	cache := make(map[int]map[string]any)
	entries, _ := filepath.Glob(filepath.Join(s.dir, "task_*.json"))
	for _, e := range entries {
		data, _ := os.ReadFile(e)
		var t map[string]any
		if json.Unmarshal(data, &t) == nil {
			if id, ok := t["id"].(float64); ok {
				cache[int(id)] = t
			}
		}
	}
	s.cache = cache
	s.loaded = true
}

func (s *Service) nextID() int {
	s.ensureCache()
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	maxID := 0
	for id := range s.cache {
		if id > maxID {
			maxID = id
		}
	}
	return maxID + 1
}

func (s *Service) load(id int) (map[string]any, error) {
	s.ensureCache()
	s.cacheMu.RLock()
	t, ok := s.cache[id]
	s.cacheMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("task %d not found", id)
	}
	c := make(map[string]any, len(t))
	for k, v := range t {
		c[k] = v
	}
	return c, nil
}

func (s *Service) save(t map[string]any) {
	id := int(t["id"].(float64))
	data, _ := json.MarshalIndent(t, "", "  ")
	store.AtomicWrite(s.taskPath(id), data)
	s.cacheMu.Lock()
	if s.cache == nil {
		s.cache = make(map[int]map[string]any)
	}
	cp := make(map[string]any, len(t))
	for k, v := range t {
		cp[k] = v
	}
	s.cache[id] = cp
	s.cacheMu.Unlock()
}

func (s *Service) loadAll() []map[string]any {
	s.ensureCache()
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	ids := make([]int, 0, len(s.cache))
	for id := range s.cache {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	result := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		cp := make(map[string]any, len(s.cache[id]))
		for k, v := range s.cache[id] {
			cp[k] = v
		}
		result = append(result, cp)
	}
	return result
}

// --- CRUD ---

func (s *Service) Create(subject, desc string, dependsOn []int) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "Error: subject is required"
	}
	s.mu.Lock()
	id := s.nextID()
	t := map[string]any{
		"id": float64(id), "subject": subject, "description": desc,
		"status": StatusPending, "owner": nil,
	}
	s.save(t)
	s.mu.Unlock()

	if len(dependsOn) > 0 {
		existing := s.loadAllIDs()
		edges := s.loadEdges()
		s.mu.Lock()
		var warn []string
		for _, dep := range dependsOn {
			if dep == id {
				warn = append(warn, fmt.Sprintf("self-dependency #%d skipped", dep))
				continue
			}
			if !existing[dep] {
				warn = append(warn, fmt.Sprintf("dependency #%d not found, skipped", dep))
				continue
			}
			edges = append(edges, dagEdge{From: dep, To: id})
		}
		s.saveEdges(edges)
		s.mu.Unlock()
		if len(warn) > 0 {
			return fmt.Sprintf("Created task #%d: %s\n[WARN] %s", id, subject, strings.Join(warn, "; "))
		}
	}

	return fmt.Sprintf("Created task #%d: %s", id, subject)
}

func (s *Service) loadAllIDs() map[int]bool {
	s.ensureCache()
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	ids := make(map[int]bool, len(s.cache))
	for id := range s.cache {
		ids[id] = true
	}
	return ids
}

func (s *Service) Get(id int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.load(id)
	if err != nil {
		return err.Error()
	}
	data, _ := json.MarshalIndent(t, "", "  ")
	return string(data)
}

func (s *Service) Update(id int, status string) string {
	if id == 0 {
		return "Error: task_id must be a positive integer. Use the task ID returned by task_create (e.g. #1, #2)."
	}
	s.mu.Lock()
	t, err := s.load(id)
	if err != nil {
		s.mu.Unlock()
		return err.Error()
	}
	if !validStatuses[status] {
		s.mu.Unlock()
		return fmt.Sprintf("Error: invalid status %q", status)
	}
	oldStatus, _ := t["status"].(string)
	if status != oldStatus {
		allowed := validTransitions[oldStatus]
		if !allowed[status] {
			s.mu.Unlock()
			return fmt.Sprintf("Error: invalid transition %s -> %s", oldStatus, status)
		}
	}
	if status == StatusDeleted {
		os.Remove(s.taskPath(id))
		s.cacheMu.Lock()
		delete(s.cache, id)
		s.cacheMu.Unlock()
		s.mu.Unlock()
		// Clean edges
		s.mu.Lock()
		edges := s.loadEdges()
		filtered := make([]dagEdge, 0, len(edges))
		for _, e := range edges {
			if e.From != id && e.To != id {
				filtered = append(filtered, e)
			}
		}
		s.saveEdges(filtered)
		s.mu.Unlock()
		return fmt.Sprintf("Task %d deleted", id)
	}
	t["status"] = status
	s.save(t)
	if status == StatusCompleted {
		// Trigger OnComplete
		ready := s.onComplete(id)
		s.mu.Unlock()
		data, _ := json.MarshalIndent(t, "", "  ")
		result := string(data)
		if ready != "" {
			result += "\n" + ready
		}
		return result
	}
	s.mu.Unlock()
	data, _ := json.MarshalIndent(t, "", "  ")
	return string(data)
}

func (s *Service) ListAll() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.loadAll()
	edges := s.loadEdges()
	dagPreds := make(map[int][]int)
	for _, e := range edges {
		dagPreds[e.To] = append(dagPreds[e.To], e.From)
	}
	markers := map[string]string{StatusPending: "[ ]", StatusInProgress: "[>]", StatusCompleted: "[x]"}
	var lines []string
	for _, t := range tasks {
		st, _ := t["status"].(string)
		if st == StatusDeleted {
			continue
		}
		id := int(t["id"].(float64))
		sub, _ := t["subject"].(string)
		mk := markers[st]
		if mk == "" {
			mk = "[?]"
		}
		depStr := ""
		if preds, ok := dagPreds[id]; ok && len(preds) > 0 {
			var parts []string
			for _, p := range preds {
				parts = append(parts, fmt.Sprintf("#%d", p))
			}
			depStr = fmt.Sprintf(" (depends on %s)", strings.Join(parts, ", "))
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s", mk, id, sub, depStr))
	}
	if len(lines) == 0 {
		return "No tasks."
	}
	return strings.Join(lines, "\n")
}

func (s *Service) Claim(id int, owner string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.load(id)
	if err != nil {
		return err.Error(), false
	}
	if existing, _ := t["owner"].(string); existing != "" && existing != owner {
		status, _ := t["status"].(string)
		return fmt.Sprintf("Error: task #%d already claimed by %s (status=%s)", id, existing, status), false
	}
	t["owner"] = owner
	t["status"] = StatusInProgress
	s.save(t)
	return fmt.Sprintf("Claimed task #%d for %s", id, owner), true
}

func completedSet(tasks []map[string]any) map[int]bool {
	c := make(map[int]bool)
	for _, t := range tasks {
		if st, _ := t["status"].(string); st == StatusCompleted {
			c[int(t["id"].(float64))] = true
		}
	}
	return c
}

func (s *Service) onComplete(completedID int) string {
	tasks := s.loadAll()
	edges := s.loadEdges()
	done := completedSet(tasks)
	done[completedID] = true

	var ready []string
	for _, t := range tasks {
		id := int(t["id"].(float64))
		if st, _ := t["status"].(string); st != StatusPending {
			continue
		}
		allDone := true
		for _, e := range edges {
			if e.To == id && !done[e.From] {
				allDone = false
				break
			}
		}
		if allDone {
			sub, _ := t["subject"].(string)
			ready = append(ready, fmt.Sprintf("#%d: %s", id, sub))
		}
	}
	if len(ready) > 0 {
		return fmt.Sprintf("Task #%d completed. Now ready: %s", completedID, strings.Join(ready, ", "))
	}
	return ""
}

// --- DAG views ---

func (s *Service) TaskCount() int {
	s.ensureCache()
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return len(s.cache)
}

func (s *Service) TaskCountSafe() int { return s.TaskCount() }

func (s *Service) EdgeCount() int {
	edges := s.loadEdges()
	return len(edges)
}

func (s *Service) TopoView() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.loadAll()
	edges := s.loadEdges()
	markers := map[string]string{"pending": "[ ]", "in_progress": "[>]", "completed": "[x]"}
	var lines []string
	for _, t := range tasks {
		st, _ := t["status"].(string)
		if st == StatusDeleted {
			continue
		}
		id := int(t["id"].(float64))
		sub, _ := t["subject"].(string)
		mk := markers[st]
		if mk == "" {
			mk = "[?]"
		}
		var deps []string
		for _, e := range edges {
			if e.To == id {
				deps = append(deps, fmt.Sprintf("#%d", e.From))
			}
		}
		depStr := ""
		if len(deps) > 0 {
			depStr = " <- " + strings.Join(deps, ", ")
		}
		lines = append(lines, fmt.Sprintf("%s #%d: %s%s", mk, id, sub, depStr))
	}
	if len(lines) == 0 {
		return "No tasks."
	}
	return "DAG:\n" + strings.Join(lines, "\n")
}

// ClearCompleted marks all completed status tasks as deleted.
func (s *Service) ClearCompleted() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.loadAll()
	count := 0
	for _, t := range tasks {
		if st, _ := t["status"].(string); st == StatusCompleted {
			t["status"] = StatusDeleted
			s.save(t)
			count++
		}
	}
	if count == 0 {
		return "No completed tasks to clear."
	}
	return fmt.Sprintf("Cleared %d completed task(s).", count)
}

// Reset deletes all task files and the edges file.
func (s *Service) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.loadAll()
	for _, t := range tasks {
		id := int(t["id"].(float64))
		_ = os.Remove(s.taskPath(id))
	}
	_ = os.Remove(s.edgesPath())
	s.cacheMu.Lock()
	s.cache = nil
	s.loaded = false
	s.cacheMu.Unlock()
	s.edgesMu.Lock()
	s.edges = nil
	s.edgesLoaded = false
	s.edgesMu.Unlock()
}

func (s *Service) ReadyTasks() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.loadAll()
	edges := s.loadEdges()
	done := completedSet(tasks)
	var ready []string
	for _, t := range tasks {
		id := int(t["id"].(float64))
		if st, _ := t["status"].(string); st != StatusPending {
			continue
		}
		allDone := true
		for _, e := range edges {
			if e.To == id && !done[e.From] {
				allDone = false
				break
			}
		}
		if allDone {
			sub, _ := t["subject"].(string)
			ready = append(ready, fmt.Sprintf("#%d: %s", id, sub))
		}
	}
	if len(ready) == 0 {
		return "No tasks are ready."
	}
	return "Ready tasks:\n" + strings.Join(ready, "\n")
}

func (s *Service) AddEdge(from, to int) string {
	if from == to {
		return fmt.Sprintf("Error: self-dependency #%d -> #%d is not allowed", from, to)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate both tasks exist.
	tasks := s.loadAll()
	foundFrom, foundTo := false, false
	for _, t := range tasks {
		id := int(t["id"].(float64))
		if id == from {
			foundFrom = true
		}
		if id == to {
			foundTo = true
		}
	}
	if !foundFrom {
		return fmt.Sprintf("Error: task #%d does not exist", from)
	}
	if !foundTo {
		return fmt.Sprintf("Error: task #%d does not exist", to)
	}

	edges := s.loadEdges()
	for _, e := range edges {
		if e.From == from && e.To == to {
			return fmt.Sprintf("Edge %d -> %d already exists", from, to)
		}
	}
	if hasPath(edges, to, from) {
		return fmt.Sprintf("Error: adding %d -> %d would create a cycle", from, to)
	}
	edges = append(edges, dagEdge{From: from, To: to})
	s.saveEdges(edges)
	return fmt.Sprintf("Added dependency: #%d -> #%d", from, to)
}

func (s *Service) RemoveEdge(from, to int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	edges := s.loadEdges()
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
		return fmt.Sprintf("Edge %d -> %d not found", from, to)
	}
	s.saveEdges(filtered)
	return fmt.Sprintf("Removed dependency: %d -> %d", from, to)
}

func (s *Service) ProgressSummary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.loadAll()
	if len(tasks) == 0 {
		return ""
	}
	edges := s.loadEdges()
	total, done, inProg, pending := 0, 0, 0, 0
	completed := make(map[int]bool)
	for _, t := range tasks {
		id := int(t["id"].(float64))
		total++
		switch st, _ := t["status"].(string); st {
		case StatusCompleted:
			done++
			completed[id] = true
		case StatusInProgress:
			inProg++
		default:
			pending++
		}
	}

	var nextUp []string
	for _, t := range tasks {
		id := int(t["id"].(float64))
		if st, _ := t["status"].(string); st != StatusPending {
			continue
		}
		ready := true
		for _, edge := range edges {
			if edge.To == id && !completed[edge.From] {
				ready = false
				break
			}
		}
		if ready {
			subject, _ := t["subject"].(string)
			nextUp = append(nextUp, fmt.Sprintf("#%d: %s", id, subject))
		}
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
	return summary + "</progress>"
}

func hasPath(edges []dagEdge, src, dst int) bool {
	adj := make(map[int][]int)
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	visited := make(map[int]bool)
	var dfs func(int) bool
	dfs = func(n int) bool {
		if n == dst {
			return true
		}
		if visited[n] {
			return false
		}
		visited[n] = true
		for _, next := range adj[n] {
			if dfs(next) {
				return true
			}
		}
		return false
	}
	return dfs(src)
}
