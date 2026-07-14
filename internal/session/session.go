package session

import (
	"encoding/json"
	"fmt"
	"go-code-agent/internal/background"
	"go-code-agent/internal/history"
	"go-code-agent/internal/task"
	"go-code-agent/internal/team"
	"os"
	"path/filepath"
	"time"
)

// BashValidator is the function signature for bash command validation.
// Injected from cmd layer to avoid importing security.go internals.
type BashValidator = background.BashValidator

// Session represents a unit of continuous work with its own task board,
// DAG, teammates, inbox, conversation history, and transcripts.
//
// Stored at: {dataDir}/sessions/<id>/  (dataDir is the resolved
// per-project state directory, normally under the user-level config
// dir rather than inside the project workdir).
// Workdir-global resources (skills, prompts, memory, MCP) are shared across sessions.
// Lifecycle managed by SessionManager.

const (
	sessionsSubDirName = "sessions"
	sessionMetaFile    = "meta.json"

	// Per-session subdirectories.
	sessionTasksDir       = "tasks"
	sessionTeamDir        = "team"
	sessionHistoryDir     = "history"
	SessionTranscriptsDir = "transcripts"

	// SessionDecisionsFile holds the append-only JSONL timeline of the
	// agent's autonomous decisions (planning, compaction, memory, judge,
	// reflection) for after-the-fact replay via /decisions.
	SessionDecisionsFile = "decisions.jsonl"

	// Session statuses tracked in meta.json / sessions.json.
	StatusActive   = "active"
	StatusArchived = "archived"
)

// sessionMeta is the on-disk shape of meta.json plus sessions.json entries.
type sessionMeta struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	MemorySaved bool   `json:"memory_saved"`
}

// Session bundles per-session subsystems. Workdir-global resources stay on AppContext.
type Session struct {
	meta    sessionMeta
	dir     string // {dataDir}/sessions/<id>
	workdir string // the workspace root (project; used by bash/file tools)
	dataDir string // per-project state root (sessions, etc.)

	// Per-session subsystems.
	Todo      *task.TodoManager
	TaskMgr   *task.TaskManager
	DagSched  *task.DAGScheduler
	BgMgr     *background.BackgroundManager
	Bus       *team.MessageBus
	Protocols *team.ProtocolStore
	History   *history.HistoryStore

	// Avoid calling SaveToMemory more than once per session lifetime.
	// Persisted in meta.json so a restart won't re-summarize.
}

// Path helpers.

// sessionsRoot returns the parent dir holding all sessions for a
// dataDir (the resolved per-project state directory).
func sessionsRoot(dataDir string) string {
	return filepath.Join(dataDir, sessionsSubDirName)
}

// sessionDir returns the on-disk path for a given session id.
func sessionDir(dataDir, id string) string {
	return filepath.Join(sessionsRoot(dataDir), id)
}

// newSessionID generates a lexicographically sortable id with millisecond
// precision.
func newSessionID() string {
	now := time.Now().UTC()
	return fmt.Sprintf("%s-%03d%04d",
		now.Format("20060102T150405"),
		now.Nanosecond()/int(time.Millisecond),
		now.UnixNano()%10000,
	)
}

// initSubSystem instantiates all per-session managers. Idempotent.
func (s *Session) initSubSystem(bashValidator BashValidator) error {
	tasksDir := filepath.Join(s.dir, sessionTasksDir)
	teamDir := filepath.Join(s.dir, sessionTeamDir)
	historyDir := filepath.Join(s.dir, sessionHistoryDir)

	for _, d := range []string{tasksDir, teamDir, historyDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	s.Todo = &task.TodoManager{}
	s.TaskMgr = task.NewTaskManager(tasksDir)
	s.DagSched = task.NewDAGScheduler(tasksDir, s.TaskMgr)
	s.TaskMgr.SetScheduler(s.DagSched)
	s.BgMgr = background.NewBgMgr(s.workdir, bashValidator)
	s.Bus = team.NewBus(filepath.Join(teamDir, "inbox"))
	s.Protocols = team.NewProtocolStore(teamDir)

	hs, err := history.NewHistoryStoreAt(filepath.Join(historyDir, history.HistoryFileName))
	if err != nil {
		return fmt.Errorf("open history: %w", err)
	}
	s.History = hs
	return nil
}

// TasksDir returns the per-session tasks directory path.
// Used by cmd layer to construct TeammateManager.
func (s *Session) TasksDir() string {
	return filepath.Join(s.dir, sessionTasksDir)
}

// TeamDir returns the per-session team directory path.
// Used by cmd layer to construct TeammateManager.
func (s *Session) TeamDir() string {
	return filepath.Join(s.dir, sessionTeamDir)
}

// saveMeta persists the session's meta.json.
func (s *Session) saveMeta() error {
	s.meta.UpdatedAt = time.Now().Unix()
	data, _ := json.MarshalIndent(s.meta, "", "  ")
	return os.WriteFile(filepath.Join(s.dir, sessionMetaFile), data, 0o644)
}

// Public accessors.

// ID returns the session's immutable identifier.
func (s *Session) ID() string { return s.meta.ID }

// Title returns the session's human-readable title.
func (s *Session) Title() string { return s.meta.Title }

// Status returns the session's lifecycle status.
func (s *Session) Status() string { return s.meta.Status }

// Dir returns the session's root directory on disk.
func (s *Session) Dir() string { return s.dir }

// SetTitle updates the title and persists meta.json.
func (s *Session) SetTitle(title string) error {
	s.meta.Title = title
	return s.saveMeta()
}

// SetStatus updates the session's lifecycle status.
func (s *Session) SetStatus(status string) error {
	s.meta.Status = status
	return s.saveMeta()
}
