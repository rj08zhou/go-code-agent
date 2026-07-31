package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
)

func TestAppendHistoryMessagesPersistsAssistantAndTool(t *testing.T) {
	store, err := history.New(filepath.Join(t.TempDir(), history.FileName))
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{
		llm.UserMessage("already persisted"),
		{
			Role:    llm.RoleAssistant,
			Content: "calling tool",
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read_file", Arguments: `{}`},
			},
		},
		llm.ToolMessage("result", "call-1"),
	}

	if err := appendHistoryMessages(store, messages); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("persisted entries = %d, want assistant and tool only", len(entries))
	}
}

func TestAppendHistoryMessagesReturnsWriteError(t *testing.T) {
	dir := t.TempDir()
	historyDir := filepath.Join(dir, "history")
	store, err := history.New(filepath.Join(historyDir, history.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(historyDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = appendHistoryMessages(store, []llm.Message{llm.AssistantMessage("answer")})
	if err == nil || !strings.Contains(err.Error(), "append assistant message") {
		t.Fatalf("error = %v, want assistant persistence error", err)
	}
}
