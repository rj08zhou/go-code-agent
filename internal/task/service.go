// Package task provides persistent task CRUD and DAG scheduling.
package task

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/store"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	StatusPending    = "pending"
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
	StatusDeleted    = "deleted"

	// legacyBatchID owns tasks written before batches existed, plus anything
	// created through the batch-less Create API.
	legacyBatchID = "legacy"
	// sealedKey marks a task whose batch no longer accepts new members.
	sealedKey = "batch_sealed"
	// maxReportedBatches caps how many batches ProgressSummary spells out, so
	// a long-lived project cannot grow the per-turn prompt without bound.
	maxReportedBatches = 3
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
	_ = store.EnsurePrivateDir(dir)
	return &Service{dir: dir}
}

// NewBatchID returns an opaque identifier for one agent request's DAG.
// It is intentionally independent from model trace IDs: traces describe
// observability, while batches describe task ownership and lifecycle.
func NewBatchID(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = "batch"
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err == nil {
		return fmt.Sprintf("%s-%x", prefix, random)
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
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

func (s *Service) saveEdges(edges []dagEdge) error {
	data, err := json.MarshalIndent(edges, "", "  ")
	if err != nil {
		return err
	}
	if err := store.AtomicWritePrivate(s.edgesPath(), data); err != nil {
		return err
	}
	s.edgesMu.Lock()
	s.edges = make([]dagEdge, len(edges))
	copy(s.edges, edges)
	s.edgesLoaded = true
	s.edgesMu.Unlock()
	return nil
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

func (s *Service) save(t map[string]any) error {
	id := int(t["id"].(float64))
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := store.AtomicWritePrivate(s.taskPath(id), data); err != nil {
		return err
	}
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
	return nil
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

// Create preserves the legacy single-board API. New runners should use
// CreateForBatch so independent requests never merge their DAGs.
func (s *Service) Create(subject, desc string, dependsOn []int) string {
	return s.CreateForBatch(legacyBatchID, subject, desc, dependsOn)
}

// CreateForBatch adds a task to one persistent DAG batch. Dependencies must
// reference tasks in that same batch; task IDs remain session-global.
func (s *Service) CreateForBatch(batchID, subject, desc string, dependsOn []int) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "Error: subject is required"
	}
	batchID = strings.TrimSpace(batchID)
	if batchID == "" {
		batchID = legacyBatchID
	}
	s.mu.Lock()
	id := s.nextID()
	t := map[string]any{
		"id": float64(id), "subject": subject, "description": desc,
		"status": StatusPending, "owner": nil, "batch_id": batchID,
	}
	if err := s.save(t); err != nil {
		s.mu.Unlock()
		return fmt.Sprintf("Error: %v", err)
	}
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
			dependency, err := s.load(dep)
			if err != nil || taskBatchID(dependency) != batchID {
				warn = append(warn, fmt.Sprintf("dependency #%d belongs to another batch, skipped", dep))
				continue
			}
			edges = append(edges, dagEdge{From: dep, To: id})
		}
		if err := s.saveEdges(edges); err != nil {
			s.mu.Unlock()
			return fmt.Sprintf("Error: created task #%d but failed to save dependencies: %v", id, err)
		}
		s.mu.Unlock()
		if len(warn) > 0 {
			return fmt.Sprintf("Created task #%d in %s: %s\n[WARN] %s", id, batchID, subject, strings.Join(warn, "; "))
		}
	}

	return fmt.Sprintf("Created task #%d in %s: %s", id, batchID, subject)
}

func taskBatchID(t map[string]any) string {
	if batchID, _ := t["batch_id"].(string); batchID != "" {
		return batchID
	}
	return legacyBatchID
}

func batchSealed(t map[string]any) bool {
	sealed, _ := t[sealedKey].(bool)
	return sealed
}

// ResolveActiveBatch answers "which batch does a new task belong to" for
// callers that carry no explicit batch. A follow-up turn on the same request
// keeps landing in the batch it started, and once that work is finished (or
// sealed) the next task opens a fresh one.
func (s *Service) ResolveActiveBatch(prefix string) string {
	if batchID := s.activeBatch(); batchID != "" {
		return batchID
	}
	return NewBatchID(prefix)
}

// StartNewBatch seals the active batch and returns a fresh ID, so an
// unrelated request never appends to a plan that was abandoned half-done.
func (s *Service) StartNewBatch(prefix string) string {
	s.sealBatch(s.activeBatch())
	return NewBatchID(prefix)
}

// SealActiveBatch stops the active batch from absorbing new tasks. Its tasks
// stay on the board and keep appearing in listings and progress summaries.
func (s *Service) SealActiveBatch() string {
	batchID := s.activeBatch()
	if batchID == "" {
		return "No active DAG batch."
	}
	remaining := s.sealBatch(batchID)
	return fmt.Sprintf("Sealed batch %s; %d unfinished task(s) kept on the board.",
		displayBatchID(batchID), remaining)
}

// activeBatch is the unsealed batch owning the newest unfinished task.
func (s *Service) activeBatch() string {
	tasks := s.loadAll()
	sealed := make(map[string]bool)
	for _, t := range tasks {
		if batchSealed(t) {
			sealed[taskBatchID(t)] = true
		}
	}
	// loadAll is ordered by ascending ID, so walking back finds the most
	// recently created unfinished task first.
	for i := len(tasks) - 1; i >= 0; i-- {
		switch st, _ := tasks[i]["status"].(string); st {
		case StatusDeleted, StatusCompleted:
			continue
		}
		if batchID := taskBatchID(tasks[i]); !sealed[batchID] {
			return batchID
		}
	}
	return ""
}

// sealBatch marks every live task in batchID sealed and reports how many of
// them are still unfinished.
func (s *Service) sealBatch(batchID string) int {
	if batchID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	remaining := 0
	for _, t := range s.loadAll() {
		st, _ := t["status"].(string)
		if st == StatusDeleted || taskBatchID(t) != batchID {
			continue
		}
		if !batchSealed(t) {
			t[sealedKey] = true
			if err := s.save(t); err != nil {
				continue
			}
		}
		if st != StatusCompleted {
			remaining++
		}
	}
	return remaining
}

// BatchCounts reports the live task count of batchID and the number of edges
// with both endpoints inside it.
func (s *Service) BatchCounts(batchID string) (tasks, edges int) {
	if batchID == "" {
		return 0, 0
	}
	inBatch := make(map[int]bool)
	for _, t := range s.loadAll() {
		if st, _ := t["status"].(string); st == StatusDeleted {
			continue
		}
		if taskBatchID(t) != batchID {
			continue
		}
		inBatch[int(t["id"].(float64))] = true
	}
	for _, e := range s.loadEdges() {
		if inBatch[e.From] && inBatch[e.To] {
			edges++
		}
	}
	return len(inBatch), edges
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
		if err := s.saveEdges(filtered); err != nil {
			s.mu.Unlock()
			return fmt.Sprintf("Error: task %d deleted but failed to update edges: %v", id, err)
		}
		s.mu.Unlock()
		return fmt.Sprintf("Task %d deleted", id)
	}
	t["status"] = status
	if err := s.save(t); err != nil {
		s.mu.Unlock()
		return fmt.Sprintf("Error: %v", err)
	}
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
		lines = append(lines, fmt.Sprintf("%s #%d [%s]: %s%s", mk, id, displayBatchID(taskBatchID(t)), sub, depStr))
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
	if err := s.save(t); err != nil {
		return fmt.Sprintf("Error: %v", err), false
	}
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

// TaskCount is every task on record, deleted ones included. Callers deciding
// what to show or gate on almost always want ActiveTaskCount or BatchCounts.
func (s *Service) TaskCount() int {
	s.ensureCache()
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	return len(s.cache)
}

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
	byBatch := make(map[string][]string)
	var batchOrder []string
	for _, t := range tasks {
		st, _ := t["status"].(string)
		if st == StatusDeleted {
			continue
		}
		batchID := taskBatchID(t)
		if _, seen := byBatch[batchID]; !seen {
			batchOrder = append(batchOrder, batchID)
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
		byBatch[batchID] = append(byBatch[batchID], fmt.Sprintf("%s #%d: %s%s", mk, id, sub, depStr))
	}
	if len(batchOrder) == 0 {
		return "No tasks."
	}
	var blocks []string
	for _, batchID := range batchOrder {
		blocks = append(blocks, "DAG "+displayBatchID(batchID)+":\n"+strings.Join(byBatch[batchID], "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

func displayBatchID(batchID string) string {
	if len(batchID) > 24 {
		return batchID[:24] + "…"
	}
	return batchID
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
			if err := s.save(t); err != nil {
				return fmt.Sprintf("Error: cleared %d then failed: %v", count, err)
			}
			count++
		}
	}
	if count == 0 {
		return "No completed tasks to clear."
	}
	return fmt.Sprintf("Cleared %d completed task(s).", count)
}

// Reset deletes all task files and the edges file. Returns a short summary.
func (s *Service) Reset() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := s.loadAll()
	count := 0
	for _, t := range tasks {
		id := int(t["id"].(float64))
		_ = os.Remove(s.taskPath(id))
		count++
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
	if count == 0 {
		return "No tasks to clear."
	}
	return fmt.Sprintf("Cleared %d task(s) and DAG edges.", count)
}

// ActiveTaskCount is the number of non-deleted tasks on the board.
func (s *Service) ActiveTaskCount() int {
	s.ensureCache()
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	n := 0
	for _, t := range s.cache {
		if st, _ := t["status"].(string); st != StatusDeleted {
			n++
		}
	}
	return n
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
	fromBatch, toBatch := "", ""
	for _, t := range tasks {
		id := int(t["id"].(float64))
		if id == from {
			foundFrom = true
			fromBatch = taskBatchID(t)
		}
		if id == to {
			foundTo = true
			toBatch = taskBatchID(t)
		}
	}
	if !foundFrom {
		return fmt.Sprintf("Error: task #%d does not exist", from)
	}
	if !foundTo {
		return fmt.Sprintf("Error: task #%d does not exist", to)
	}
	if fromBatch != toBatch {
		return fmt.Sprintf("Error: tasks #%d and #%d belong to different DAG batches", from, to)
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
	if err := s.saveEdges(edges); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
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
	if err := s.saveEdges(filtered); err != nil {
		return fmt.Sprintf("Error: %v", err)
	}
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
	type batchProgress struct {
		total, done, inProgress, pending int
		newestTask                       int
		sealed                           bool
		completed                        map[int]bool
		nextUp                           []string
	}
	batches := make(map[string]*batchProgress)
	var batchOrder []string
	for _, t := range tasks {
		if st, _ := t["status"].(string); st == StatusDeleted {
			continue
		}
		batchID := taskBatchID(t)
		progress := batches[batchID]
		if progress == nil {
			progress = &batchProgress{completed: make(map[int]bool)}
			batches[batchID] = progress
			batchOrder = append(batchOrder, batchID)
		}
		id := int(t["id"].(float64))
		if id > progress.newestTask {
			progress.newestTask = id
		}
		progress.total++
		progress.sealed = progress.sealed || batchSealed(t)
		switch st, _ := t["status"].(string); st {
		case StatusCompleted:
			progress.done++
			progress.completed[id] = true
		case StatusInProgress:
			progress.inProgress++
		default:
			progress.pending++
		}
	}

	for _, t := range tasks {
		batchID := taskBatchID(t)
		progress := batches[batchID]
		if progress == nil {
			continue
		}
		id := int(t["id"].(float64))
		if st, _ := t["status"].(string); st != StatusPending {
			continue
		}
		ready := true
		for _, edge := range edges {
			if edge.To == id && !progress.completed[edge.From] {
				ready = false
				break
			}
		}
		if ready {
			subject, _ := t["subject"].(string)
			progress.nextUp = append(progress.nextUp, fmt.Sprintf("#%d: %s", id, subject))
		}
	}

	if len(batchOrder) == 0 {
		return ""
	}
	// Order by newest task so the tail of the list is what the session is
	// working on now, which is what survives the cap below.
	sort.Slice(batchOrder, func(i, j int) bool {
		return batches[batchOrder[i]].newestTask < batches[batchOrder[j]].newestTask
	})
	currentBatch := batchOrder[len(batchOrder)-1]

	var summaries []string
	for _, batchID := range batchOrder {
		progress := batches[batchID]
		finished := progress.done == progress.total
		// A finished batch is still worth reporting while it is the one in
		// hand, so the model sees its plan close out.
		if finished && batchID != currentBatch {
			continue
		}
		label := displayBatchID(batchID)
		if progress.sealed {
			label += " (sealed)"
		}
		summary := fmt.Sprintf("%s: %d/%d tasks completed", label, progress.done, progress.total)
		if progress.inProgress > 0 {
			summary += fmt.Sprintf(", %d in progress", progress.inProgress)
		}
		switch {
		case finished:
			summary += ". All tasks done!"
		case len(progress.nextUp) > 0:
			summary += ". Next ready: " + strings.Join(progress.nextUp, ", ")
		case progress.pending > 0:
			summary += ". Remaining tasks are blocked by dependencies."
		}
		summaries = append(summaries, summary)
	}
	if len(summaries) == 0 {
		return ""
	}
	// batchOrder is oldest-first, so the tail holds what the session is
	// actually working on. Older leftovers are counted rather than listed,
	// otherwise every abandoned plan would grow the prompt forever.
	header := "Task batches:"
	if omitted := len(summaries) - maxReportedBatches; omitted > 0 {
		summaries = summaries[omitted:]
		header = fmt.Sprintf("Task batches (%d older unfinished batch(es) omitted, see task_dag):", omitted)
	}
	return "<task-batches>" + header + "\n" + strings.Join(summaries, "\n") + "</task-batches>"
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
