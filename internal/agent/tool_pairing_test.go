package agent

import (
	"go-code-agent/internal/llm"
	"testing"
)

func TestEnsureToolCallPairing_ReordersInterruptedBlock(t *testing.T) {
	msgs := []llm.Message{
		llm.UserMessage("task"),
		{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "a", Name: "read_file"},
				{ID: "b", Name: "read_file"},
				{ID: "c", Name: "read_file"},
			},
		},
		llm.ToolMessage("ra", "a"),
		llm.UserMessage("<convergence-nudge>stop</convergence-nudge>"),
		llm.ToolMessage("rb", "b"),
		llm.ToolMessage("rc", "c"),
	}

	out, filled := ensureToolCallPairing(msgs)
	if filled != 0 {
		t.Fatalf("filled = %d, want 0", filled)
	}
	// Expected: user, assistant, tool a/b/c, then deferred nudge.
	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleTool, llm.RoleTool, llm.RoleUser}
	if len(out) != len(wantRoles) {
		t.Fatalf("len(out)=%d, want %d: %+v", len(out), len(wantRoles), rolesOf(out))
	}
	for i, role := range wantRoles {
		if out[i].Role != role {
			t.Fatalf("out[%d].Role=%s, want %s (roles=%v)", i, out[i].Role, role, rolesOf(out))
		}
	}
	if out[5].Content != "<convergence-nudge>stop</convergence-nudge>" {
		t.Fatalf("deferred nudge lost: %q", out[5].Content)
	}
}

func TestEnsureToolCallPairing_FillsMissing(t *testing.T) {
	msgs := []llm.Message{
		{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "a", Name: "bash"},
				{ID: "b", Name: "bash"},
			},
		},
		llm.ToolMessage("ra", "a"),
		llm.UserMessage("<system>truncated</system>"),
	}
	out, filled := ensureToolCallPairing(msgs)
	if filled != 1 {
		t.Fatalf("filled = %d, want 1", filled)
	}
	if out[2].Role != llm.RoleTool || out[2].ToolCallID != "b" {
		t.Fatalf("expected synthetic tool for b, got %+v", out[2])
	}
	if out[3].Role != llm.RoleUser {
		t.Fatalf("expected deferred user after tools, got %s", out[3].Role)
	}
}

func TestEnsureToolCallPairing_NoOpWhenValid(t *testing.T) {
	msgs := []llm.Message{
		{
			Role:      llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: "a", Name: "bash"}},
		},
		llm.ToolMessage("ra", "a"),
		llm.UserMessage("<stuck>x</stuck>"),
	}
	out, filled := ensureToolCallPairing(msgs)
	if filled != 0 {
		t.Fatalf("filled = %d, want 0", filled)
	}
	if &out[0] != &msgs[0] && len(out) != len(msgs) {
		// Function may return original slice on no-op.
	}
	if len(out) != len(msgs) {
		t.Fatalf("len changed on no-op: %d -> %d", len(msgs), len(out))
	}
}

func rolesOf(msgs []llm.Message) []llm.Role {
	out := make([]llm.Role, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}
