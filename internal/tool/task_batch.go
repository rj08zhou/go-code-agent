package tool

import "sync"

// TaskBatchRef holds the persistent DAG batch that one agent run is building.
//
// The runner leaves it unresolved so the first task_create can pick up whichever
// batch the request is already using, rather than minting one per turn and
// fragmenting a multi-turn plan. It is a pointer shared by every tool call in
// the run because the executor hands each handler its own copy of ToolScope,
// which would otherwise discard the resolution.
type TaskBatchRef struct {
	mu sync.Mutex
	id string
}

// NewTaskBatch returns a reference pinned to id, or unresolved when id is empty.
func NewTaskBatch(id string) *TaskBatchRef { return &TaskBatchRef{id: id} }

// ID is the resolved batch, empty while the run has created no tasks.
func (b *TaskBatchRef) ID() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.id
}

// Resolve returns the batch in use, calling pick once if there is none yet.
func (b *TaskBatchRef) Resolve(pick func() string) string {
	if b == nil {
		return pick()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.id == "" {
		b.id = pick()
	}
	return b.id
}

// Restart abandons the current batch and adopts whatever pick returns, for a
// request unrelated to the tasks already on the board.
func (b *TaskBatchRef) Restart(pick func() string) string {
	if b == nil {
		return pick()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.id = pick()
	return b.id
}
