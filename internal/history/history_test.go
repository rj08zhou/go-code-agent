package history

import (
	"os"
	"path/filepath"
	"testing"

	"go-code-agent/internal/llm"
)

// --- trimDanglingToolCalls ------------------------------------------------

func TestTrimDanglingNone(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("hi"),
		llm.AssistantMessage("hello"),
	}
	got := trimDanglingToolCalls(msgs)
	if len(got) != 2 {
		t.Fatalf("expected no trim, got %d msgs", len(got))
	}
}

func TestTrimDanglingOrphanAssistant(t *testing.T) {
	// Assistant with a tool_call that has no matching tool result.
	msgs := []llm.Message{
		llm.UserMessage("run it"),
		{
			Role:      llm.RoleAssistant,
			Content:   "",
			ToolCalls: []llm.ToolCall{{ID: "c1", Name: "bash", Arguments: "{}"}},
		},
	}
	got := trimDanglingToolCalls(msgs)
	if len(got) != 1 {
		t.Fatalf("expected orphan assistant trimmed, got %d msgs: %+v", len(got), got)
	}
	if got[0].Role != llm.RoleUser {
		t.Errorf("remaining msg should be the user turn, got %v", got[0].Role)
	}
}

func TestTrimDanglingAnsweredKept(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("run it"),
		{
			Role:      llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: "c1", Name: "bash", Arguments: "{}"}},
		},
		llm.ToolMessage("ok", "c1"),
		llm.AssistantMessage("done"),
	}
	got := trimDanglingToolCalls(msgs)
	if len(got) != 4 {
		t.Fatalf("all answered turns should be kept, got %d", len(got))
	}
}

func TestTrimDanglingMultipleOrphans(t *testing.T) {
	// Both orphan assistant turns must be dropped, not just the
	// trailing one: history.jsonl is append-only, so a mid-turn
	// orphan from a crashed session can end up buried under a later
	// session's valid turns.
	msgs := []llm.Message{
		llm.UserMessage("a"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x1", Name: "b"}}},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x2", Name: "b"}}},
	}
	got := trimDanglingToolCalls(msgs)
	if len(got) != 1 {
		t.Fatalf("expected both orphans trimmed, got %d msgs: %+v", len(got), got)
	}
	if got[0].Role != llm.RoleUser {
		t.Errorf("unexpected surviving msgs: %+v", got)
	}
}

func TestTrimDanglingOrphanInMiddle(t *testing.T) {
	// Regression: an orphan buried in the middle (e.g. session A
	// crashed mid-turn, session B appended a clean turn afterward)
	// must be dropped even though the tail is clean.
	msgs := []llm.Message{
		llm.UserMessage("q1"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x1", Name: "b"}}}, // orphan, no tool result follows
		llm.UserMessage("q2"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x2", Name: "b"}}},
		llm.ToolMessage("ok", "x2"),
		llm.AssistantMessage("done"),
	}
	got := trimDanglingToolCalls(msgs)
	for _, m := range got {
		if m.Role == llm.RoleAssistant {
			for _, tc := range m.ToolCalls {
				if tc.ID == "x1" {
					t.Fatalf("orphan x1 should have been dropped: %+v", got)
				}
			}
		}
	}
	if len(got) != 5 { // q1, q2, a(x2), tool(x2), done
		t.Fatalf("expected 5 msgs (orphan a(x1) dropped), got %d: %+v", len(got), got)
	}
}

func TestTrimDanglingKeepsContentBetweenTwoOrphans(t *testing.T) {
	// Regression for the copy-forward bookkeeping: content between two
	// dropped orphans (not just before the first or after the last)
	// must survive.
	msgs := []llm.Message{
		llm.UserMessage("q1"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x1", Name: "b"}}}, // orphan
		llm.UserMessage("kept"),
		llm.AssistantMessage("kept reply"),
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "x2", Name: "b"}}}, // orphan
	}
	got := trimDanglingToolCalls(msgs)
	want := []string{"q1", "kept", "kept reply"}
	if len(got) != len(want) {
		t.Fatalf("expected %d msgs, got %d: %+v", len(want), len(got), got)
	}
	for i, m := range got {
		if m.Content != want[i] {
			t.Errorf("msg[%d] = %q, want %q", i, m.Content, want[i])
		}
	}
}

func TestTrimDanglingEmpty(t *testing.T) {
	if got := trimDanglingToolCalls(nil); got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}
}

// --- append / ReadAll round-trip ------------------------------------------

func TestAppendReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	hs, err := NewHistoryStoreAt(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := hs.AppendSystem("sys"); err != nil {
		t.Fatalf("append system: %v", err)
	}
	if err := hs.AppendUser("hello"); err != nil {
		t.Fatalf("append user: %v", err)
	}
	if err := hs.AppendAssistant("thinking", []llm.ToolCall{{ID: "t1", Name: "bash", Arguments: "{}"}}); err != nil {
		t.Fatalf("append assistant: %v", err)
	}
	if err := hs.AppendTool("t1", "result"); err != nil {
		t.Fatalf("append tool: %v", err)
	}

	all, err := hs.ReadAll()
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(all))
	}
	if all[0].Kind != kindSystem || all[1].Kind != kindUser ||
		all[2].Kind != kindAssistant || all[3].Kind != kindTool {
		t.Errorf("kinds mismatch: %v", []string{all[0].Kind, all[1].Kind, all[2].Kind, all[3].Kind})
	}
	if len(all[2].ToolCalls) != 1 || all[2].ToolCalls[0].ID != "t1" {
		t.Errorf("assistant tool_calls not persisted: %+v", all[2].ToolCalls)
	}
	if hs.WrittenCount() != 4 {
		t.Errorf("WrittenCount = %d, want 4", hs.WrittenCount())
	}
}

// --- LoadRuntime: checkpoint replay + dangling guard ----------------------

func TestLoadRuntimeNoCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	hs, _ := NewHistoryStoreAt(path)
	hs.AppendUser("q1")
	hs.AppendAssistant("a1", nil)
	hs.AppendUser("q2")

	msgs, restored, err := hs.LoadRuntime("SYS")
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if restored != 3 {
		t.Errorf("restored = %d, want 3", restored)
	}
	if len(msgs) != 4 || msgs[0].Role != llm.RoleSystem {
		t.Fatalf("expected [system, user, assistant, user], got %d msgs", len(msgs))
	}
}

func TestLoadRuntimeWithCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	hs, _ := NewHistoryStoreAt(path)
	hs.AppendUser("old q")
	hs.AppendAssistant("old a", nil)
	hs.AppendCheckpoint("summary of old", 2)
	hs.AppendUser("new q")

	msgs, restored, err := hs.LoadRuntime("SYS")
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	// system + 2 checkpoint msgs + 1 user == 4; old entries skipped.
	if len(msgs) != 4 {
		t.Fatalf("expected 4 msgs after checkpoint replay, got %d: %+v", len(msgs), msgs)
	}
	if restored != 1 {
		t.Errorf("restored = %d, want 1 (only post-checkpoint user turn)", restored)
	}
	// The injected checkpoint summary must appear as a user message.
	found := false
	for _, m := range msgs {
		if m.Role == llm.RoleUser && contains(m.Content, "summary of old") {
			found = true
		}
	}
	if !found {
		t.Errorf("checkpoint summary not injected into runtime messages: %+v", msgs)
	}
}

func TestLoadRuntimeDropsDanglingAfterCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	hs, _ := NewHistoryStoreAt(path)
	hs.AppendCheckpoint("sum", 0)
	hs.AppendUser("q")
	// Orphan assistant (tool call never answered) after the checkpoint.
	err := hs.AppendAssistant("", []llm.ToolCall{{ID: "z1", Name: "bash"}})
	if err != nil {
		t.Fatalf("append assistant: %v", err)
	}

	msgs, _, err := hs.LoadRuntime("SYS")
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	for _, m := range msgs {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			t.Errorf("dangling assistant tool_call should have been trimmed: %+v", msgs)
		}
	}
}

// --- LoadRuntime: corrupt lines skipped -----------------------------------

func TestLoadRuntimeSkipsCorruptLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	hs, _ := NewHistoryStoreAt(path)
	hs.AppendUser("good")
	// Write a corrupt line directly to the file.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open for corrupt line: %v", err)
	}
	if _, err := f.WriteString("{not valid json}\n"); err != nil {
		t.Fatalf("write corrupt line: %v", err)
	}
	f.Close()

	msgs, restored, err := hs.LoadRuntime("SYS")
	if err != nil {
		t.Fatalf("LoadRuntime: %v", err)
	}
	if restored != 1 {
		t.Errorf("corrupt line should be skipped; restored = %d, want 1", restored)
	}
	if len(msgs) != 2 { // system + 1 user
		t.Errorf("expected 2 msgs (system + user), got %d", len(msgs))
	}
}

// --- helpers ---------------------------------------------------------------

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
