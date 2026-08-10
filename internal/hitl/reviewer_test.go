package hitl

import (
	"testing"

	"go-code-agent/internal/tool"
)

type stubReviewer struct {
	last approvalDecision
	out  tool.ApprovalResult
}

func (s *stubReviewer) Apply(decision approvalDecision) tool.ApprovalResult {
	s.last = decision
	return s.out
}

func TestAdapterDelegatesPlanToReviewer(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	mgr.SetMode(HITLModeAutoApprove)
	adapter := NewHITLApprovalAdapter(mgr)
	stub := &stubReviewer{out: tool.ApprovalResult{Decision: tool.ApprovalAllowed}}
	adapter.reviewer = stub

	result := adapter.DecideTool("read_file", nil, tool.MutationApprovalInput{})
	if result.Decision != tool.ApprovalAllowed {
		t.Fatalf("result = %#v", result)
	}
	if stub.last.kind != decisionAllow {
		t.Fatalf("reviewer saw decision kind %v, want allow", stub.last.kind)
	}
}

func TestTerminalReviewerRejectsMissingMutation(t *testing.T) {
	mgr := NewHITLManager(nil)
	reviewer := newTerminalApprovalReviewer(mgr, nil)
	result := reviewer.Apply(approvalDecision{kind: decisionPromptMutation, toolName: "write_file"})
	if result.Decision != tool.ApprovalRejected {
		t.Fatalf("result = %#v, want rejected", result)
	}
}
