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

// HistoryStore - append-only conversation persistence + checkpoint.
//
// Persists the REPL conversation to {session}/history/history.jsonl so
// the next boot can resume. Each line is one JSON record with a stable
// schema. Checkpoints record compaction summaries; LoadRuntime replays
// only entries after the last checkpoint.

const (
	HistoryFileName = "history.jsonl"

	kindSystem     = "system"
	kindUser       = "user"
	kindAssistant  = "assistant"
	kindTool       = "tool"
	kindCheckpoint = "checkpoint"
)

// historyEntry is the on-disk record. Unused fields are omitted via json
// tags so the jsonl stays readable.
type historyEntry struct {
	TS         string                `json:"ts"`
	Kind       string                `json:"kind"`
	Content    string                `json:"content,omitempty"`
	ToolCalls  []historyToolCallRec  `json:"tool_calls,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
	Summary    string                `json:"summary,omitempty"`
	Covers     *historyCoverageRange `json:"covers,omitempty"`
}

type historyToolCallRec struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type historyCoverageRange struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// HistoryStore is the persistence gateway for a single conversation log.
type HistoryStore struct {
	path string
	mu   sync.Mutex

	// written counts the entries appended during this process (plus
	// entries observed on load). Used to generate checkpoint `covers`.
	written int
}

// NewHistoryStoreAt opens (or creates) a history file at an explicit
// path. Missing parent directories are created.
func NewHistoryStoreAt(path string) (*HistoryStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create history parent: %w", err)
	}
	hs := &HistoryStore{path: path}
	if n, err := hs.countEntries(); err == nil {
		hs.written = n
	}
	return hs, nil
}

// Path returns the on-disk jsonl path (useful for log messages).
func (hs *HistoryStore) Path() string { return hs.path }

// countEntries tallies non-blank lines in the history file. Returns 0
// when the file doesn't exist yet.
func (hs *HistoryStore) countEntries() (int, error) {
	f, err := os.Open(hs.path)
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

// ---- Writes ----

func (hs *HistoryStore) appendEntry(e historyEntry) error {
	hs.mu.Lock()
	defer hs.mu.Unlock()

	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal history entry: %w", err)
	}
	f, err := os.OpenFile(hs.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open history file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	// Durability: fsync the appended record so a crash within seconds
	// of writing won't lose the last turn. Cost is one syscall (~1ms
	// on SSD); acceptable because history writes are infrequent
	// (one per LLM turn, not per token).
	_ = f.Sync()
	hs.written++
	return nil
}

// AppendSystem records the system prompt. Callers should only do this
// once per session boot; the system prompt is rebuilt per-turn in
// main.go but we do not re-log it.
func (hs *HistoryStore) AppendSystem(content string) error {
	return hs.appendEntry(historyEntry{Kind: kindSystem, Content: content})
}

// AppendUser records a user turn.
func (hs *HistoryStore) AppendUser(content string) error {
	return hs.appendEntry(historyEntry{Kind: kindUser, Content: content})
}

// AppendAssistant records an assistant turn (possibly with tool calls).
// Accepts neutral ToolCall values so the history layer stays decoupled
// from any vendor SDK.
func (hs *HistoryStore) AppendAssistant(content string, toolCalls []llm.ToolCall) error {
	var recs []historyToolCallRec
	for _, tc := range toolCalls {
		recs = append(recs, historyToolCallRec{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		})
	}
	return hs.appendEntry(historyEntry{
		Kind:      kindAssistant,
		Content:   content,
		ToolCalls: recs,
	})
}

// AppendTool records a tool result.
func (hs *HistoryStore) AppendTool(toolCallID, content string) error {
	return hs.appendEntry(historyEntry{
		Kind:       kindTool,
		ToolCallID: toolCallID,
		Content:    content,
	})
}

// AppendCheckpoint records a compaction summary that supersedes all
// entries up to (but not including) the checkpoint itself. `coveredTo`
// is the 1-indexed count of entries covered by the summary (typically
// the value of hs.written at the moment of compaction).
func (hs *HistoryStore) AppendCheckpoint(summary string, coveredTo int) error {
	return hs.appendEntry(historyEntry{
		Kind:    kindCheckpoint,
		Summary: summary,
		Covers:  &historyCoverageRange{From: 1, To: coveredTo},
	})
}

// ---- Reads ----

// ReadAll returns every entry in file order. Used by LoadRuntime and by
// tooling that wants the raw trail (audit, transcript export).
func (hs *HistoryStore) ReadAll() ([]historyEntry, error) {
	f, err := os.Open(hs.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []historyEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e historyEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// Skip corrupt lines rather than fail the whole load -
			// the conversation is too valuable to drop.
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// LoadRuntime reconstructs an in-memory message slice from history.
// Uses the freshly-built system prompt (ignores on-disk system entries).
// Only materializes entries after the last checkpoint.
func (hs *HistoryStore) LoadRuntime(systemPrompt string) ([]llm.Message, int, error) {
	entries, err := hs.ReadAll()
	if err != nil {
		return nil, 0, err
	}

	// Find the last checkpoint, if any.
	lastCheckpoint := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Kind == kindCheckpoint {
			lastCheckpoint = i
			break
		}
	}

	msgs := []llm.Message{llm.SystemMessage(systemPrompt)}

	// Replay the checkpoint summary as a synthetic user/assistant pair
	// so the LLM has the pre-compaction context.
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
			// Ignored - we already injected the caller-supplied prompt.
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
					calls = append(calls, llm.ToolCall{
						ID:        tc.ID,
						Name:      tc.Name,
						Arguments: tc.Arguments,
					})
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
		case kindCheckpoint:
			// Intermediate checkpoints (shouldn't happen - we only walked
			// past the *last* one) are safe to skip.
			continue
		}
	}

	// Tail-dangling tool_calls guard.
	//
	// If the process was killed mid-turn (SIGINT, crash) we may have
	// persisted an assistant message with pending tool_calls but none
	// of the tool results. Both OpenAI and Anthropic reject that shape:
	// every tool_call id must be answered by a matching tool message.
	// Trim the dangling assistant turn(s) from the tail so the restored
	// conversation is API-valid.
	msgs = trimDanglingToolCalls(msgs)

	return msgs, restored, nil
}

// WrittenCount is the number of entries ever written by this process
// instance plus those observed on disk at boot. Used when creating a
// checkpoint to label its coverage range.
func (hs *HistoryStore) WrittenCount() int {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.written
}

// trimDanglingToolCalls drops orphan assistant-with-tool_calls from the tail
// (prevents 400 errors after crash-mid-turn).
func trimDanglingToolCalls(msgs []llm.Message) []llm.Message {
	if len(msgs) == 0 {
		return msgs
	}

	// Walk from the tail collecting tool result IDs.
	seenToolIDs := map[string]bool{}
	i := len(msgs) - 1
	for ; i >= 0; i-- {
		m := msgs[i]
		if m.Role == llm.RoleTool {
			seenToolIDs[m.ToolCallID] = true
			continue
		}
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			allAnswered := true
			for _, tc := range m.ToolCalls {
				if !seenToolIDs[tc.ID] {
					allAnswered = false
					break
				}
			}
			if allAnswered {
				// This assistant turn is fine; nothing to trim.
				return msgs
			}
			// Orphan assistant. Cut it and everything after.
			return msgs[:i]
		}
		// Anything else in the tail means the conversation ended cleanly
		// (e.g. last message is a user turn, a plain assistant reply, a
		// system message).
		return msgs
	}
	return msgs
}
