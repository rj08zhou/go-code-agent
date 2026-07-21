package tool

import (
	"context"
	"go-code-agent-refactor/internal/llm"
	"testing"
)

type protocolToolFake struct {
	shutdown      string
	submitted     string
	reviewID      string
	reviewApprove bool
}

func (p *protocolToolFake) ShutdownRequest(name string) string {
	p.shutdown = name
	return "shutdown queued"
}
func (p *protocolToolFake) SubmitPlan(agent, plan string) string {
	p.submitted = agent + ":" + plan
	return "plan queued"
}
func (p *protocolToolFake) ReviewPlan(id string, approve bool, feedback string) string {
	p.reviewID, p.reviewApprove = id, approve
	return "plan reviewed"
}

func TestProtocolTools_DelegateToProtocolService(t *testing.T) {
	fake := &protocolToolFake{}
	defs := BuiltinTools(nil, nil, nil, nil, nil, nil, nil, nil, fake, nil)
	catalog := NewToolCatalog()
	catalog.RegisterAll(defs)
	exec := NewExecutor(catalog, nil, nil)
	scope := &ToolScope{Role: "lead", AgentID: "lead", CanTeam: true}

	shutdown := exec.Execute(context.Background(), scope, toolCall("shutdown_request", `{"teammate":"alice"}`))
	if shutdown.Status != StatusSucceeded || fake.shutdown != "alice" {
		t.Fatalf("shutdown not delegated: %#v %#v", shutdown, fake)
	}

	approval := exec.Execute(context.Background(), scope, toolCall("plan_approval", `{"request_id":"plan_1","approve":true,"feedback":"ok"}`))
	if approval.Status != StatusSucceeded || fake.reviewID != "plan_1" || !fake.reviewApprove {
		t.Fatalf("approval not delegated: %#v %#v", approval, fake)
	}
}

func TestProtocolTools_ApprovalRequiresLead(t *testing.T) {
	fake := &protocolToolFake{}
	defs := BuiltinTools(nil, nil, nil, nil, nil, nil, nil, nil, fake, nil)
	catalog := NewToolCatalog()
	catalog.RegisterAll(defs)
	exec := NewExecutor(catalog, nil, nil)
	result := exec.Execute(context.Background(), &ToolScope{Role: "teammate", CanTeam: true}, toolCall("plan_approval", `{"request_id":"plan_1","approve":true}`))
	if result.Status != StatusDenied {
		t.Fatalf("expected denied, got %s", result.Status)
	}
}

func toolCall(name, args string) llm.ToolCall { return llm.ToolCall{Name: name, Arguments: args} }
