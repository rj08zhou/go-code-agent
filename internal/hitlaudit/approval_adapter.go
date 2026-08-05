package hitlaudit

import (
	"encoding/json"

	"go-code-agent/internal/security"
	"go-code-agent/internal/tool"
)

// HITLApprovalAdapter adapts HITLManager to the tool.ApprovalChecker interface,
// enabling the executor to call HITL for human-in-the-loop approval.
//
// Policy resolution is pure (see policy.go); terminal interaction is delegated
// to ApprovalReviewer.
type HITLApprovalAdapter struct {
	mgr         *HITLManager
	approval    *security.ApprovalState
	catalog     *tool.ToolCatalog
	permissions *security.Permissions
	reviewer    ApprovalReviewer
}

type reviewRequirement struct {
	needsReview bool
	riskLevel   string
	reason      string
}

func NewHITLApprovalAdapter(mgr *HITLManager, consoles ...security.InteractiveIO) *HITLApprovalAdapter {
	console := security.DefaultInteractiveIO()
	if len(consoles) > 0 && consoles[0] != nil {
		console = consoles[0]
	}
	return &HITLApprovalAdapter{
		mgr:      mgr,
		reviewer: newTerminalApprovalReviewer(mgr, console),
	}
}

// SetApproval binds the session ApprovalState used by effective approval modes.
func (a *HITLApprovalAdapter) SetApproval(s *security.ApprovalState) { a.approval = s }

// SetCatalog binds the live catalog so definition risk metadata participates
// in the same approval decision as legacy tool-specific rules.
func (a *HITLApprovalAdapter) SetCatalog(c *tool.ToolCatalog) { a.catalog = c }

// SetPermissions binds session-scoped user permission rules.
func (a *HITLApprovalAdapter) SetPermissions(p *security.Permissions) { a.permissions = p }

func (a *HITLApprovalAdapter) AllowTool(toolName string, args json.RawMessage) (bool, string) {
	result := a.DecideTool(toolName, args, tool.ApprovalPreview{})
	if result.Decision == tool.ApprovalModified {
		return false, result.Feedback
	}
	return result.Decision == tool.ApprovalAllowed, result.Reason
}

func (a *HITLApprovalAdapter) AllowToolWithPreview(toolName string, args json.RawMessage, preview string) (bool, string) {
	result := a.DecideTool(toolName, args, tool.ApprovalPreview{Text: preview})
	if result.Decision == tool.ApprovalModified {
		return false, result.Feedback
	}
	return result.Decision == tool.ApprovalAllowed, result.Reason
}

// DecideTool separates policy resolution from the interactive review step. The
// ordering deliberately matches the previous pipeline: permissions, intrinsic
// review requirements, mutation review, then general HITL approval.
func (a *HITLApprovalAdapter) DecideTool(toolName string, args json.RawMessage, preview tool.ApprovalPreview) tool.ApprovalResult {
	snap := a.policySnapshot()
	permissionLevel, earlyReject := a.resolvePermissionPlan(toolName, args, snap.enabled)
	review := a.resolveReviewRequirementFor(toolName, args, permissionLevel, snap)
	plan := decideApprovalPlan(toolName, args, preview, snap, permissionLevel, earlyReject, review)
	return a.reviewer.Apply(plan)
}

func (a *HITLApprovalAdapter) resolvePermissionPlan(toolName string, args json.RawMessage, hitlEnabled bool) (string, *approvalPlan) {
	if a.permissions == nil {
		return "", nil
	}
	level := a.permissions.Match(toolName, string(args))
	return permissionRejectPlan(level, hitlEnabled, toolName)
}

func (a *HITLApprovalAdapter) resolveReviewRequirementFor(
	toolName string,
	args json.RawMessage,
	permissionLevel string,
	snap approvalPolicySnapshot,
) reviewRequirement {
	needsReview, riskLevel, reason := false, "", ""
	if snap.enabled {
		needsReview, riskLevel, reason = a.mgr.classifyReview(toolName, string(args))
	}
	var definition tool.ToolDefinition
	hasDefinition := false
	if a.catalog != nil {
		definition, hasDefinition = a.catalog.Load().Definitions[toolName]
	}
	return resolveReviewRequirement(needsReview, riskLevel, reason, permissionLevel, snap, definition, hasDefinition)
}
