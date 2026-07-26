package agent

import (
	"os"
	"path/filepath"
	"testing"

	"go-code-agent/internal/llm"
)

func TestUsageTrackerCreatesSessionDir(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "sessions", "abc")

	u, err := NewUsageTracker(sessionDir)
	if err != nil {
		t.Fatalf("NewUsageTracker: %v", err)
	}
	defer u.Close()

	u.Record("openai", "lead", "m", "t1", llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, 0.5)
	if _, err := os.Stat(filepath.Join(sessionDir, "usage.jsonl")); err != nil {
		t.Fatalf("usage.jsonl missing: %v", err)
	}
}
