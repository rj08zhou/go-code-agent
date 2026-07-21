package team

import "testing"

func TestProtocolStore_RequestIDReview(t *testing.T) {
	bus := NewBus(t.TempDir())
	ps := NewProtocolStore(bus)
	id, message := SubmitPlanWithID(ps, bus, "alice", "update the parser")
	if id == "" || message == "" {
		t.Fatalf("expected request id and message, got %q %q", id, message)
	}
	if !HasApprovedPlan(ps, "alice") {
		if result := ReviewPlan(ps, bus, id, true, "approved"); result == "" {
			t.Fatal("expected review result")
		}
	}
	if !HasApprovedPlan(ps, "alice") {
		t.Fatal("approved plan was not recorded")
	}
	inbox := bus.ReadInbox("alice")
	if len(inbox) != 1 || inbox[0]["type"] != "plan_approval_response" {
		t.Fatalf("unexpected approval response: %#v", inbox)
	}
}
