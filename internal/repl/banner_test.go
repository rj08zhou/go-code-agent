package repl

import (
	"strings"
	"testing"

	"go-code-agent/internal/application"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/mcp"
	"go-code-agent/internal/security"
)

func TestRenderBannerShowsOneCompleteStatusCard(t *testing.T) {
	hitl := hitlaudit.NewHITLManager(nil)
	hitl.SetEnabled(true)
	hitl.SetMode(hitlaudit.HITLModeSafeAuto)
	approval := security.NewApprovalState()
	approval.ApplyPreset("safe-auto")
	built := &application.BuiltRunner{
		Session: application.SessionFacade{
			ID:      "session-20260730-abcdef",
			Title:   "Review startup experience",
			Workdir: "/workspace/project",
			ModelID: "gpt-5.2",
		},
		Runtime: application.RuntimeFacade{
			JudgeEnabled:       true,
			ProviderName:       "openai",
			EndpointHost:       "proxy.example:8443",
			ReasoningRequested: true,
			ReasoningAvailable: true,
			ReasoningEffort:    "high",
		},
		Security: application.SecurityFacade{
			HITL:     hitl,
			Approval: approval,
		},
		Team: application.TeamFacade{MCP: mcp.NewManager(t.TempDir())},
	}

	got := renderBanner(built, 12)
	for _, want := range []string{
		"go-code-agent",
		"Provider: openai  |  Model: gpt-5.2",
		"Endpoint: proxy.example:8443  |  Reasoning: on (effort=high)",
		"Workspace: /workspace/project",
		"Session: session-20260730-abcdef - Review startup experience",
		"Restored: 12 conversation entries",
		"Approval: safe-auto",
		"Judge: on",
		"MCP: 0 active  |  0 pending  |  0 failed",
		"Type a message; /help for commands; Ctrl-C clears input or interrupts a turn; Ctrl-D exits.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner %q does not contain %q", got, want)
		}
	}
	if count := strings.Count(got, "go-code-agent"); count != 1 {
		t.Fatalf("banner contains product name %d times, want 1", count)
	}
}

func TestRenderBannerShowsReasoningDegradation(t *testing.T) {
	built := &application.BuiltRunner{
		Session: application.SessionFacade{
			ID:      "session-degraded",
			Workdir: "/workspace/project",
			ModelID: "claude-opus-4.7",
		},
		Runtime: application.RuntimeFacade{
			ProviderName:       "anthropic",
			EndpointHost:       "api.anthropic.com",
			ReasoningRequested: true,
			ReasoningAvailable: false,
			ReasoningEffort:    "medium",
		},
	}

	got := renderBanner(built, 0)
	if !strings.Contains(got, "Reasoning: degraded (unsupported; effort=medium)") {
		t.Fatalf("banner does not expose reasoning degradation: %q", got)
	}
}

func TestRenderBannerOmitsRestoreStatusForFreshSession(t *testing.T) {
	built := &application.BuiltRunner{
		Session: application.SessionFacade{
			ID:      "session-new",
			Workdir: "/workspace/project",
			ModelID: "gpt-4o",
		},
	}

	if got := renderBanner(built, 0); strings.Contains(got, "Restored:") {
		t.Fatalf("fresh-session banner unexpectedly reports restored history: %q", got)
	}
}
