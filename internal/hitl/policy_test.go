package hitl

import (
	"encoding/json"
	"testing"

	"go-code-agent/internal/tool"
)

func TestDecideApprovalDecisionBehaviorMatrix(t *testing.T) {
	ordinaryMutation := tool.MutationApprovalInput{
		DiffText: "diff preview",
		Plan: &tool.MutationPlan{
			Path: "ordinary.txt", Content: []byte("new"),
		},
	}
	dangerDef := tool.ToolDefinition{
		Name:      "mcp__demo__delete_user",
		RiskLevel: tool.RiskDanger,
		Effects:   tool.Effects(tool.EffectNetworkAccess),
	}
	bashDef := tool.ToolDefinition{
		Name:      "bash",
		RiskLevel: tool.RiskDanger,
		Effects:   tool.Effects(tool.EffectExecuteProcess),
	}
	safeDef := tool.ToolDefinition{
		Name:      "mcp__demo__read",
		RiskLevel: tool.RiskAuto,
		Effects:   tool.Effects(tool.EffectNetworkAccess),
	}

	tests := []struct {
		name              string
		toolName          string
		args              string
		approvalInput     tool.MutationApprovalInput
		snap              approvalPolicySnapshot
		permissionLevel   string
		earlyReject       bool
		needsReview       bool
		severity          ReviewSeverity
		reviewReason      string
		commandClassified bool
		definition        tool.ToolDefinition
		hasDefinition     bool
		want              approvalDecisionKind
		wantSeverity      ReviewSeverity
	}{
		{
			name:     "disabled allows dangerous bash without review",
			toolName: "bash",
			args:     `{"command":"rm -rf /"}`,
			snap:     approvalPolicySnapshot{enabled: false, mode: HITLModeInteractive, showDiffUI: true},
			want:     decisionAllow,
		},
		{
			name:            "confirm permission rejects when HITL disabled",
			toolName:        "mcp__demo__read",
			args:            `{}`,
			snap:            approvalPolicySnapshot{enabled: false, mode: HITLModeInteractive, showDiffUI: true},
			permissionLevel: "confirm",
			earlyReject:     true,
			want:            decisionReject,
		},
		{
			name:            "block permission rejects even when HITL enabled",
			toolName:        "mcp__demo__write",
			args:            `{}`,
			snap:            approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			permissionLevel: "block",
			earlyReject:     true,
			want:            decisionReject,
		},
		{
			name:          "interactive mutation opens diff review",
			toolName:      "write_file",
			args:          `{"path":"ordinary.txt","content":"new"}`,
			approvalInput: ordinaryMutation,
			snap:          approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			want:          decisionPromptMutation,
		},
		{
			name:          "safe-auto mutation opens diff review",
			toolName:      "write_file",
			args:          `{"path":"ordinary.txt","content":"new"}`,
			approvalInput: ordinaryMutation,
			snap:          approvalPolicySnapshot{enabled: true, mode: HITLModeSafeAuto, showDiffUI: true},
			want:          decisionPromptMutation,
		},
		{
			name:          "auto-approve ordinary mutation is silent allow",
			toolName:      "write_file",
			args:          `{"path":"ordinary.txt","content":"new"}`,
			approvalInput: ordinaryMutation,
			snap:          approvalPolicySnapshot{enabled: true, mode: HITLModeAutoApprove, showDiffUI: false},
			want:          decisionAllow,
		},
		{
			name:          "auto-reject ordinary mutation becomes general prompt",
			toolName:      "write_file",
			args:          `{"path":"ordinary.txt","content":"new"}`,
			approvalInput: ordinaryMutation,
			snap:          approvalPolicySnapshot{enabled: true, mode: HITLModeAutoReject, showDiffUI: true},
			want:          decisionPromptGeneral,
			wantSeverity:  SeverityMedium,
		},
		{
			name:          "notify-only ordinary mutation becomes general prompt",
			toolName:      "write_file",
			args:          `{"path":"ordinary.txt","content":"new"}`,
			approvalInput: ordinaryMutation,
			snap:          approvalPolicySnapshot{enabled: true, mode: HITLModeNotifyOnly, showDiffUI: true},
			want:          decisionPromptGeneral,
			wantSeverity:  SeverityMedium,
		},
		{
			name:         "interactive dangerous shell becomes general prompt",
			toolName:     "bash",
			args:         `{"command":"rm -rf /"}`,
			snap:         approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			needsReview:  true,
			severity:     SeverityHigh,
			reviewReason: "dangerous",
			want:         decisionPromptGeneral,
			wantSeverity: SeverityHigh,
		},
		{
			name:              "read-only shell command is not re-escalated by bash RiskDanger",
			toolName:          "bash",
			args:              `{"command":"ls -la"}`,
			snap:              approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			severity:          SeverityLow,
			reviewReason:      "read-only/inspection-only",
			commandClassified: true,
			definition:        bashDef,
			hasDefinition:     true,
			want:              decisionAllow,
		},
		{
			name:              "permission confirm outranks a classified shell command",
			toolName:          "bash",
			args:              `{"command":"ls -la"}`,
			snap:              approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			permissionLevel:   "confirm",
			severity:          SeverityLow,
			commandClassified: true,
			definition:        bashDef,
			hasDefinition:     true,
			want:              decisionPromptGeneral,
			wantSeverity:      SeverityHigh,
		},
		{
			name:              "classified caution shell keeps command risk, not bash RiskDanger",
			toolName:          "bash",
			args:              `{"command":"mkdir tmp"}`,
			snap:              approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			needsReview:       true,
			severity:          SeverityHigh,
			reviewReason:      "shell execution via 'bash' requires review: command has side effects; no dangerous pattern matched",
			commandClassified: true,
			definition:        bashDef,
			hasDefinition:     true,
			want:              decisionPromptGeneral,
			wantSeverity:      SeverityHigh,
		},
		{
			name:              "classified danger shell keeps command reason, not bash RiskDanger",
			toolName:          "bash",
			args:              `{"command":"rm foo.txt"}`,
			snap:              approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			needsReview:       true,
			severity:          SeverityHigh,
			reviewReason:      "command matches a potentially dangerous pattern",
			commandClassified: true,
			definition:        bashDef,
			hasDefinition:     true,
			want:              decisionPromptGeneral,
			wantSeverity:      SeverityHigh,
		},
		{
			name:          "safe-auto dangerous MCP definition becomes general prompt",
			toolName:      "mcp__demo__delete_user",
			args:          `{"id":"42"}`,
			snap:          approvalPolicySnapshot{enabled: true, mode: HITLModeSafeAuto, showDiffUI: true},
			definition:    dangerDef,
			hasDefinition: true,
			want:          decisionPromptGeneral,
			wantSeverity:  SeverityDanger,
		},
		{
			name:            "permission allow suppresses definition risk",
			toolName:        "mcp__demo__delete_user",
			args:            `{"id":"42"}`,
			snap:            approvalPolicySnapshot{enabled: true, mode: HITLModeSafeAuto, showDiffUI: true},
			permissionLevel: "allow",
			definition:      dangerDef,
			hasDefinition:   true,
			want:            decisionAllow,
		},
		{
			name:            "permission allow does not suppress NeedsReview",
			toolName:        "bash",
			args:            `{"command":"rm -rf /"}`,
			snap:            approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			permissionLevel: "allow",
			needsReview:     true,
			severity:        SeverityHigh,
			reviewReason:    "dangerous",
			want:            decisionPromptGeneral,
			wantSeverity:    SeverityHigh,
		},
		{
			name:            "permission confirm forces general prompt for safe tool",
			toolName:        "mcp__demo__read",
			args:            `{}`,
			snap:            approvalPolicySnapshot{enabled: true, mode: HITLModeAutoReject, showDiffUI: true},
			permissionLevel: "confirm",
			definition:      safeDef,
			hasDefinition:   true,
			want:            decisionPromptGeneral,
			wantSeverity:    SeverityHigh,
		},
		{
			name:          "interactive mutation without showDiffUI falls through to allow",
			toolName:      "write_file",
			args:          `{"path":"ordinary.txt","content":"new"}`,
			approvalInput: ordinaryMutation,
			snap:          approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: false},
			want:          decisionAllow,
		},
		{
			name:     "unchanged mutation content does not require review",
			toolName: "edit_file",
			args:     `{"path":"same.txt"}`,
			approvalInput: tool.MutationApprovalInput{
				Plan: &tool.MutationPlan{
					Path: "same.txt", OriginalContent: []byte("x"), Content: []byte("x"), Existed: true,
				},
			},
			snap: approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, showDiffUI: true},
			want: decisionAllow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var early *approvalDecision
			if tc.earlyReject {
				_, early = permissionRejectDecision(tc.permissionLevel, tc.snap.enabled, tc.toolName)
				if early == nil {
					t.Fatal("expected early reject decision")
				}
			}
			review := resolveReviewRequirement(
				reviewRequirement{
					needsReview:       tc.needsReview,
					severity:          tc.severity,
					reason:            tc.reviewReason,
					commandClassified: tc.commandClassified,
				},
				tc.permissionLevel, tc.snap, tc.definition, tc.hasDefinition,
			)
			decision := decideApprovalDecision(
				tc.toolName,
				json.RawMessage(tc.args),
				tc.approvalInput,
				tc.snap,
				tc.permissionLevel,
				early,
				review,
			)
			if decision.kind != tc.want {
				t.Fatalf("decision kind = %v, want %v", decision.kind, tc.want)
			}
			if tc.want == decisionPromptGeneral && tc.wantSeverity != "" && decision.general.Severity != tc.wantSeverity {
				t.Fatalf("general severity = %q, want %q", decision.general.Severity, tc.wantSeverity)
			}
		})
	}
}

func TestPermissionRejectPlan(t *testing.T) {
	_, reject := permissionRejectDecision("block", true, "bash")
	if reject == nil || reject.kind != decisionReject {
		t.Fatalf("block plan = %#v", reject)
	}
	_, reject = permissionRejectDecision("confirm", false, "bash")
	if reject == nil || reject.kind != decisionReject {
		t.Fatalf("confirm+disabled plan = %#v", reject)
	}
	level, reject := permissionRejectDecision("confirm", true, "bash")
	if level != "confirm" || reject != nil {
		t.Fatalf("confirm+enabled = (%q, %#v)", level, reject)
	}
	level, reject = permissionRejectDecision("allow", true, "bash")
	if level != "allow" || reject != nil {
		t.Fatalf("allow = (%q, %#v)", level, reject)
	}
}
