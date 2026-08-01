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
	interactionMu sync.Mutex
}

func NewHITLApprovalAdapter(mgr *HITLManager) *HITLApprovalAdapter {
	return &HITLApprovalAdapter{mgr: mgr}
}

// SetApproval binds the session ApprovalState used by effective approval modes.
func (a *HITLApprovalAdapter) SetApproval(s *security.ApprovalState) { a.approval = s }

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

func (a *HITLApprovalAdapter) DecideTool(toolName string, args json.RawMessage, preview tool.ApprovalPreview) tool.ApprovalResult {
	needsReview, riskLevel, reason := a.mgr.NeedsReview(toolName, string(args))

	// Only prompting modes perform interactive file review. Reject and
	// notify-only preserve their non-interactive semantics.
	mode := a.mgr.Mode()
	mutationPreview := false
	if preview.Mutation != nil && isFileMutation(toolName) {
		request := preview.Mutation
		mutationPreview = !request.Existed || request.Delete ||
			!bytes.Equal(request.OriginalContent, request.Content)
	}
	promptingMode := mode == HITLModeInteractive || mode == HITLModeSafeOnly
	if mutationPreview && a.mgr.IsEnabled() && promptingMode && a.shouldPreviewDiff() {
		request := *preview.Mutation
		var accepted string
		var ok bool

		a.interactionMu.Lock()
		switch {
		case request.Delete:
			ok = security.PreviewDeleteAndConfirm(request.Path, preview.Text)
		case !request.Existed:
			accepted, ok = security.PreviewCreateAndConfirm(request.Path, string(request.Content), preview.Text)
		default:
			accepted, ok = security.PreviewAndConfirm(
				request.Path,
				string(request.OriginalContent),
				string(request.Content),
				preview.Text,
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
	if mutationPreview && a.mgr.IsEnabled() &&
		(mode == HITLModeAutoReject || mode == HITLModeNotifyOnly) && !needsReview {
		needsReview = true
		riskLevel = "medium"
		reason = "file mutation requires review"
	}
	if !needsReview {
		return tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	}
	if preview.Text != "" {
		reason += "\n\nProposed mutation:\n" + preview.Text
	}

	a.interactionMu.Lock()
	resp := a.mgr.RequestApproval(HITLRequest{
		ToolName:  toolName,
		Arguments: string(args),
		RiskLevel: riskLevel,
		Reason:    reason,
	})
	a.interactionMu.Unlock()

	switch resp.Decision {
	case HITLApprove:
		return tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	case HITLReject:
		return tool.ApprovalResult{
			Decision: tool.ApprovalRejected,
			Reason:   fmt.Sprintf("HITL rejected %s: %s", toolName, reason),
		}
	case HITLModify:
		return tool.ApprovalResult{Decision: tool.ApprovalModified, Feedback: resp.Feedback}
	default:
		return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "unknown HITL decision"}
	}
}
