package hitlaudit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"go-code-agent/internal/security"
	"go-code-agent/internal/tool"
)

// HITLApprovalAdapter adapts HITLManager to the tool.ApprovalChecker interface,
// enabling the executor to call HITL for human-in-the-loop approval.
type HITLApprovalAdapter struct {
	mgr           *HITLManager
	approval      *security.ApprovalState
	catalog       *tool.ToolCatalog
	permissions   *security.Permissions
	console       security.InteractiveConsole
	interactionMu sync.Mutex
}

type reviewRequirement struct {
	needsReview bool
	riskLevel   string
	reason      string
}

func NewHITLApprovalAdapter(mgr *HITLManager, consoles ...security.InteractiveConsole) *HITLApprovalAdapter {
	console := security.DefaultInteractiveConsole()
	if len(consoles) > 0 && consoles[0] != nil {
		console = consoles[0]
	}
	return &HITLApprovalAdapter{mgr: mgr, console: console}
}

// SetApproval binds the session ApprovalState used by effective approval modes.
func (a *HITLApprovalAdapter) SetApproval(s *security.ApprovalState) { a.approval = s }

// SetCatalog binds the live catalog so definition risk metadata participates
// in the same approval decision as legacy tool-specific rules.
func (a *HITLApprovalAdapter) SetCatalog(c *tool.ToolCatalog) { a.catalog = c }

// SetPermissions binds session-scoped user permission rules.
func (a *HITLApprovalAdapter) SetPermissions(p *security.Permissions) { a.permissions = p }

func (a *HITLApprovalAdapter) shouldPreviewDiff() bool {
	if a.approval == nil {
		return true
	}
	return a.approval.ShouldPreviewDiff()
}

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

func isFileMutation(toolName string) bool {
	switch toolName {
	case "write_file", "edit_file", "insert_file", "delete_file":
		return true
	}
	return false
}

// DecideTool separates policy resolution from the interactive review step. The
// ordering deliberately matches the previous pipeline: permissions, intrinsic
// review requirements, mutation review, then general HITL approval.
func (a *HITLApprovalAdapter) DecideTool(toolName string, args json.RawMessage, preview tool.ApprovalPreview) tool.ApprovalResult {
	permissionLevel, denied := a.resolvePermission(toolName, args)
	if denied.Decision != tool.ApprovalAllowed {
		return denied
	}

	review := a.resolveReviewRequirement(toolName, args, permissionLevel)
	mode := a.mgr.Mode()
	mutationReview := isMutationReview(toolName, preview)
	if a.shouldRunInteractiveMutationReview(mutationReview, mode) {
		return a.reviewMutation(*preview.Mutation, preview.Text)
	}
	if requiresGeneralMutationReview(mutationReview, a.mgr.IsEnabled(), mode, review.needsReview) {
		review = reviewRequirement{
			needsReview: true,
			riskLevel:   "medium",
			reason:      "file mutation requires review",
		}
	}
	if !review.needsReview {
		return tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	}

	if preview.Text != "" {
		review.reason += "\n\nProposed mutation:\n" + preview.Text
	}
	return generalApprovalResult(a.requestGeneralApproval(HITLRequest{
		ToolName:  toolName,
		Arguments: string(args),
		RiskLevel: review.riskLevel,
		Reason:    review.reason,
	}), toolName, review.reason)
}

// resolvePermission applies the permission layer before all other review
// decisions. An allowed result has Decision=ApprovalAllowed and no reason.
func (a *HITLApprovalAdapter) resolvePermission(toolName string, args json.RawMessage) (string, tool.ApprovalResult) {
	if a.permissions == nil {
		return "", tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	}

	level := a.permissions.Match(toolName, string(args))
	switch level {
	case "block":
		return level, tool.ApprovalResult{
			Decision: tool.ApprovalRejected,
			Reason:   fmt.Sprintf("tool %q blocked by user permission rule", toolName),
		}
	case "confirm":
		if !a.mgr.IsEnabled() {
			return level, tool.ApprovalResult{
				Decision: tool.ApprovalRejected,
				Reason:   fmt.Sprintf("tool %q requires HITL confirmation, but HITL is disabled", toolName),
			}
		}
	}
	return level, tool.ApprovalResult{Decision: tool.ApprovalAllowed}
}

func (a *HITLApprovalAdapter) resolveReviewRequirement(toolName string, args json.RawMessage, permissionLevel string) reviewRequirement {
	needsReview, riskLevel, reason := a.mgr.NeedsReview(toolName, string(args))
	if permissionLevel == "confirm" {
		needsReview = true
		riskLevel = "high"
		reason = "user permission rule requires confirmation"
	}

	// RiskLevel used to be metadata only. Make it effective for all
	// definition-backed tools, including dynamically registered MCP tools.
	if !permissionLevelIsAllow(permissionLevel) && a.mgr.IsEnabled() && a.catalog != nil {
		definition, ok := a.catalog.Load().Definitions[toolName]
		if ok {
			if review, level, definitionReason := definitionReview(definition); review {
				needsReview = true
				riskLevel = level
				reason = definitionReason
			}
		}
	}
	return reviewRequirement{needsReview: needsReview, riskLevel: riskLevel, reason: reason}
}

func isMutationReview(toolName string, preview tool.ApprovalPreview) bool {
	if preview.Mutation == nil || !isFileMutation(toolName) {
		return false
	}
	request := preview.Mutation
	return !request.Existed || request.Delete || !bytes.Equal(request.OriginalContent, request.Content)
}

func (a *HITLApprovalAdapter) shouldRunInteractiveMutationReview(mutationReview bool, mode HITLMode) bool {
	promptingMode := mode == HITLModeInteractive || mode == HITLModeSafeOnly
	return mutationReview && a.mgr.IsEnabled() && promptingMode && a.shouldPreviewDiff()
}

func requiresGeneralMutationReview(mutationReview, hitlEnabled bool, mode HITLMode, alreadyRequired bool) bool {
	return mutationReview && hitlEnabled &&
		(mode == HITLModeAutoReject || mode == HITLModeNotifyOnly) && !alreadyRequired
}

func (a *HITLApprovalAdapter) reviewMutation(request tool.PreviewRequest, previewText string) tool.ApprovalResult {
	var accepted string
	var ok bool

	a.interactionMu.Lock()
	switch {
	case request.Delete:
		ok = security.PreviewDeleteAndConfirm(request.Path, previewText, a.console)
	case !request.Existed:
		accepted, ok = security.PreviewCreateAndConfirm(request.Path, string(request.Content), previewText, a.console)
	default:
		accepted, ok = security.PreviewAndConfirm(
			request.Path,
			string(request.OriginalContent),
			string(request.Content),
			previewText,
			a.console,
		)
	}
	a.interactionMu.Unlock()

	if !ok {
		return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "changes rejected by operator"}
	}
	result := tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	if !request.Delete {
		result.ReplacementContent = &accepted
	}
	return result
}

func (a *HITLApprovalAdapter) requestGeneralApproval(request HITLRequest) HITLResponse {
	a.interactionMu.Lock()
	defer a.interactionMu.Unlock()
	return a.mgr.RequestApproval(request)
}

func generalApprovalResult(response HITLResponse, toolName, reason string) tool.ApprovalResult {
	switch response.Decision {
	case HITLApprove:
		return tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	case HITLReject:
		return tool.ApprovalResult{
			Decision: tool.ApprovalRejected,
			Reason:   fmt.Sprintf("HITL rejected %s: %s", toolName, reason),
		}
	case HITLModify:
		return tool.ApprovalResult{Decision: tool.ApprovalModified, Feedback: response.Feedback}
	default:
		return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "unknown HITL decision"}
	}
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
