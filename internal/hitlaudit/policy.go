package hitlaudit

import (
	"bytes"
	"encoding/json"
	"fmt"

	"go-code-agent/internal/tool"
)

// approvalDecisionKind is the pure policy outcome before any terminal I/O.
type approvalDecisionKind int

const (
	decisionAllow approvalDecisionKind = iota
	decisionReject
	decisionPromptMutation
	decisionPromptGeneral
)

// approvalDecision is a pure decision: allow/reject immediately, or ask the
// interactive reviewer (mutation diff UI or general y/n/m panel).
type approvalDecision struct {
	kind     approvalDecisionKind
	reason   string
	toolName string
	mutation *tool.MutationPlan
	diffText string
	general  HITLRequest
}

// approvalPolicySnapshot freezes mode-related inputs for one DecideTool call
// so permission/review/mutation branches cannot observe mid-flight mode flips.
type approvalPolicySnapshot struct {
	enabled    bool
	mode       HITLMode
	showDiffUI bool
}

func (a *HITLApprovalAdapter) policySnapshot() approvalPolicySnapshot {
	showDiffUI := true
	if a.approval != nil {
		showDiffUI = a.approval.ShouldShowDiffUI()
	}
	return approvalPolicySnapshot{
		enabled:    a.mgr.IsEnabled(),
		mode:       a.mgr.Mode(),
		showDiffUI: showDiffUI,
	}
}

// decideApprovalDecision resolves the approval pipeline without terminal I/O.
// Ordering matches the historical DecideTool path: permissions (already
// applied into permissionLevel / early reject), intrinsic review, mutation
// interactive review, general mutation review, then allow or general prompt.
func decideApprovalDecision(
	toolName string,
	args json.RawMessage,
	approvalInput tool.MutationApprovalInput,
	snap approvalPolicySnapshot,
	permissionLevel string,
	earlyReject *approvalDecision,
	review reviewRequirement,
) approvalDecision {
	if earlyReject != nil {
		return *earlyReject
	}

	hasMutation := hasMaterialFileMutation(toolName, approvalInput)
	if shouldOpenInteractiveDiffReview(hasMutation, snap) {
		return approvalDecision{
			kind:     decisionPromptMutation,
			toolName: toolName,
			mutation: approvalInput.Plan,
			diffText: approvalInput.DiffText,
		}
	}
	if shouldRouteMutationToGeneralApproval(hasMutation, snap.enabled, snap.mode, review.needsReview) {
		review = reviewRequirement{
			needsReview: true,
			severity:    SeverityMedium,
			reason:      "file mutation requires review",
		}
	}
	if !review.needsReview {
		return approvalDecision{kind: decisionAllow, toolName: toolName}
	}

	reason := review.reason
	if approvalInput.DiffText != "" {
		reason += "\n\nProposed mutation:\n" + approvalInput.DiffText
	}
	return approvalDecision{
		kind:     decisionPromptGeneral,
		toolName: toolName,
		reason:   reason,
		general: HITLRequest{
			ToolName:  toolName,
			Arguments: string(args),
			Severity:  review.severity,
			Reason:    reason,
		},
	}
}

func permissionRejectDecision(level string, hitlEnabled bool, toolName string) (permissionLevel string, reject *approvalDecision) {
	switch level {
	case "block":
		return level, &approvalDecision{
			kind:     decisionReject,
			toolName: toolName,
			reason:   fmt.Sprintf("tool %q blocked by user permission rule", toolName),
		}
	case "confirm":
		if !hitlEnabled {
			return level, &approvalDecision{
				kind:     decisionReject,
				toolName: toolName,
				reason:   fmt.Sprintf("tool %q requires HITL confirmation, but HITL is disabled", toolName),
			}
		}
	}
	return level, nil
}

func resolveReviewRequirement(
	review reviewRequirement,
	permissionLevel string,
	snap approvalPolicySnapshot,
	definition tool.ToolDefinition,
	hasDefinition bool,
) reviewRequirement {
	if permissionLevel == "confirm" {
		// An explicit user rule outranks any classification, including one
		// derived from the command itself. Return immediately so the static
		// tool RiskLevel cannot overwrite the user-facing reason.
		return reviewRequirement{
			needsReview: true,
			severity:    SeverityHigh,
			reason:      "user permission rule requires confirmation",
		}
	}

	// A shell tool declares the worst case its command could do, so "bash" is
	// RiskDanger even for `ls`. Once ClassifyCommand has judged this call,
	// that per-call result stands for Safe/Caution/Danger/Deny alike.
	if review.commandClassified {
		return review
	}

	// RiskLevel used to be metadata only. Make it effective for all
	// definition-backed tools, including dynamically registered MCP tools.
	// This is the fallback when there is no per-call classifier (e.g. MCP).
	if !permissionLevelIsAllow(permissionLevel) && snap.enabled && hasDefinition {
		if needsReview, severity, definitionReason := definitionReview(definition); needsReview {
			review = reviewRequirement{needsReview: true, severity: severity, reason: definitionReason}
		}
	}
	return review
}

func isFileMutation(toolName string) bool {
	switch toolName {
	case "write_file", "edit_file", "insert_file", "delete_file":
		return true
	}
	return false
}

func hasMaterialFileMutation(toolName string, approvalInput tool.MutationApprovalInput) bool {
	if approvalInput.Plan == nil || !isFileMutation(toolName) {
		return false
	}
	mutation := approvalInput.Plan
	return !mutation.Existed || mutation.Delete || !bytes.Equal(mutation.OriginalContent, mutation.Content)
}

func shouldOpenInteractiveDiffReview(hasMutation bool, snap approvalPolicySnapshot) bool {
	promptingMode := snap.mode == HITLModeInteractive || snap.mode == HITLModeSafeAuto
	return hasMutation && snap.enabled && promptingMode && snap.showDiffUI
}

func shouldRouteMutationToGeneralApproval(hasMutation, hitlEnabled bool, mode HITLMode, alreadyRequiresReview bool) bool {
	return hasMutation && hitlEnabled &&
		(mode == HITLModeAutoReject || mode == HITLModeNotifyOnly) && !alreadyRequiresReview
}

func permissionLevelIsAllow(level string) bool { return level == "allow" }

func definitionReview(definition tool.ToolDefinition) (bool, ReviewSeverity, string) {
	if !definition.Effects.Declared() || definition.HasEffect(tool.EffectUnclassified) {
		return true, SeverityHigh, "tool effects are not classified; explicit review is required"
	}
	switch definition.RiskLevel {
	case tool.RiskDanger:
		return true, SeverityDanger, "tool is classified as dangerous"
	case tool.RiskInteractive:
		return true, SeverityHigh, "tool requires interactive approval"
	default:
		return false, SeverityNone, ""
	}
}

func decisionToRejectResult(decision approvalDecision) tool.ApprovalResult {
	return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: decision.reason}
}
