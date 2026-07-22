package agent

import (
	"fmt"
	"go-code-agent/internal/llm"
	"strings"
	"testing"
)

func makeAssistant(content string, toolCalls ...llm.ToolCall) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: content, ToolCalls: toolCalls}
}

func TestMicroCompact_ClearsOldResults(t *testing.T) {
	// Build enough tool results to exceed KeepRecent (which defaults to 15)
	// so the older ones get cleared.
	var msgs []llm.Message
	msgs = append(msgs, llm.SystemMessage("system"), llm.UserMessage("task"))

	for i := range 20 {
		cid := fmt.Sprintf("c%d", i)
		msgs = append(msgs, makeAssistant("", llm.ToolCall{ID: cid, Name: "bash", Arguments: "{}"}))
		msgs = append(msgs, llm.ToolMessage(string(make([]byte, 200)), cid))
	}

	cleared, reclaimed := MicroCompact(msgs, 0)
	// Older tool results should be cleared; recent ones kept.
	if cleared == 0 {
		t.Fatalf("expected some cleared, got 0")
	}
	if reclaimed <= 0 {
		t.Fatalf("expected reclaimed bytes > 0, got %d", reclaimed)
	}
	// At least one old result should have been replaced with "[cleared: bash]"
	foundCleared := false
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, "[cleared: bash]") {
			foundCleared = true
		}
	}
	if !foundCleared {
		t.Fatal("no tool result was cleared to '[cleared: bash]'")
	}
}

func TestMicroCompact_NoOpWhenFewTools(t *testing.T) {
	msgs := []llm.Message{
		llm.SystemMessage("system"),
		llm.UserMessage("task"),
		makeAssistant("", llm.ToolCall{ID: "c1", Name: "list_dir", Arguments: "{}"}),
		llm.ToolMessage("dir listing content...", "c1"),
	}
	cleared, _ := MicroCompact(msgs, 0)
	if cleared != 0 {
		t.Fatalf("expected 0 cleared (only 1 tool result), got %d", cleared)
	}
}

func TestMicroCompact_ClearAtLeastGuard(t *testing.T) {
	// 20 tool results, each large enough to be clearable. The 5 oldest
	// (20 - KeepRecent=15) are eligible, reclaiming ~5*185 bytes.
	build := func() []llm.Message {
		var msgs []llm.Message
		msgs = append(msgs, llm.SystemMessage("system"), llm.UserMessage("task"))
		for i := range 20 {
			cid := fmt.Sprintf("c%d", i)
			msgs = append(msgs, makeAssistant("", llm.ToolCall{ID: cid, Name: "bash", Arguments: "{}"}))
			msgs = append(msgs, llm.ToolMessage(string(make([]byte, 200)), cid))
		}
		return msgs
	}

	// Guard higher than what can be reclaimed -> no-op, slice untouched.
	msgs := build()
	cleared, reclaimed := MicroCompact(msgs, 1<<20)
	if cleared != 0 || reclaimed != 0 {
		t.Fatalf("expected no clearing under high guard, got cleared=%d reclaimed=%d", cleared, reclaimed)
	}
	for _, m := range msgs {
		if m.Role == llm.RoleTool && len(m.Content) != 200 {
			t.Fatalf("guard should leave tool contents intact, found len=%d", len(m.Content))
		}
	}

	// Guard below what can be reclaimed -> clears.
	msgs = build()
	cleared, reclaimed = MicroCompact(msgs, 100)
	if cleared == 0 || reclaimed < 100 {
		t.Fatalf("expected clearing above guard, got cleared=%d reclaimed=%d", cleared, reclaimed)
	}
}

func TestFindCompactionSplit_SafeSplit(t *testing.T) {
	msgs := []llm.Message{
		llm.SystemMessage("sys"),
		llm.UserMessage("task 1"),
		llm.Message{Role: llm.RoleAssistant, Content: "response"},
		llm.UserMessage("task 2"),
		llm.Message{Role: llm.RoleAssistant, Content: "response 2"},
		llm.UserMessage("task 3"),
		llm.Message{Role: llm.RoleAssistant, Content: "response 3"},
	}

	split := findCompactionSplit(msgs, 3)
	if split <= 0 {
		t.Fatalf("expected positive split, got %d", split)
	}
	if split >= len(msgs) {
		t.Fatalf("split %d must be < %d", split, len(msgs))
	}
}

func TestFindCompactionSplit_UnsafeSplitOnTool(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("task"),
		makeAssistant("", llm.ToolCall{ID: "c1", Name: "bash"}),
		llm.ToolMessage("result", "c1"),
		llm.Message{Role: llm.RoleAssistant, Content: "analysis"},
		llm.UserMessage("more work"),
	}

	split := findCompactionSplit(msgs, 1)
	if msgs[split].Role == "tool" {
		t.Fatalf("split should not land on a tool message, got index %d (%s)", split, msgs[split].Role)
	}
}

func TestNeedsCompaction(t *testing.T) {
	empty := []llm.Message{}
	if NeedsCompaction(empty, nil, 200000) {
		t.Fatal("should not compact empty messages")
	}

	// Build large messages that will exceed token estimate
	many := make([]llm.Message, 200)
	for i := range many {
		many[i] = llm.UserMessage(
			"this is a test message with some content to increase token count " +
				"and another sentence to make it longer and longer each time we loop " +
				"additional padding to reach the compaction threshold quickly enough " +
				"even more padding because the estimate is based on len(json)/4",
		)
	}
	if !NeedsCompaction(many, nil, 1000) {
		t.Fatal("should compact when estimate exceeds budget")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
