package hitlaudit

import (
	"encoding/json"
	"testing"

	"go-code-agent/internal/tool"
)

func TestDecideApprovalPlanBehaviorMatrix(t *testing.T) {
	ordinaryMutation := tool.ApprovalPreview{
		Text: "diff preview",
		Mutation: &tool.PreviewRequest{
			Path: "ordinary.txt", Content: []byte("new"),
		},
	}
	dangerDef := tool.ToolDefinition{
		Name:      "mcp__demo__delete_user",
		RiskLevel: tool.RiskDanger,
		Effects:   tool.Effects(tool.EffectNetworkAccess),
	}
	safeDef := tool.ToolDefinition{
		Name:      "mcp__demo__read",
		RiskLevel: tool.RiskAuto,
		Effects:   tool.Effects(tool.EffectNetworkAccess),
	}

	tests := []struct {
		name            string
		toolName        string
		args            string
		preview         tool.ApprovalPreview
		snap            approvalPolicySnapshot
		permissionLevel string
		earlyReject     bool
		needsReview     bool
		riskLevel       string
		reviewReason    string
		definition      tool.ToolDefinition
		hasDefinition   bool
		want            approvalPlanKind
		wantRisk        string
	}{
		{
			name:     "disabled allows dangerous bash without review",
			toolName: "bash",
			args:     `{"command":"rm -rf /"}`,
			snap:     approvalPolicySnapshot{enabled: false, mode: HITLModeInteractive, previewDiff: true},
			want:     planAllow,
		},
		{
			name:            "confirm permission rejects when HITL disabled",
			toolName:        "mcp__demo__read",
			args:            `{}`,
			snap:            approvalPolicySnapshot{enabled: false, mode: HITLModeInteractive, previewDiff: true},
			permissionLevel: "confirm",
			earlyReject:     true,
			want:            planReject,
		},
		{
			name:            "block permission rejects even when HITL enabled",
			toolName:        "mcp__demo__write",
			args:            `{}`,
			snap:            approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, previewDiff: true},
			permissionLevel: "block",
			earlyReject:     true,
			want:            planReject,
		},
		{
			name:     "interactive mutation opens diff review",
			toolName: "write_file",
			args:     `{"path":"ordinary.txt","content":"new"}`,
			preview:  ordinaryMutation,
			snap:     approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, previewDiff: true},
			want:     planPromptMutation,
		},
		{
			name:     "safe-only mutation opens diff review",
			toolName: "write_file",
			args:     `{"path":"ordinary.txt","content":"new"}`,
			preview:  ordinaryMutation,
			snap:     approvalPolicySnapshot{enabled: true, mode: HITLModeSafeOnly, previewDiff: true},
			want:     planPromptMutation,
		},
		{
			name:     "auto-approve ordinary mutation is silent allow",
			toolName: "write_file",
			args:     `{"path":"ordinary.txt","content":"new"}`,
			preview:  ordinaryMutation,
			snap:     approvalPolicySnapshot{enabled: true, mode: HITLModeAutoApprove, previewDiff: false},
			want:     planAllow,
		},
		{
			name:     "auto-reject ordinary mutation becomes general prompt",
			toolName: "write_file",
			args:     `{"path":"ordinary.txt","content":"new"}`,
			preview:  ordinaryMutation,
			snap:     approvalPolicySnapshot{enabled: true, mode: HITLModeAutoReject, previewDiff: true},
			want:     planPromptGeneral,
			wantRisk: "medium",
		},
		{
			name:     "notify-only ordinary mutation becomes general prompt",
			toolName: "write_file",
			args:     `{"path":"ordinary.txt","content":"new"}`,
			preview:  ordinaryMutation,
			snap:     approvalPolicySnapshot{enabled: true, mode: HITLModeNotifyOnly, previewDiff: true},
			want:     planPromptGeneral,
			wantRisk: "medium",
		},
		{
			name:         "interactive dangerous shell becomes general prompt",
			toolName:     "bash",
			args:         `{"command":"rm -rf /"}`,
			snap:         approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, previewDiff: true},
			needsReview:  true,
			riskLevel:    "high",
			reviewReason: "dangerous",
			want:         planPromptGeneral,
			wantRisk:     "high",
		},
		{
			name:          "safe-only dangerous MCP definition becomes general prompt",
			toolName:      "mcp__demo__delete_user",
			args:          `{"id":"42"}`,
			snap:          approvalPolicySnapshot{enabled: true, mode: HITLModeSafeOnly, previewDiff: true},
			definition:    dangerDef,
			hasDefinition: true,
			want:          planPromptGeneral,
			wantRisk:      "danger",
		},
		{
			name:            "permission allow suppresses definition risk",
			toolName:        "mcp__demo__delete_user",
			args:            `{"id":"42"}`,
			snap:            approvalPolicySnapshot{enabled: true, mode: HITLModeSafeOnly, previewDiff: true},
			permissionLevel: "allow",
			definition:      dangerDef,
			hasDefinition:   true,
			want:            planAllow,
		},
		{
			name:            "permission allow does not suppress NeedsReview",
			toolName:        "bash",
			args:            `{"command":"rm -rf /"}`,
			snap:            approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, previewDiff: true},
			permissionLevel: "allow",
			needsReview:     true,
			riskLevel:       "high",
			reviewReason:    "dangerous",
			want:            planPromptGeneral,
			wantRisk:        "high",
		},
		{
			name:            "permission confirm forces general prompt for safe tool",
			toolName:        "mcp__demo__read",
			args:            `{}`,
			snap:            approvalPolicySnapshot{enabled: true, mode: HITLModeAutoReject, previewDiff: true},
			permissionLevel: "confirm",
			definition:      safeDef,
			hasDefinition:   true,
			want:            planPromptGeneral,
			wantRisk:        "high",
		},
		{
			name:     "interactive mutation without previewDiff falls through to allow",
			toolName: "write_file",
			args:     `{"path":"ordinary.txt","content":"new"}`,
			preview:  ordinaryMutation,
			snap:     approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, previewDiff: false},
			want:     planAllow,
		},
		{
			name:     "unchanged mutation content does not require review",
			toolName: "edit_file",
			args:     `{"path":"same.txt"}`,
			preview: tool.ApprovalPreview{
				Mutation: &tool.PreviewRequest{
					Path: "same.txt", OriginalContent: []byte("x"), Content: []byte("x"), Existed: true,
				},
			},
			snap: approvalPolicySnapshot{enabled: true, mode: HITLModeInteractive, previewDiff: true},
			want: planAllow,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var early *approvalPlan
			if tc.earlyReject {
				_, early = permissionRejectPlan(tc.permissionLevel, tc.snap.enabled, tc.toolName)
				if early == nil {
					t.Fatal("expected early reject plan")
				}
			}
			review := resolveReviewRequirement(
				tc.needsReview, tc.riskLevel, tc.reviewReason,
				tc.permissionLevel, tc.snap, tc.definition, tc.hasDefinition,
			)
			plan := decideApprovalPlan(
				tc.toolName,
				json.RawMessage(tc.args),
				tc.preview,
				tc.snap,
				tc.permissionLevel,
				early,
				review,
			)
			if plan.kind != tc.want {
				t.Fatalf("plan kind = %v, want %v", plan.kind, tc.want)
			}
			if tc.want == planPromptGeneral && tc.wantRisk != "" && plan.general.RiskLevel != tc.wantRisk {
				t.Fatalf("general risk = %q, want %q", plan.general.RiskLevel, tc.wantRisk)
			}
		})
	}
}

func TestPermissionRejectPlan(t *testing.T) {
	_, reject := permissionRejectPlan("block", true, "bash")
	if reject == nil || reject.kind != planReject {
		t.Fatalf("block plan = %#v", reject)
	}
	_, reject = permissionRejectPlan("confirm", false, "bash")
	if reject == nil || reject.kind != planReject {
		t.Fatalf("confirm+disabled plan = %#v", reject)
	}
	level, reject := permissionRejectPlan("confirm", true, "bash")
	if level != "confirm" || reject != nil {
		t.Fatalf("confirm+enabled = (%q, %#v)", level, reject)
	}
	level, reject = permissionRejectPlan("allow", true, "bash")
	if level != "allow" || reject != nil {
		t.Fatalf("allow = (%q, %#v)", level, reject)
	}
}
