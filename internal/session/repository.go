// Package session defines SessionState (data) and SessionRepository (persistence).
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go-code-agent/internal/store"
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
	StatusActive      = "active"
	StatusArchived    = "archived"
	maxSessionIDBytes = 128
)

var (
	// ErrSessionNotFound indicates that the requested session has no metadata.
	ErrSessionNotFound = errors.New("session not found")
	// ErrInvalidSessionID indicates an unsafe or malformed session identifier.
	ErrInvalidSessionID = errors.New("invalid session ID")
	// ErrInvalidSessionMetadata indicates corrupt or mismatched meta.json data.
	ErrInvalidSessionMetadata = errors.New("invalid session metadata")
)

func NewSessionID() string {
	now := time.Now().UTC()
	return fmt.Sprintf("%s-%03d%04d",
		now.Format("20060102T150405"),
		now.Nanosecond()/int(time.Millisecond),
		now.UnixNano()%10000,
	)
}

// ValidateSessionID verifies that id is one safe path component.
func ValidateSessionID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: ID is empty", ErrInvalidSessionID)
	}
	if len(id) > maxSessionIDBytes {
		return fmt.Errorf("%w %q: maximum length is %d bytes", ErrInvalidSessionID, id, maxSessionIDBytes)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("%w %q: reserved path component", ErrInvalidSessionID, id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			continue
		}
		return fmt.Errorf("%w %q: only letters, digits, '.', '-' and '_' are allowed", ErrInvalidSessionID, id)
	}
	return nil
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

func (r *Repository) sessionDir(id string) (string, error) {
	if err := ValidateSessionID(id); err != nil {
		return "", err
	}
	root := filepath.Clean(r.sessionsRoot())
	dir := filepath.Join(root, id)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w %q: path escapes sessions directory", ErrInvalidSessionID, id)
	}
	return dir, nil
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadIndexLocked()
}

func (r *Repository) loadIndexLocked() (*sessionsIndex, error) {
	data, err := os.ReadFile(r.indexPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &sessionsIndex{}, nil
		}
		return nil, fmt.Errorf("read sessions index: %w", err)
	}
	var idx sessionsIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("decode sessions index: %w", err)
	}
	if idx.ActiveID != "" {
		if err := ValidateSessionID(idx.ActiveID); err != nil {
			return nil, fmt.Errorf("invalid active session in index: %w", err)
		}
	}

	// Reconcile valid on-disk sessions that are missing from the index.
	root := r.sessionsRoot()
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read sessions directory: %w", err)
	}
	known := make(map[string]bool)
	for _, st := range idx.Sessions {
		if err := ValidateSessionID(st.ID); err != nil {
			return nil, fmt.Errorf("invalid session in index: %w", err)
		}
		known[st.ID] = true
	}
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || known[id] || ValidateSessionID(id) != nil {
			continue
		}
		st, err := r.LoadSessionMeta(id)
		if err != nil {
			continue
		}
		idx.Sessions = append(idx.Sessions, *st)
	}
	return &idx, nil
}

// SaveIndex persists the sessions index.
func (r *Repository) SaveIndex(idx *sessionsIndex) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saveIndexLocked(idx)
}

func (r *Repository) saveIndexLocked(idx *sessionsIndex) error {
	if idx == nil {
		return errors.New("sessions index is nil")
	}
	if idx.ActiveID != "" {
		if err := ValidateSessionID(idx.ActiveID); err != nil {
			return fmt.Errorf("invalid active session in index: %w", err)
		}
	}
	for _, st := range idx.Sessions {
		if err := ValidateSessionID(st.ID); err != nil {
			return fmt.Errorf("invalid session in index: %w", err)
		}
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sessions index: %w", err)
	}
	return store.AtomicWritePrivate(r.indexPath(), data)
}

// CreateSession creates a new session directory and meta.json.
func (r *Repository) CreateSession(st *State) error {
	if st == nil {
		return errors.New("session state is nil")
	}
	dir, err := r.sessionDir(st.ID)
	if err != nil {
		return err
	}
	st.CreatedAt = time.Now().Unix()
	st.UpdatedAt = st.CreatedAt
	if err := store.EnsurePrivateDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session %q metadata: %w", st.ID, err)
	}
	return store.AtomicWritePrivate(filepath.Join(dir, "meta.json"), data)
}

// EnsureSessionDir creates the session directory (and sessions root) if
// missing, and tightens permissions of pre-existing session dirs.
// Safe to call for both new and resumed sessions.
func (r *Repository) EnsureSessionDir(id string) error {
	dir, err := r.sessionDir(id)
	if err != nil {
		return err
	}
	return store.EnsurePrivateDir(dir)
}

// LoadSessionMeta reads and validates meta.json for a session.
func (r *Repository) LoadSessionMeta(id string) (*State, error) {
	dir, err := r.sessionDir(id)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("session %q: %w", id, ErrSessionNotFound)
		}
		return nil, fmt.Errorf("read session %q metadata: %w", id, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%w for session %q: %v", ErrInvalidSessionMetadata, id, err)
	}
	if st.ID != id {
		return nil, fmt.Errorf("%w for session %q: metadata ID is %q", ErrInvalidSessionMetadata, id, st.ID)
	}
	return &st, nil
}

// SaveSessionMeta persists a session's meta.json.
func (r *Repository) SaveSessionMeta(st *State) error {
	if st == nil {
		return errors.New("session state is nil")
	}
	dir, err := r.sessionDir(st.ID)
	if err != nil {
		return err
	}
	st.UpdatedAt = time.Now().Unix()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session %q metadata: %w", st.ID, err)
	}
	return store.AtomicWritePrivate(filepath.Join(dir, "meta.json"), data)
}

// SessionDir returns the validated on-disk directory for a session.
func (r *Repository) SessionDir(id string) (string, error) {
	return r.sessionDir(id)
}

// DataDir returns the per-project state root.
func (r *Repository) DataDir() string {
	return r.dataDir
}

// SwitchActive validates and sets the active session to id.
func (r *Repository) SwitchActive(id string) error {
	if err := ValidateSessionID(id); err != nil {
		return err
	}
	idx, err := r.LoadIndex()
	if err != nil {
		return err
	}
	found := false
	for _, st := range idx.Sessions {
		if st.ID == id {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("session %q: %w", id, ErrSessionNotFound)
	}
	if _, err := r.LoadSessionMeta(id); err != nil {
		return fmt.Errorf("cannot activate session %q: %w", id, err)
	}
	idx.ActiveID = id
	if err := r.SaveIndex(idx); err != nil {
		return fmt.Errorf("switch session %q: %w", id, err)
	}
	return nil
}

// RenameSession sets a session's title.
func (r *Repository) RenameSession(sessionID, title string) error {
	st, err := r.LoadSessionMeta(sessionID)
	if err != nil {
		return fmt.Errorf("load session %q: %w", sessionID, err)
	}
	st.Title = title
	if err := r.SaveSessionMeta(st); err != nil {
		return fmt.Errorf("save session %q metadata: %w", sessionID, err)
	}
	idx, err := r.LoadIndex()
	if err != nil {
		return fmt.Errorf("load sessions index: %w", err)
	}
	for i := range idx.Sessions {
		if idx.Sessions[i].ID == sessionID {
			idx.Sessions[i].Title = title
			if err := r.SaveIndex(idx); err != nil {
				return fmt.Errorf("update session index: %w", err)
			}
			break
		}
	}
	return nil
}

// ArchiveSession marks a session as archived.
func (r *Repository) ArchiveSession(id string) error {
	st, err := r.LoadSessionMeta(id)
	if err != nil {
		return fmt.Errorf("load session %q: %w", id, err)
	}
	st.Status = StatusArchived
	if err := r.SaveSessionMeta(st); err != nil {
		return fmt.Errorf("save session %q metadata: %w", id, err)
	}
	idx, err := r.LoadIndex()
	if err != nil {
		return fmt.Errorf("load sessions index: %w", err)
	}
	for i := range idx.Sessions {
		if idx.Sessions[i].ID == id {
			idx.Sessions[i].Status = StatusArchived
			break
		}
	}
	if idx.ActiveID == id {
		idx.ActiveID = ""
	}
	if err := r.SaveIndex(idx); err != nil {
		return fmt.Errorf("update session index: %w", err)
	}
	return nil
}

// MarkMemorySaved records that a session has already been distilled into
// long-term memory so later startups skip it.
func (r *Repository) MarkMemorySaved(id string) error {
	st, err := r.LoadSessionMeta(id)
	if err != nil {
		return fmt.Errorf("load session %q: %w", id, err)
	}
	if st.MemorySaved {
		return nil
	}
	st.MemorySaved = true
	if err := r.SaveSessionMeta(st); err != nil {
		return fmt.Errorf("save session %q metadata: %w", id, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, err := r.loadIndexLocked()
	if err != nil {
		return fmt.Errorf("load sessions index: %w", err)
	}
	for i := range idx.Sessions {
		if idx.Sessions[i].ID == id {
			idx.Sessions[i].MemorySaved = true
			if err := r.saveIndexLocked(idx); err != nil {
				return fmt.Errorf("update session index: %w", err)
			}
			break
		}
	}
	return nil
}

// ListBackfillCandidates returns non-archived, unsaved sessions excluding
// activeID. Metadata is loaded from disk so MemorySaved/Status stay accurate.
func (r *Repository) ListBackfillCandidates(activeID string) ([]State, error) {
	idx, err := r.LoadIndex()
	if err != nil {
		return nil, err
	}
	var out []State
	for _, entry := range idx.Sessions {
		if entry.ID == "" || entry.ID == activeID {
			continue
		}
		st, err := r.LoadSessionMeta(entry.ID)
		if err != nil {
			continue
		}
		if st.Status == StatusArchived || st.MemorySaved {
			continue
		}
		out = append(out, *st)
	}
	return out, nil
}
