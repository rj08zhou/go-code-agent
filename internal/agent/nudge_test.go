package agent

import (
	"go-code-agent/internal/llm"
	"testing"
)

func TestDropConsumedNudges(t *testing.T) {
	t.Run("removes nudge before last assistant, keeps trailing nudge", func(t *testing.T) {
		msgs := []llm.Message{
			llm.SystemMessage("sys"),
			llm.UserMessage("real task"),
			llm.UserMessage("<reflect>consumed nudge</reflect>"),
			llm.AssistantMessage("did work"),
			llm.UserMessage("<stuck>not yet consumed</stuck>"),
		}
		out, removed := dropConsumedNudges(msgs)
		if removed != 1 {
			t.Fatalf("removed = %d, want 1", removed)
		}
		if len(out) != 4 {
			t.Fatalf("len(out) = %d, want 4", len(out))
		}
		// The trailing (unconsumed) nudge must survive.
		if out[len(out)-1].Content != "<stuck>not yet consumed</stuck>" {
			t.Fatalf("trailing nudge was dropped: %+v", out)
		}
		// The real task user message must survive.
		if out[1].Content != "real task" {
			t.Fatalf("real user message was dropped: %+v", out)
		}
	})

	t.Run("no assistant message is a no-op", func(t *testing.T) {
		msgs := []llm.Message{
			llm.SystemMessage("sys"),
			llm.UserMessage("<reflect>x</reflect>"),
		}
		_, removed := dropConsumedNudges(msgs)
		if removed != 0 {
			t.Fatalf("removed = %d, want 0", removed)
		}
	})

	t.Run("does not touch non-nudge user messages", func(t *testing.T) {
		msgs := []llm.Message{
			llm.UserMessage("real task"),
			llm.UserMessage("Relevant memory:\nfoo"),
			llm.AssistantMessage("ok"),
		}
		_, removed := dropConsumedNudges(msgs)
		if removed != 0 {
			t.Fatalf("removed = %d, want 0 (memory recall must be kept)", removed)
		}
	})

	t.Run("removes multiple consumed nudges", func(t *testing.T) {
		msgs := []llm.Message{
			llm.UserMessage("real task"),
			llm.UserMessage("<mini-reflect>a</mini-reflect>"),
			llm.AssistantMessage("t1"),
			llm.UserMessage("<convergence-nudge>b</convergence-nudge>"),
			llm.UserMessage("<verification-failed>c</verification-failed>"),
			llm.AssistantMessage("t2"),
		}
		out, removed := dropConsumedNudges(msgs)
		if removed != 3 {
			t.Fatalf("removed = %d, want 3", removed)
		}
		for _, m := range out {
			if isEphemeralNudge(m) {
				t.Fatalf("nudge survived: %q", m.Content)
			}
		}
	})
}
