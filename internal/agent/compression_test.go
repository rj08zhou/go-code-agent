package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go-code-agent/internal/history"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/model"
	"go-code-agent/internal/prompt"
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

func TestAutoCompactPersistsCoverageAndRestoresRecentMessages(t *testing.T) {
	histStore, messages := compressionHistoryFixture(t)
	provider := &fakeProvider{name: "fake", content: "summary of older work"}
	compression := testCompression(t, provider, histStore)
	boundary := len(messages)

	compacted := compression.AutoCompact(
		WithPersistedBoundary(context.Background(), &boundary),
		messages,
		"system",
	)
	if len(compacted) != 5 {
		t.Fatalf("compacted messages = %d, want summary pair plus two recent messages", len(compacted))
	}
	if boundary != len(compacted) {
		t.Fatalf("persisted boundary = %d, want %d", boundary, len(compacted))
	}

	entries, err := histStore.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := entries[len(entries)-1]
	if checkpoint.Covers == nil || checkpoint.Covers.To != 4 {
		t.Fatalf("checkpoint coverage = %#v, want first 4 history entries", checkpoint.Covers)
	}

	restored, restoredCount, err := histStore.LoadRuntime("system")
	if err != nil {
		t.Fatal(err)
	}
	contents := joinedMessageContents(restored)
	for _, want := range []string{"summary of older work", "recent task", "recent answer"} {
		if strings.Count(contents, want) != 1 {
			t.Fatalf("content %q count = %d in %q", want, strings.Count(contents, want), contents)
		}
	}
	if strings.Contains(contents, "old task") || strings.Contains(contents, "middle task") {
		t.Fatalf("covered raw messages were restored: %q", contents)
	}
	if restoredCount != 2 {
		t.Fatalf("restored entries = %d, want two recent messages", restoredCount)
	}
}

func TestAutoCompactKeepsContextWhenSummaryUnavailable(t *testing.T) {
	tests := []struct {
		name    string
		content string
		callErr error
	}{
		{name: "call error", callErr: errors.New("bad request")},
		{name: "empty summary", content: "   "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			histStore, messages := compressionHistoryFixture(t)
			provider := &fakeProvider{name: "fake", content: tc.content, callErr: tc.callErr}
			compression := testCompression(t, provider, histStore)
			boundary := len(messages)

			got := compression.AutoCompact(
				WithPersistedBoundary(context.Background(), &boundary),
				messages,
				"system",
			)
			if !reflect.DeepEqual(got, messages) {
				t.Fatalf("context changed after summary failure:\n got %#v\nwant %#v", got, messages)
			}
			if boundary != len(messages) {
				t.Fatalf("persisted boundary changed to %d", boundary)
			}
			entries, err := histStore.ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 6 {
				t.Fatalf("history entries = %d, want no checkpoint", len(entries))
			}
		})
	}
}

func TestAutoCompactKeepsContextWhenCheckpointWriteFails(t *testing.T) {
	histStore, messages := compressionHistoryFixture(t)
	historyDir := filepath.Dir(histStore.Path())
	if err := os.RemoveAll(historyDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &fakeProvider{name: "fake", content: "valid summary"}
	compression := testCompression(t, provider, histStore)
	boundary := len(messages)

	got := compression.AutoCompact(
		WithPersistedBoundary(context.Background(), &boundary),
		messages,
		"system",
	)
	if !reflect.DeepEqual(got, messages) {
		t.Fatalf("context changed after checkpoint failure:\n got %#v\nwant %#v", got, messages)
	}
	if boundary != len(messages) {
		t.Fatalf("persisted boundary changed to %d", boundary)
	}
}

func TestAutoCompactNoSafeSplitIsNoOp(t *testing.T) {
	provider := &fakeProvider{name: "fake", content: "unused summary"}
	compression := testCompression(t, provider, nil)
	compression.keepRecent = 10
	messages := []llm.Message{
		llm.SystemMessage("system"),
		llm.UserMessage("task"),
		llm.AssistantMessage("answer"),
	}

	got := compression.AutoCompact(context.Background(), messages, "system")
	if !reflect.DeepEqual(got, messages) {
		t.Fatalf("context changed without safe split: got %#v want %#v", got, messages)
	}
	if provider.callCount != 0 {
		t.Fatalf("summary model calls = %d, want 0", provider.callCount)
	}
}

func compressionHistoryFixture(t *testing.T) (*history.Store, []llm.Message) {
	t.Helper()
	store, err := history.New(filepath.Join(t.TempDir(), history.FileName))
	if err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{
		llm.SystemMessage("system"),
		llm.UserMessage("old task"),
		llm.AssistantMessage("old answer"),
		llm.UserMessage("middle task"),
		llm.AssistantMessage("middle answer"),
		llm.UserMessage("recent task"),
		llm.AssistantMessage("recent answer"),
	}
	for _, message := range messages[1:] {
		var err error
		switch message.Role {
		case llm.RoleUser:
			err = store.AppendUser(message.Content)
		case llm.RoleAssistant:
			err = store.AppendAssistant(message.Content, message.ToolCalls)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return store, messages
}

func testCompression(t *testing.T, provider *fakeProvider, histStore *history.Store) *Compression {
	t.Helper()
	compression := NewCompression(
		model.NewGateway(provider, model.NewRoleThrottle(2)),
		histStore,
		t.TempDir(),
		"fake",
		prompt.NewLoader(),
	)
	compression.keepRecent = 2
	return compression
}

func joinedMessageContents(messages []llm.Message) string {
	var contents []string
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return strings.Join(contents, "\n")
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
