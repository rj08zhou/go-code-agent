package history

import (
	"path/filepath"
	"strings"
	"testing"
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
