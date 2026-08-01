package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/llm"
)

func TestStore_AppendReadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	s.AppendUser("hello")
	s.AppendAssistant("hi there", nil)
	s.AppendTool("call_1", "result")

	entries, err := s.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Kind != "user" {
		t.Fatalf("expected user, got %s", entries[0].Kind)
	}
	if entries[1].Kind != "assistant" {
		t.Fatalf("expected assistant, got %s", entries[1].Kind)
	}
	if entries[2].Kind != "tool" {
		t.Fatalf("expected tool, got %s", entries[2].Kind)
	}
}

func TestStore_AssistantHistoryExcludesReasoningData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	// AppendAssistant deliberately accepts only final content and tool calls;
	// native reasoning has no persistence input and cannot survive resume.
	if err := s.AppendAssistant("final answer", nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "reasoning") || strings.Contains(string(raw), "opaque") {
		t.Fatalf("history schema unexpectedly contains reasoning data: %s", raw)
	}
	msgs, _, err := s.LoadRuntime("system")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		if msg.Reasoning != nil {
			t.Fatalf("resumed history restored reasoning data: %#v", msg.Reasoning)
		}
	}
}

func TestStore_Checkpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	s, _ := New(path)
	s.AppendUser("task 1")
	s.AppendAssistant("done", nil)
	s.AppendCheckpoint("summary of task 1", 2)

	entries, _ := s.ReadAll()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[2].Kind != "checkpoint" {
		t.Fatalf("expected checkpoint, got %s", entries[2].Kind)
	}
	if entries[2].Summary != "summary of task 1" {
		t.Fatalf("unexpected summary: %s", entries[2].Summary)
	}
}

func TestStore_LoadRuntimeWithCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	s, _ := New(path)

	// Older messages (will be summarized)
	s.AppendUser("first task")
	s.AppendAssistant("working on it", nil)
	s.AppendTool("c1", "result")

	// Checkpoint summarizing older messages
	s.AppendCheckpoint("User wanted to build a Go server.", 3)

	// Recent messages (kept verbatim)
	s.AppendUser("now add health check")
	s.AppendAssistant("adding /health endpoint", nil)

	sysPrompt := "You are a helpful agent."
	msgs, _, err := s.LoadRuntime(sysPrompt)
	if err != nil {
		t.Fatal(err)
	}
	// Should contain: system, checkpoint user msg, checkpoint asst msg, recent user, recent asst
	if len(msgs) < 4 {
		t.Fatalf("expected >= 4 messages, got %d", len(msgs))
	}
	// Check that the checkpoint summary is present
	foundSummary := false
	for _, m := range msgs {
		if strings.Contains(m.Content, "Go server") {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Errorf("checkpoint summary not found in loaded messages (got %d msgs)", len(msgs))
	}
}

func TestStore_LoadRuntimeRestoresUncoveredEntriesBeforeCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, write := range []func() error{
		func() error { return s.AppendUser("old task") },
		func() error { return s.AppendAssistant("old answer", nil) },
		func() error { return s.AppendUser("recent task") },
		func() error { return s.AppendAssistant("recent answer", nil) },
		func() error { return s.AppendCheckpoint("compressed prefix", 2) },
		func() error { return s.AppendUser("after checkpoint") },
		func() error { return s.AppendAssistant("after answer", nil) },
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}

	msgs, restored, err := s.LoadRuntime("system")
	if err != nil {
		t.Fatal(err)
	}
	contents := messageContents(msgs)
	if strings.Contains(contents, "old task") || strings.Contains(contents, "old answer") {
		t.Fatalf("covered messages were restored: %q", contents)
	}
	for _, want := range []string{"compressed prefix", "recent task", "recent answer", "after checkpoint", "after answer"} {
		if strings.Count(contents, want) != 1 {
			t.Fatalf("content %q count = %d in %q", want, strings.Count(contents, want), contents)
		}
	}
	if restored != 4 {
		t.Fatalf("restored entries = %d, want 4 uncovered messages", restored)
	}
}

func messageContents(messages []llm.Message) string {
	var contents []string
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return strings.Join(contents, "\n")
}

func TestStore_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")
	s, _ := New(path)
	s.AppendUser("hello")

	s2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := s2.ReadAll()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after reload, got %d", len(entries))
	}
}

func TestStore_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent", "test.jsonl")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := s.ReadAll()
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
	msgs, _, _ := s.LoadRuntime("system")
	// Should have at least the system message
	if len(msgs) == 0 {
		t.Fatal("expected at least system message")
	}
}

func TestNewReturnsExistingHistoryReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), "count history entries") {
		t.Fatalf("New error = %v, want history count error", err)
	}
}
