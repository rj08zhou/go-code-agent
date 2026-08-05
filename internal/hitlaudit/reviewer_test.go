package hitlaudit

import (
	"testing"

	"go-code-agent/internal/tool"
)

type stubReviewer struct {
	last approvalPlan
	out  tool.ApprovalResult
}

func (s *stubReviewer) Apply(plan approvalPlan) tool.ApprovalResult {
	s.last = plan
	return s.out
}

func TestAdapterDelegatesPlanToReviewer(t *testing.T) {
	mgr := NewHITLManager(nil)
	mgr.SetEnabled(true)
	mgr.SetMode(HITLModeAutoApprove)
	adapter := NewHITLApprovalAdapter(mgr)
	stub := &stubReviewer{out: tool.ApprovalResult{Decision: tool.ApprovalAllowed}}
	adapter.reviewer = stub

	result := adapter.DecideTool("read_file", nil, tool.ApprovalPreview{})
	if result.Decision != tool.ApprovalAllowed {
		t.Fatalf("result = %#v", result)
	}
	if stub.last.kind != planAllow {
		t.Fatalf("reviewer saw plan kind %v, want allow", stub.last.kind)
	}
}

func TestTerminalReviewerRejectsMissingMutation(t *testing.T) {
	mgr := NewHITLManager(nil)
	reviewer := newTerminalApprovalReviewer(mgr, nil)
	result := reviewer.Apply(approvalPlan{kind: planPromptMutation, toolName: "write_file"})
	if result.Decision != tool.ApprovalRejected {
		t.Fatalf("result = %#v, want rejected", result)
	}
}
