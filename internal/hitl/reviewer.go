package hitl

import (
	"fmt"
	"sync"

	"go-code-agent/internal/tool"
)

// ApprovalReviewer turns a pure approvalDecision into a tool.ApprovalResult,
// performing any required terminal interaction.
type ApprovalReviewer interface {
	Apply(decision approvalDecision) tool.ApprovalResult
}

// terminalApprovalReviewer owns console I/O and serializes prompts so concurrent
// tool calls cannot interleave mutation reviews or general HITL panels.
type terminalApprovalReviewer struct {
	mgr     *HITLManager
	console InteractiveIO
	mu      sync.Mutex
}

func newTerminalApprovalReviewer(mgr *HITLManager, console InteractiveIO) *terminalApprovalReviewer {
	if console == nil {
		console = DefaultInteractiveIO()
	}
	return &terminalApprovalReviewer{mgr: mgr, console: console}
}

func (r *terminalApprovalReviewer) Apply(decision approvalDecision) tool.ApprovalResult {
	switch decision.kind {
	case decisionAllow:
		return tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	case decisionReject:
		return decisionToRejectResult(decision)
	case decisionPromptMutation:
		if decision.mutation == nil {
			return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "mutation plan missing"}
		}
		return r.reviewMutation(*decision.mutation, decision.diffText)
	case decisionPromptGeneral:
		return generalApprovalResult(r.requestGeneralApproval(decision.general), decision.toolName, decision.general.Reason)
	default:
		return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "unknown approval decision"}
	}
}

func (r *terminalApprovalReviewer) reviewMutation(mutation tool.MutationPlan, diffText string) tool.ApprovalResult {
	var accepted string
	var ok bool

	r.mu.Lock()
	switch {
	case mutation.Delete:
		ok = PreviewDeleteAndConfirm(mutation.Path, diffText, r.console)
	case !mutation.Existed:
		accepted, ok = PreviewCreateAndConfirm(mutation.Path, string(mutation.Content), diffText, r.console)
	default:
		accepted, ok = PreviewAndConfirm(
			mutation.Path,
			string(mutation.OriginalContent),
			string(mutation.Content),
			diffText,
			r.console,
		)
	}
	r.mu.Unlock()

	if !ok {
		return tool.ApprovalResult{Decision: tool.ApprovalRejected, Reason: "changes rejected by operator"}
	}
	result := tool.ApprovalResult{Decision: tool.ApprovalAllowed}
	if !mutation.Delete {
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
