package hitlaudit

import (
	"bytes"
	"encoding/json"
	"fmt"

	"go-code-agent/internal/tool"
)

// approvalPlanKind is the pure policy outcome before any terminal I/O.
type approvalPlanKind int

const (
	planAllow approvalPlanKind = iota
	planReject
	planPromptMutation
	planPromptGeneral
)

// approvalPlan is a pure decision: allow/reject immediately, or ask the
// interactive reviewer (mutation diff or general y/n/m panel).
type approvalPlan struct {
	kind        approvalPlanKind
	reason      string
	toolName    string
	mutation    *tool.PreviewRequest
	previewText string
	general     HITLRequest
}

// approvalPolicySnapshot freezes mode-related inputs for one DecideTool call
// so permission/review/mutation branches cannot observe mid-flight mode flips.
type approvalPolicySnapshot struct {
	enabled     bool
	mode        HITLMode
	previewDiff bool
}

func (a *HITLApprovalAdapter) policySnapshot() approvalPolicySnapshot {
	previewDiff := true
	if a.approval != nil {
		previewDiff = a.approval.ShouldPreviewDiff()
	}
	return approvalPolicySnapshot{
		enabled:     a.mgr.IsEnabled(),
		mode:        a.mgr.Mode(),
		previewDiff: previewDiff,
	}
}

// decideApprovalPlan resolves the approval pipeline without terminal I/O.
// Ordering matches the historical DecideTool path: permissions (already
// applied into permissionLevel / early reject), intrinsic review, mutation
// interactive review, general mutation review, then allow or general prompt.
func decideApprovalPlan(
	toolName string,
	args json.RawMessage,
	preview tool.ApprovalPreview,
	snap approvalPolicySnapshot,
	permissionLevel string,
	earlyReject *approvalPlan,
	review reviewRequirement,
) approvalPlan {
	if earlyReject != nil {
		return *earlyReject
	}

	mutationReview := isMutationReview(toolName, preview)
	if shouldRunInteractiveMutationReview(mutationReview, snap) {
		return approvalPlan{
			kind:        planPromptMutation,
			toolName:    toolName,
			mutation:    preview.Mutation,
			previewText: preview.Text,
		}
	}
	if requiresGeneralMutationReview(mutationReview, snap.enabled, snap.mode, review.needsReview) {
		review = reviewRequirement{
			needsReview: true,
			riskLevel:   "medium",
			reason:      "file mutation requires review",
		}
	}
	if !review.needsReview {
		return approvalPlan{kind: planAllow, toolName: toolName}
	}

	reason := review.reason
	if preview.Text != "" {
		reason += "\n\nProposed mutation:\n" + preview.Text
	}
	return approvalPlan{
		kind:     planPromptGeneral,
		toolName: toolName,
		reason:   reason,
		general: HITLRequest{
			ToolName:  toolName,
			Arguments: string(args),
			RiskLevel: review.riskLevel,
			Reason:    reason,
		},
	}
}

func permissionRejectPlan(level string, hitlEnabled bool, toolName string) (permissionLevel string, reject *approvalPlan) {
	switch level {
	case "block":
		return level, &approvalPlan{
			kind:     planReject,
			toolName: toolName,
			reason:   fmt.Sprintf("tool %q blocked by user permission rule", toolName),
		}
	case "confirm":
		if !hitlEnabled {
			return level, &approvalPlan{
				kind:     planReject,
				toolName: toolName,
				reason:   fmt.Sprintf("tool %q requires HITL confirmation, but HITL is disabled", toolName),
			}
		}
	}
	return level, nil
}

func resolveReviewRequirement(
	needsReview bool,
	riskLevel, reason string,
	permissionLevel string,
	snap approvalPolicySnapshot,
	definition tool.ToolDefinition,
	hasDefinition bool,
) reviewRequirement {
	if permissionLevel == "confirm" {
		needsReview = true
		riskLevel = "high"
		reason = "user permission rule requires confirmation"
	}

	// RiskLevel used to be metadata only. Make it effective for all
	// definition-backed tools, including dynamically registered MCP tools.
	if !permissionLevelIsAllow(permissionLevel) && snap.enabled && hasDefinition {
		if review, level, definitionReason := definitionReview(definition); review {
			needsReview = true
			riskLevel = level
			reason = definitionReason
		}
	}
	return reviewRequirement{needsReview: needsReview, riskLevel: riskLevel, reason: reason}
}

func isFileMutation(toolName string) bool {
	switch toolName {
	case "write_file", "edit_file", "insert_file", "delete_file":
		return true
	}
	return false
}

func isMutationReview(toolName string, preview tool.ApprovalPreview) bool {
	if preview.Mutation == nil || !isFileMutation(toolName) {
		return false
	}
	request := preview.Mutation
	return !request.Existed || request.Delete || !bytes.Equal(request.OriginalContent, request.Content)
}

func shouldRunInteractiveMutationReview(mutationReview bool, snap approvalPolicySnapshot) bool {
	promptingMode := snap.mode == HITLModeInteractive || snap.mode == HITLModeSafeOnly
	return mutationReview && snap.enabled && promptingMode && snap.previewDiff
}

func requiresGeneralMutationReview(mutationReview, hitlEnabled bool, mode HITLMode, alreadyRequired bool) bool {
	return mutationReview && hitlEnabled &&
		(mode == HITLModeAutoReject || mode == HITLModeNotifyOnly) && !alreadyRequired
}

func permissionLevelIsAllow(level string) bool { return level == "allow" }

func definitionReview(definition tool.ToolDefinition) (bool, string, string) {
	if !definition.Effects.Declared() || definition.HasEffect(tool.EffectUnclassified) {
		return true, "high", "tool effects are not classified; explicit review is required"
	}
	switch definition.RiskLevel {
	case tool.RiskDanger:
		return true, "danger", "tool is classified as dangerous"
	case tool.RiskInteractive:
		return true, "high", "tool requires interactive approval"
	default:
		return false, "", ""
	}
}

func planToRejectResult(plan approvalPlan) tool.ApprovalResult {
	return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: plan.reason}
}
