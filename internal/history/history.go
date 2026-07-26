// Package history provides append-only conversation persistence.
package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go-code-agent/internal/llm"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	FileName = "history.jsonl"
)

type entryKind string

const (
	kindSystem     entryKind = "system"
	kindUser       entryKind = "user"
	kindAssistant  entryKind = "assistant"
	kindTool       entryKind = "tool"
	kindCheckpoint entryKind = "checkpoint"
)

type Entry struct {
	TS         string         `json:"ts"`
	Kind       entryKind      `json:"kind"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []toolCallRec  `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Covers     *coverageRange `json:"covers,omitempty"`
}

type toolCallRec struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type coverageRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// Store is an append-only conversation log backed by a JSONL file.
type Store struct {
	path    string
	mu      sync.Mutex
	written int
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	hs := &Store{path: path}
	if n, err := hs.countEntries(); err == nil {
		hs.written = n
	}
	return hs, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) countEntries() (int, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n, sc.Err()
}

func (s *Store) appendEntry(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	s.written++
	return nil
}

// Sync flushes all buffered history to disk. Call after batch writes.
func (s *Store) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (s *Store) AppendSystem(content string) error {
	return s.appendEntry(Entry{Kind: kindSystem, Content: content})
}

func (s *Store) AppendUser(content string) error {
	return s.appendEntry(Entry{Kind: kindUser, Content: content})
}

func (s *Store) AppendAssistant(content string, toolCalls []llm.ToolCall) error {
	var recs []toolCallRec
	for _, tc := range toolCalls {
		recs = append(recs, toolCallRec{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	return s.appendEntry(Entry{Kind: kindAssistant, Content: content, ToolCalls: recs})
}

func (s *Store) AppendTool(toolCallID, content string) error {
	return s.appendEntry(Entry{Kind: kindTool, ToolCallID: toolCallID, Content: content})
}

func (s *Store) AppendCheckpoint(summary string, coveredTo int) error {
	return s.appendEntry(Entry{
		Kind:    kindCheckpoint,
		Summary: summary,
		Covers:  &coverageRange{From: 1, To: coveredTo},
	})
}

func (s *Store) ReadAll() ([]Entry, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// LoadRuntime reconstructs messages from history, using systemPrompt and respecting checkpoints.
func (s *Store) LoadRuntime(systemPrompt string) ([]llm.Message, int, error) {
	entries, err := s.ReadAll()
	if err != nil {
		return nil, 0, err
	}
	lastCheckpoint := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == kindCheckpoint {
			lastCheckpoint = i
			break
		}
	}
	msgs := []llm.Message{llm.SystemMessage(systemPrompt)}
	if lastCheckpoint >= 0 {
		cp := entries[lastCheckpoint]
		if strings.TrimSpace(cp.Summary) != "" {
			msgs = append(msgs,
				llm.UserMessage(fmt.Sprintf("[Compressed history - restored from checkpoint]\n%s", cp.Summary)),
				llm.AssistantMessage("Understood. Continuing with summary context."),
			)
		}
	}
	start := 0
	if lastCheckpoint >= 0 {
		start = lastCheckpoint + 1
	}
	restored := 0
	for _, e := range entries[start:] {
		switch e.Kind {
		case kindSystem:
			continue
		case kindUser:
			if e.Content != "" {
				msgs = append(msgs, llm.UserMessage(e.Content))
				restored++
			}
		case kindAssistant:
			m := llm.Message{Role: llm.RoleAssistant, Content: e.Content}
			if len(e.ToolCalls) > 0 {
				calls := make([]llm.ToolCall, 0, len(e.ToolCalls))
				for _, tc := range e.ToolCalls {
					calls = append(calls, llm.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
				}
				m.ToolCalls = calls
			}
			msgs = append(msgs, m)
			restored++
		case kindTool:
			if e.ToolCallID != "" {
				msgs = append(msgs, llm.ToolMessage(e.Content, e.ToolCallID))
				restored++
			}
		}
	}
	beforeTrim := len(msgs)
	msgs = trimDanglingToolCalls(msgs)
	// Adjust restored count for entries stripped by trimming (orphaned
	// tool results, incomplete assistant blocks, trailing user messages).
	if dropped := beforeTrim - len(msgs); dropped > 0 && restored > dropped {
		restored -= dropped
	} else if dropped >= restored {
		restored = 0
	}
	return msgs, restored, nil
}

func (s *Store) WrittenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

// Close flushes any pending writes and releases resources.
func (s *Store) Close() error {
	// Flush by re-opening and syncing the file
	f, err := os.OpenFile(s.path, os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return f.Sync()
}

func trimDanglingToolCalls(msgs []llm.Message) []llm.Message {
	// Pass 1: remove orphan tool messages that appear BEFORE any assistant
	// with tool_calls (or after the last assistant). Such messages cause
	// API errors because they lack a preceding tool_calls message.
	var cleaned []llm.Message
	hasToolCalls := false
	for i, m := range msgs {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			hasToolCalls = true
		}
		if m.Role == llm.RoleTool && !hasToolCalls {
			continue // orphan tool before any assistant with tool_calls
		}
		cleaned = append(cleaned, msgs[i])
	}

	// Pass 2: if an assistant has N tool_calls but fewer than N matching
	// tool results follow, strip the assistant + its partial results.
	var out []llm.Message
	flushed := 0
	i := 0
	for i < len(cleaned) {
		m := cleaned[i]
		if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
			i++
			continue
		}
		j := i + 1
		unanswered := len(m.ToolCalls)
		for j < len(cleaned) && cleaned[j].Role == llm.RoleTool && unanswered > 0 {
			for _, tc := range m.ToolCalls {
				if tc.ID == cleaned[j].ToolCallID {
					unanswered--
					break
				}
			}
			j++
		}
		if unanswered == 0 {
			i = j
			continue
		}
		out = append(out, cleaned[flushed:i]...)
		i = j
		flushed = j
	}
	if out != nil {
		cleaned = append(out, cleaned[flushed:]...)
	}

	// Pass 3: strip trailing orphaned user messages. When the user sends a
	// prompt and Ctrl-C's before the agent responds, the user message is
	// already persisted but has no matching assistant/tool reply. Replaying
	// it on the next session would cause the stale prompt to be re-executed.
	for len(cleaned) > 0 && cleaned[len(cleaned)-1].Role == llm.RoleUser {
		cleaned = cleaned[:len(cleaned)-1]
	}
	return cleaned
}
