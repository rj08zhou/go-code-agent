package session

import (
	"context"
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"go-code-agent/internal/memory"
	"go-code-agent/internal/prompt"
	"go-code-agent/utils"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SessionManager — workdir-scoped session lifecycle manager.
//
// Responsibilities: index management (sessions.json), create/load sessions,
// deactivate (SaveToMemory), list/archive/rename/switch.
// Persisted at {workdir}/.go-code-agent/sessions.json.

const sessionsIndexFile = "sessions.json"

type sessionsIndex struct {
	ActiveID string        `json:"active_id"`
	Sessions []sessionMeta `json:"sessions"`
}

// SessionManager manages all session lifecycles.
type SessionManager struct {
	workdir string
	path    string
	mu      sync.Mutex
	idx     sessionsIndex

	// Currently active loaded session
	active *Session

	// Dependencies (constructor-injected)
	model         string
	promptLoader  *prompt.Loader
	memStore      *memory.MemoryStore
	bashValidator BashValidator
}

// NewSessionManager constructs the session manager with all dependencies.
// Called once at startup from main.
func NewSessionManager(workdir string, model string, pl *prompt.Loader, ms *memory.MemoryStore, bv BashValidator) *SessionManager {
	appRoot := filepath.Join(workdir, appRootDirName)
	_ = os.MkdirAll(appRoot, 0o755)
	sm := &SessionManager{
		workdir:       workdir,
		path:          filepath.Join(appRoot, sessionsIndexFile),
		model:         model,
		promptLoader:  pl,
		memStore:      ms,
		bashValidator: bv,
	}
	sm.load()
	return sm
}

// Factory methods: create / load Session.

// NewSession creates a new session with all subsystems wired.
func (sm *SessionManager) NewSession(title string) (*Session, error) {
	id := newSessionID()
	dir := sessionDir(sm.workdir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	s := &Session{
		meta: sessionMeta{
			ID:        id,
			Title:     title,
			Status:    StatusActive,
			CreatedAt: time.Now().Unix(),
			UpdatedAt: time.Now().Unix(),
		},
		dir:     dir,
		workdir: sm.workdir,
	}
	if err := s.initSubSystem(sm.bashValidator); err != nil {
		return nil, err
	}
	if err := s.saveMeta(); err != nil {
		return nil, err
	}
	// Register in index.
	if err := sm.Register(s, true); err != nil {
		return nil, err
	}
	return s, nil
}

// LoadSession rehydrates an existing session from disk.
func (sm *SessionManager) LoadSession(id string) (*Session, error) {
	dir := sessionDir(sm.workdir, id)
	metaPath := filepath.Join(dir, sessionMetaFile)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", id, err)
	}
	var m sessionMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", metaPath, err)
	}
	s := &Session{meta: m, dir: dir, workdir: sm.workdir}
	if err := s.initSubSystem(sm.bashValidator); err != nil {
		return nil, err
	}
	return s, nil
}

// Lifecycle: Deactivate / SaveToMemory.

// Deactivate flushes lightweight state (history is already append-only,
// so nothing to flush here) and marks the session inactive. The heavy
// memory-distillation LLM call is deliberately NOT done here — it would
// block the exit path for seconds to minutes. Instead, unsaved sessions
// are picked up by BackfillMemory on the next startup.
func (sm *SessionManager) Deactivate(s *Session) {
	if s == nil {
		return
	}
}

// SaveToMemory extracts insights from conversation history to long-term memory.
func (sm *SessionManager) SaveToMemory(ctx context.Context, s *Session) (string, error) {
	if sm.memStore == nil {
		return "", fmt.Errorf("no MemStore available (deps not configured)")
	}
	if s.meta.MemorySaved {
		return "Memory already saved for this session.", nil
	}

	// Bail if this session became active while we were waiting (e.g.
	// user /session-switched into it). Reading its history now would
	// race with concurrent appends from the REPL loop.
	if sm.ActiveID() == s.ID() {
		return "session is now active, skipping backfill.", nil
	}

	// 1. Read history.
	entries, err := s.History.ReadAll()
	if err != nil {
		return "", fmt.Errorf("read history: %w", err)
	}
	if len(entries) == 0 {
		s.markMemorySaved()
		return "No history to save.", nil
	}
	// Re-check after the (slow) read: if the session became active in
	// the meantime, its history is now being mutated and the snapshot
	// we just read may be stale.
	if sm.ActiveID() == s.ID() {
		return "session became active during read, skipping backfill.", nil
	}

	// 2. Format history (last 100 entries; truncate long content).
	const maxEntries = 100
	start := 0
	if len(entries) > maxEntries {
		start = len(entries) - maxEntries
	}
	var hist strings.Builder
	for _, e := range entries[start:] {
		switch e.Kind {
		case infra.KindUser:
			fmt.Fprintf(&hist, "[user]: %s\n", utils.Truncate(e.Content, 500))
		case infra.KindAssistant:
			fmt.Fprintf(&hist, "[assistant]: %s\n", utils.Truncate(e.Content, 500))
		case infra.KindTool:
			fmt.Fprintf(&hist, "[tool_result]: %s\n", utils.Truncate(e.Content, 300))
		}
	}

	// 3. Call LLM with session_to_memory prompt.
	tmpl := ""
	if sm.promptLoader != nil {
		tmpl = sm.promptLoader.Load("session_to_memory")
	}
	if tmpl == "" {
		return "", fmt.Errorf("prompt template 'session_to_memory' not found")
	}
	promptText := prompt.Render(tmpl, map[string]string{
		"session_history": hist.String(),
	})

	// UserMessage, not SystemMessage: this is a one-shot instruction
	// with no real conversation, but some OpenAI-compatible endpoints
	// (observed with GLM/bigmodel.cn) reject a messages array that
	// contains only a system-role turn with 400 "messages 参数非法" -
	// they require at least one user message. A lone user message
	// works identically on Anthropic/OpenAI too, so this is safe
	// across all providers.
	comp, err := llm.NewClient(nil).CallWithRetry(ctx, "memory-save", llm.CallParams{
		Model:       sm.model,
		Messages:    []llm.Message{llm.UserMessage(promptText)},
		Temperature: 0.0,
	})
	if err != nil {
		return "", fmt.Errorf("LLM call failed: %w", err)
	}

	// 4. Parse response — expect a JSON array of {content, category}.
	content := strings.TrimSpace(comp.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var items []map[string]string
	if err := json.Unmarshal([]byte(content), &items); err != nil {
		return "", fmt.Errorf("parse LLM response: %w, raw=%s", err, utils.Truncate(content, 200))
	}

	if len(items) == 0 {
		s.markMemorySaved()
		return "No valuable insights found to save.", nil
	}

	// 5. Write each item to MemStore.
	saved := 0
	var summaries []string
	for _, item := range items {
		c := strings.TrimSpace(item["content"])
		cat := strings.TrimSpace(item["category"])
		if c == "" {
			continue
		}
		switch cat {
		case "preference", "lesson", "fact", "context", "change_log":
			// OK
		default:
			cat = "fact"
		}
		result := sm.memStore.WriteMemory(c, cat)
		if !strings.HasPrefix(result, "Error") {
			saved++
			summaries = append(summaries, fmt.Sprintf("[%s] %s", cat, utils.Truncate(c, 80)))
		}
	}

	s.markMemorySaved()
	return fmt.Sprintf("Saved %d insights to memory:\n- %s", saved, strings.Join(summaries, "\n- ")), nil
}

// markMemorySaved persists the memorySaved flag to meta.json so a
// restart won't re-summarize the same session.
func (s *Session) markMemorySaved() {
	s.meta.MemorySaved = true
	_ = s.saveMeta()
}

// Index management.

// load reads sessions.json and reconciles it with the on-disk session
// directories.
func (sm *SessionManager) load() {
	if data, err := os.ReadFile(sm.path); err == nil {
		_ = json.Unmarshal(data, &sm.idx)
	}

	root := sessionsRoot(sm.workdir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	known := make(map[string]bool, len(sm.idx.Sessions))
	for _, s := range sm.idx.Sessions {
		known[s.ID] = true
	}
	for _, e := range entries {
		if !e.IsDir() || known[e.Name()] {
			continue
		}
		metaPath := filepath.Join(root, e.Name(), sessionMetaFile)
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var m sessionMeta
		if json.Unmarshal(data, &m) != nil || m.ID == "" {
			continue
		}
		sm.idx.Sessions = append(sm.idx.Sessions, m)
	}
}

// saveLocked writes the current index to disk. Caller must hold sm.mu.
func (sm *SessionManager) saveLocked() error {
	data, err := json.MarshalIndent(sm.idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sm.path, data, 0o644)
}

// findIndexLocked returns the slice index of the session with the given
// id, or -1. Caller must hold sm.mu.
func (sm *SessionManager) findIndexLocked(id string) int {
	for i, s := range sm.idx.Sessions {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// ActiveID returns the id of the active session (or "" if none).
func (sm *SessionManager) ActiveID() string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.idx.ActiveID
}

// Active returns the currently-active loaded Session instance, or nil.
func (sm *SessionManager) Active() *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.active
}

// Activate sets the given session as the active one. It updates both
// the in-memory pointer and the persisted index. Callers should call
// Deactivate on the previous session before activating a new one.
func (sm *SessionManager) Activate(s *Session) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.active = s
	if s != nil {
		sm.idx.ActiveID = s.ID()
	} else {
		sm.idx.ActiveID = ""
	}
	_ = sm.saveLocked()
}

// List returns a copy of all known sessions, sorted newest-first.
func (sm *SessionManager) List() []sessionMeta {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := make([]sessionMeta, len(sm.idx.Sessions))
	copy(out, sm.idx.Sessions)
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

// Register adds a new session to the index and optionally marks it active.
func (sm *SessionManager) Register(s *Session, makeActive bool) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if sm.findIndexLocked(s.ID()) >= 0 {
		return fmt.Errorf("session %s already registered", s.ID())
	}
	sm.idx.Sessions = append(sm.idx.Sessions, s.meta)
	if makeActive {
		sm.idx.ActiveID = s.ID()
	}
	return sm.saveLocked()
}

// SetActive marks the given id as the active session.
func (sm *SessionManager) SetActive(id string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.findIndexLocked(id) < 0 {
		return fmt.Errorf("unknown session %s", id)
	}
	sm.idx.ActiveID = id
	return sm.saveLocked()
}

// UpdateMeta refreshes the stored metadata for a session.
func (sm *SessionManager) UpdateMeta(m sessionMeta) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	i := sm.findIndexLocked(m.ID)
	if i < 0 {
		return fmt.Errorf("unknown session %s", m.ID)
	}
	m.UpdatedAt = time.Now().Unix()
	sm.idx.Sessions[i] = m
	return sm.saveLocked()
}

// Touch bumps the UpdatedAt of the active session.
func (sm *SessionManager) Touch(id string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	i := sm.findIndexLocked(id)
	if i < 0 {
		return
	}
	sm.idx.Sessions[i].UpdatedAt = time.Now().Unix()
	_ = sm.saveLocked()
}

// Archive marks a session as archived.
func (sm *SessionManager) Archive(id string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	i := sm.findIndexLocked(id)
	if i < 0 {
		return fmt.Errorf("unknown session %s", id)
	}
	sm.idx.Sessions[i].Status = StatusArchived
	sm.idx.Sessions[i].UpdatedAt = time.Now().Unix()
	if sm.idx.ActiveID == id {
		sm.idx.ActiveID = ""
	}
	// Also update the session's own meta.json for consistency.
	s, err := sm.LoadSession(id)
	if err == nil {
		_ = s.SetStatus(StatusArchived)
	}
	return sm.saveLocked()
}

// Rename updates a session's title in both the index and its meta.json.
func (sm *SessionManager) Rename(id, title string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	i := sm.findIndexLocked(id)
	if i < 0 {
		return fmt.Errorf("unknown session %s", id)
	}
	sm.idx.Sessions[i].Title = title
	sm.idx.Sessions[i].UpdatedAt = time.Now().Unix()
	if s, err := sm.LoadSession(id); err == nil {
		_ = s.SetTitle(title)
	}
	return sm.saveLocked()
}

// MostRecentActiveID returns the id of the most recently-updated
// non-archived session, or "" if none exist.
func (sm *SessionManager) MostRecentActiveID() string {
	for _, s := range sm.List() {
		if s.Status != StatusArchived {
			return s.ID
		}
	}
	return ""
}

// Render produces a human-readable table for the /session REPL command.
func (sm *SessionManager) Render() string {
	sessions := sm.List()
	if len(sessions) == 0 {
		return "No sessions yet. Anything you do will be saved into a new one on first message."
	}
	active := sm.ActiveID()
	var lines []string
	lines = append(lines, fmt.Sprintf("SessionManager in %s:", filepath.Join(sm.workdir, appRootDirName)))
	for _, s := range sessions {
		marker := "  "
		if s.ID == active {
			marker = "* "
		}
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		age := time.Since(time.Unix(s.UpdatedAt, 0)).Round(time.Second)
		lines = append(lines, fmt.Sprintf("%s%s  [%s]  %s  - updated %s ago",
			marker, s.ID, s.Status, title, age))
	}
	return strings.Join(lines, "\n")
}

// Boot helper.

// BootstrapOrCreate extends BootstrapSession's resolution policy with
// the CLI's --new-session override: forceNew (with no explicit id)
// always creates a fresh session, bypassing the most-recent fallback
// below. Otherwise defers entirely to BootstrapSession's own chain
// (explicit id > most recent > fresh). Single source of truth for
// "which session do we start with" - previously this forceNew rule
// lived in main.go, split from the rest of the same policy here.
func (sm *SessionManager) BootstrapOrCreate(forceNew bool, explicitID string) (*Session, error) {
	if forceNew && explicitID == "" {
		return sm.NewSession("New session")
	}
	return sm.BootstrapSession(explicitID)
}

// BootstrapSession picks the right session to activate at startup:
// explicit id > most recent active > brand new.
func (sm *SessionManager) BootstrapSession(explicitID string) (*Session, error) {
	// 1. Explicit id.
	if explicitID != "" {
		s, err := sm.LoadSession(explicitID)
		if err != nil {
			return nil, err
		}
		sm.mu.Lock()
		if sm.findIndexLocked(s.ID()) < 0 {
			sm.idx.Sessions = append(sm.idx.Sessions, s.meta)
		}
		sm.idx.ActiveID = s.ID()
		err = sm.saveLocked()
		sm.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return s, nil
	}

	// 2. Most recent active.
	if id := sm.MostRecentActiveID(); id != "" {
		s, err := sm.LoadSession(id)
		if err == nil {
			_ = sm.SetActive(id)
			return s, nil
		}
		logging.PrintSystem(fmt.Sprintf("[session] could not load %s: %v - creating fresh", id, err))
	}

	// 3. Fresh session.
	s, err := sm.NewSession("Session " + time.Now().UTC().Format("2006-01-02 15:04"))
	if err != nil {
		return nil, err
	}
	return s, nil
}

// BackfillMemory summarizes unsaved non-archived sessions (excluding
// the active one) in a background goroutine. Replaces the old
// synchronous exit-time SaveToMemory.
func (sm *SessionManager) BackfillMemory(activeID string) {
	var pending []string
	for _, m := range sm.List() {
		if m.Status == StatusArchived || m.MemorySaved || m.ID == activeID {
			continue
		}
		pending = append(pending, m.ID)
	}
	if len(pending) == 0 {
		return
	}
	logging.PrintSystem(fmt.Sprintf("[session] background memory backfill: %d unsaved session(s)", len(pending)))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		for _, id := range pending {
			s, err := sm.LoadSession(id)
			if err != nil {
				continue
			}
			if msg, err := sm.SaveToMemory(ctx, s); err != nil {
				logging.PrintSystem(fmt.Sprintf("[session] backfill %s error: %v", id, err))
			} else if msg != "" {
				logging.PrintSystem(fmt.Sprintf("[session] backfill %s: %s", id, msg))
			}
		}
	}()
}
