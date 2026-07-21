// Package session defines SessionState (data) and SessionRepository (persistence).
package session

import (
	"encoding/json"
	"fmt"
	"go-code-agent-refactor/internal/store"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// State holds only persistent business data — no runtime resources.
type State struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	MemorySaved bool   `json:"memory_saved"`
}

const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

func NewSessionID() string {
	now := time.Now().UTC()
	return fmt.Sprintf("%s-%03d%04d",
		now.Format("20060102T150405"),
		now.Nanosecond()/int(time.Millisecond),
		now.UnixNano()%10000,
	)
}

// Repository manages session index and per-session metadata.
// All paths are under {dataDir}/sessions/.
type Repository struct {
	dataDir string
	mu      sync.Mutex
}

func NewRepository(dataDir string) *Repository {
	return &Repository{dataDir: dataDir}
}

func (r *Repository) sessionsRoot() string {
	return filepath.Join(r.dataDir, "sessions")
}

func (r *Repository) sessionDir(id string) string {
	return filepath.Join(r.sessionsRoot(), id)
}

func (r *Repository) indexPath() string {
	return filepath.Join(r.dataDir, "sessions.json")
}

type sessionsIndex struct {
	ActiveID string  `json:"active_id"`
	Sessions []State `json:"sessions"`
}

// LoadIndex reads the sessions index from disk.
func (r *Repository) LoadIndex() (*sessionsIndex, error) {
	data, err := os.ReadFile(r.indexPath())
	if err != nil {
		return &sessionsIndex{}, nil
	}
	var idx sessionsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return &sessionsIndex{}, nil
	}
	// Reconcile with on-disk directories
	root := r.sessionsRoot()
	entries, _ := os.ReadDir(root)
	known := make(map[string]bool)
	for _, s := range idx.Sessions {
		known[s.ID] = true
	}
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		metaPath := filepath.Join(root, e.Name(), "meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m State
		if json.Unmarshal(data, &m) != nil || m.ID == "" {
			continue
		}
		idx.Sessions = append(idx.Sessions, m)
	}
	return &idx, nil
}

// SaveIndex persists the sessions index.
func (r *Repository) SaveIndex(idx *sessionsIndex) error {
	data, _ := json.MarshalIndent(idx, "", "  ")
	return store.AtomicWrite(r.indexPath(), data)
}

// CreateSession creates a new session directory and meta.json.
func (r *Repository) CreateSession(st *State) error {
	st.CreatedAt = time.Now().Unix()
	st.UpdatedAt = st.CreatedAt
	dir := r.sessionDir(st.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	return store.AtomicWrite(filepath.Join(dir, "meta.json"), data)
}

// LoadSessionMeta reads meta.json for a session.
func (r *Repository) LoadSessionMeta(id string) (*State, error) {
	path := filepath.Join(r.sessionDir(id), "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// SaveSessionMeta persists a session's meta.json.
func (r *Repository) SaveSessionMeta(st *State) error {
	st.UpdatedAt = time.Now().Unix()
	data, _ := json.MarshalIndent(st, "", "  ")
	return store.AtomicWrite(filepath.Join(r.sessionDir(st.ID), "meta.json"), data)
}

// SessionDir returns the on-disk directory for a session.
func (r *Repository) SessionDir(id string) string {
	return r.sessionDir(id)
}

// DataDir returns the per-project state root.
func (r *Repository) DataDir() string {
	return r.dataDir
}

// SwitchActive sets the active session to id. Returns "" on success, error on fail.
func (r *Repository) SwitchActive(id string) string {
	idx, _ := r.LoadIndex()
	found := false
	for _, s := range idx.Sessions {
		if s.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("Session '%s' not found", id)
	}
	idx.ActiveID = id
	if err := r.SaveIndex(idx); err != nil {
		return fmt.Sprintf("Failed to switch: %v", err)
	}
	return ""
}

// RenameSession sets the title of the active session.
func (r *Repository) RenameSession(sessionID, title string) string {
	st, err := r.LoadSessionMeta(sessionID)
	if err != nil {
		return fmt.Sprintf("Session not found: %v", err)
	}
	st.Title = title
	if err := r.SaveSessionMeta(st); err != nil {
		return fmt.Sprintf("Failed to rename: %v", err)
	}
	idx, _ := r.LoadIndex()
	for i := range idx.Sessions {
		if idx.Sessions[i].ID == sessionID {
			idx.Sessions[i].Title = title
			_ = r.SaveIndex(idx)
			break
		}
	}
	return ""
}

// ArchiveSession marks a session as archived.
func (r *Repository) ArchiveSession(id string) string {
	st, err := r.LoadSessionMeta(id)
	if err != nil {
		return fmt.Sprintf("Session not found: %v", err)
	}
	st.Status = StatusArchived
	if err := r.SaveSessionMeta(st); err != nil {
		return fmt.Sprintf("Failed to archive: %v", err)
	}
	idx, _ := r.LoadIndex()
	if idx.ActiveID == id {
		idx.ActiveID = ""
		_ = r.SaveIndex(idx)
	}
	return ""
}

// ListSessions returns a formatted list of all sessions.
func (r *Repository) ListSessions() string {
	idx, _ := r.LoadIndex()
	if len(idx.Sessions) == 0 {
		return "No sessions."
	}
	var sb strings.Builder
	for _, s := range idx.Sessions {
		marker := " "
		if s.ID == idx.ActiveID {
			marker = "*"
		}
		fmt.Fprintf(&sb, " %s %s  %-16s %s\n", marker, s.ID[:24], s.Status, s.Title)
	}
	return sb.String()
}
