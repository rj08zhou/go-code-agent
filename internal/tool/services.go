package tool

import "context"

// TaskService is the interface for task CRUD.
type TaskService interface {
	Create(subject, desc string, deps []int) string
	Get(id int) string
	Update(id int, status string) string
	ListAll() string
	Claim(id int, owner string) (string, bool)
	AddEdge(from, to int) string
	RemoveEdge(from, to int) string
	ReadyTasks() string
	TopoView() string
	ProgressSummary() string
	ClearCompleted() string
	Reset()
}

// TodoService manages the current model-facing checklist separately from
// persistent dependency-tracked tasks.
type TodoService interface {
	Update(items []map[string]string) (string, error)
	Render() string
	HasOpenItems() bool
}

// MemoryService is the interface for memory persistence.
type MemoryService interface {
	Write(content, category string) string
	Search(query string, topK, withinDays int, category string) string
	Delete(query, category string) string
	Stats() string
	SaveSessionMemory(sessionID, summary string) string
}

// SkillLoader loads skill definitions.
type SkillLoader interface {
	Load(name string) string
}

// TeamService manages teammate lifecycle.
type TeamService interface {
	Spawn(ctx context.Context, name, role, prompt string) string
	ListAll() string
}

// MessageBus is the interface for inter-agent messaging.
type MessageBus interface {
	Send(from, to, content, msgType string, meta map[string]any) string
	ReadInbox(id string) []map[string]any
	Broadcast(from, content string, recipients []string) string
}

// TeamProtocolService exposes the durable multi-agent approval protocol to tools.
type TeamProtocolService interface {
	ShutdownRequest(teammate string) string
	SubmitPlan(agent, plan string) string
	ReviewPlan(requestID string, approve bool, feedback string) string
}

// DiffPreview generates a preview before a mutating file operation.
type DiffPreview interface {
	Preview(path string, content []byte) (string, error)
	PreviewDelete(path string) (string, error)
}

// BackgroundService is the interface for background job management.
type BackgroundService interface {
	Run(sessionID, command string, timeout int) string
	Check(taskID string) string
}

// SubagentService executes an isolated sub-/explore agent investigation.
type SubagentService interface {
	Run(ctx context.Context, prompt, agentType, workdir string) string
}

// WebService provides the Web capabilities exposed as Agent tools.
type WebService interface {
	Fetch(ctx context.Context, url string) (string, error)
	Search(ctx context.Context, query string) (string, error)
}
