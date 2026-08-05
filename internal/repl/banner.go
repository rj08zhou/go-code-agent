package repl

import (
	"fmt"
	"strings"

	"go-code-agent/internal/application"
	"go-code-agent/internal/utils"
)

func (r *Loop) printBanner(restored int) {
	fmt.Print(renderBanner(r.built, restored))
}

func renderBanner(b *application.BuiltRunner, restored int) string {
	judgeStatus := "off"
	if b.Runtime.JudgeEnabled {
		judgeStatus = "on"
	}

	providerName := strings.TrimSpace(b.Runtime.ProviderName)
	if providerName == "" {
		providerName = "unknown"
	}
	endpointHost := strings.TrimSpace(b.Runtime.EndpointHost)
	if endpointHost == "" {
		endpointHost = "unknown"
	}

	var out strings.Builder
	divider := strings.Repeat("=", 60)
	fmt.Fprintf(&out, "%s%s%s\n", utils.Bold+utils.Cyan, divider, utils.Reset)
	fmt.Fprintf(&out, "%s  go-code-agent%s\n", utils.Bold+utils.Cyan, utils.Reset)
	fmt.Fprintf(&out, "  Provider: %s  |  Model: %s\n", providerName, b.Session.ModelID)
	fmt.Fprintf(&out, "  Endpoint: %s  |  Reasoning: %s\n", endpointHost, reasoningStatus(b.Runtime))
	fmt.Fprintf(&out, "  Workspace: %s\n", b.Session.Workdir)
	fmt.Fprintf(&out, "  Session: %s", b.Session.ID)
	if b.Session.Title != "" {
		fmt.Fprintf(&out, " - %s", b.Session.Title)
	}
	if restored > 0 {
		fmt.Fprintf(&out, "  |  Restored: %d conversation entries", restored)
	}
	out.WriteByte('\n')
	fmt.Fprintf(&out, "  Approval: %s  |  Judge: %s", effectiveApprovalMode(b), judgeStatus)
	if perms := b.Security.Permissions; perms != nil && perms.Count() > 0 {
		fmt.Fprintf(&out, "  |  Permissions: %d rules", perms.Count())
	}
	out.WriteByte('\n')
	if b.Team.MCP != nil {
		active := b.Team.MCP.Count()
		pending := len(b.Team.MCP.ListPending())
		failed := b.Team.MCP.FailedCount()
		fmt.Fprintf(&out, "  MCP: %d active  |  %d pending  |  %d failed\n", active, pending, failed)
	}
	fmt.Fprintf(&out, "%s%s%s\n\n", utils.Bold+utils.Cyan, divider, utils.Reset)
	out.WriteString("Type a message; /help for commands; Ctrl-C clears input or interrupts a turn; Ctrl-D exits.\n")
	return out.String()
}

func reasoningStatus(runtime application.RuntimeFacade) string {
	if !runtime.ReasoningRequested {
		return "off"
	}
	effort := strings.ToLower(strings.TrimSpace(runtime.ReasoningEffort))
	if effort == "" {
		effort = "provider-default"
	}
	if !runtime.ReasoningAvailable {
		return fmt.Sprintf("degraded (unsupported; effort=%s)", effort)
	}
	return fmt.Sprintf("on (effort=%s)", effort)
}
