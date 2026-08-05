package hitlaudit

import (
	"fmt"
	"sync"

	"go-code-agent/internal/security"
	"go-code-agent/internal/tool"
)

// ApprovalReviewer turns a pure approvalPlan into a tool.ApprovalResult,
// performing any required terminal interaction.
type ApprovalReviewer interface {
	Apply(plan approvalPlan) tool.ApprovalResult
}

// terminalApprovalReviewer owns console I/O and serializes prompts so concurrent
// tool calls cannot interleave mutation reviews or general HITL panels.
type terminalApprovalReviewer struct {
	mgr     *HITLManager
	console security.InteractiveIO
	mu      sync.Mutex
}

func newTerminalApprovalReviewer(mgr *HITLManager, console security.InteractiveIO) *terminalApprovalReviewer {
	if console == nil {
		console = security.DefaultInteractiveIO()
	}
	return &terminalApprovalReviewer{mgr: mgr, console: console}
}

func (r *terminalApprovalReviewer) Apply(plan approvalPlan) tool.ApprovalResult {
	switch plan.kind {
	case planAllow:
		return tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	case planReject:
		return planToRejectResult(plan)
	case planPromptMutation:
		if plan.mutation == nil {
			return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "mutation preview missing"}
		}
		return r.reviewMutation(*plan.mutation, plan.previewText)
	case planPromptGeneral:
		return generalApprovalResult(r.requestGeneralApproval(plan.general), plan.toolName, plan.general.Reason)
	default:
		return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "unknown approval plan"}
	}
}

func (r *terminalApprovalReviewer) reviewMutation(request tool.PreviewRequest, previewText string) tool.ApprovalResult {
	var accepted string
	var ok bool

	r.mu.Lock()
	switch {
	case request.Delete:
		ok = security.PreviewDeleteAndConfirm(request.Path, previewText, r.console)
	case !request.Existed:
		accepted, ok = security.PreviewCreateAndConfirm(request.Path, string(request.Content), previewText, r.console)
	default:
		accepted, ok = security.PreviewAndConfirm(
			request.Path,
			string(request.OriginalContent),
			string(request.Content),
			previewText,
			r.console,
		)
	}
	r.mu.Unlock()

	if !ok {
		return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "changes rejected by operator"}
	}
	result := tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	if !request.Delete {
		result.ReplacementContent = &accepted
	}
	return result
}

func (r *terminalApprovalReviewer) requestGeneralApproval(request HITLRequest) HITLResponse {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mgr.RequestApproval(request)
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
