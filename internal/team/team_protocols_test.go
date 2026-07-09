package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ----------------------------------------------------------------------------
// Shutdown protocol
// ----------------------------------------------------------------------------

func TestPostShutdownRequest(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	bus := NewBus(dir)

	id := PostShutdownRequest(ps, bus, "alice")
	if id == "" {
		t.Fatal("expected non-empty request id")
	}

	req, ok := ps.requests[id]
	if !ok {
		t.Fatalf("request %s not stored", id)
	}
	if req.Kind != "shutdown" || req.Target != "alice" || req.Status != "pending" {
		t.Errorf("unexpected request state: %+v", req)
	}

	inbox := bus.ReadInbox("alice")
	if len(inbox) != 1 {
		t.Fatalf("expected 1 message in alice's inbox, got %d", len(inbox))
	}
	if inbox[0]["type"] != "shutdown_request" {
		t.Errorf("message type = %v, want shutdown_request", inbox[0]["type"])
	}
}

func TestHandleShutdownReq_FormatsMessage(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	bus := NewBus(dir)

	msg := HandleShutdownReq(ps, bus, "bob")
	if msg == "" || msg[:8] != "Shutdown" {
		t.Errorf("unexpected message: %q", msg)
	}
}

func TestHandleShutdownReq_NilGuards(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	bus := NewBus(dir)

	if got := HandleShutdownReq(nil, bus, "bob"); got != "Error: no active session" {
		t.Errorf("nil store: got %q", got)
	}
	if got := HandleShutdownReq(ps, nil, "bob"); got != "Error: no active session" {
		t.Errorf("nil bus: got %q", got)
	}
}

// ----------------------------------------------------------------------------
// Plan approval protocol
// ----------------------------------------------------------------------------

func TestSubmitPlan_And_Approve(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	bus := NewBus(dir)

	msg := SubmitPlan(ps, bus, "alice", "do the thing")
	if msg == "" {
		t.Fatal("expected non-empty response")
	}

	// Lead should have received the plan_submission message.
	leadInbox := bus.ReadInbox("lead")
	if len(leadInbox) != 1 || leadInbox[0]["type"] != "plan_submission" {
		t.Fatalf("unexpected lead inbox: %+v", leadInbox)
	}
	reqID, _ := leadInbox[0]["request_id"].(string)
	if reqID == "" {
		t.Fatal("expected request_id in plan_submission message")
	}

	if HasApprovedPlan(ps, "alice") {
		t.Fatal("plan should not be approved yet")
	}

	result := HandlePlanReview(ps, bus, reqID, true, "looks good")
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	if !HasApprovedPlan(ps, "alice") {
		t.Fatal("expected plan to be approved after HandlePlanReview(approve=true)")
	}

	// Alice should have received the plan_approval_response.
	aliceInbox := bus.ReadInbox("alice")
	if len(aliceInbox) != 1 || aliceInbox[0]["type"] != "plan_approval_response" {
		t.Fatalf("unexpected alice inbox: %+v", aliceInbox)
	}
	if approve, _ := aliceInbox[0]["approve"].(bool); !approve {
		t.Errorf("expected approve=true in response payload")
	}
}

func TestSubmitPlan_And_Reject(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	bus := NewBus(dir)

	SubmitPlan(ps, bus, "carol", "risky plan")
	leadInbox := bus.ReadInbox("lead")
	reqID, _ := leadInbox[0]["request_id"].(string)

	HandlePlanReview(ps, bus, reqID, false, "needs more detail")

	if HasApprovedPlan(ps, "carol") {
		t.Fatal("rejected plan must not count as approved")
	}
	if ps.requests[reqID].Status != "rejected" {
		t.Errorf("status = %q, want rejected", ps.requests[reqID].Status)
	}
}

func TestHandlePlanReview_UnknownID(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	bus := NewBus(dir)

	got := HandlePlanReview(ps, bus, "req_does_not_exist", true, "")
	if got != "Error: Unknown plan request_id" {
		t.Errorf("got %q, want unknown-id error", got)
	}
}

func TestSubmitPlan_NilGuards(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	bus := NewBus(dir)

	if got := SubmitPlan(nil, bus, "alice", "x"); got != "Error: no active session" {
		t.Errorf("nil store: got %q", got)
	}
	if got := SubmitPlan(ps, nil, "alice", "x"); got != "Error: no active session" {
		t.Errorf("nil bus: got %q", got)
	}
}

func TestHasApprovedPlan_NilOrUnknown(t *testing.T) {
	if HasApprovedPlan(nil, "alice") {
		t.Error("nil store should never report an approved plan")
	}

	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	if HasApprovedPlan(ps, "nobody") {
		t.Error("teammate with no plans should not be approved")
	}
}

// ----------------------------------------------------------------------------
// Persistence across process restarts
// ----------------------------------------------------------------------------

func TestProtocolStore_PersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)
	bus := NewBus(dir)

	SubmitPlan(ps, bus, "dave", "persisted plan")
	leadInbox := bus.ReadInbox("lead")
	reqID, _ := leadInbox[0]["request_id"].(string)
	HandlePlanReview(ps, bus, reqID, true, "ok")

	// Reload from disk into a fresh store rooted at the same dir.
	ps2 := NewProtocolStore(dir)
	if !HasApprovedPlan(ps2, "dave") {
		t.Fatal("expected approved plan to survive reload from disk")
	}
}

// ----------------------------------------------------------------------------
// TTL pruning (white-box: directly manipulate timestamps)
// ----------------------------------------------------------------------------

func TestHasApprovedPlan_PrunesExpiredApproval(t *testing.T) {
	dir := t.TempDir()
	ps := NewProtocolStore(dir)

	ps.mu.Lock()
	id := ps.nextReqIDLocked()
	ps.requests[id] = &protocolRequest{
		ID: id, Kind: "plan", From: "erin", Plan: "old",
		Status:    "approved",
		CreatedAt: time.Now().Add(-48 * time.Hour).Unix(),
		UpdatedAt: time.Now().Add(-25 * time.Hour).Unix(), // older than ApprovedPlanTTL (24h)
	}
	ps.mu.Unlock()

	if HasApprovedPlan(ps, "erin") {
		t.Fatal("expired approved plan should be pruned and not count as approved")
	}
	if _, ok := ps.requests[id]; ok {
		t.Error("expired request should have been removed from the store")
	}
}

func TestProtocolStore_PrunesExpiredPendingOnLoad(t *testing.T) {
	dir := t.TempDir()

	// Manually seed a stale pending shutdown request on disk (older
	// than PlanRequestTTL of 30 min) and confirm it's pruned on load.
	stale := struct {
		Counter  int                         `json:"counter"`
		Requests map[string]*protocolRequest `json:"requests"`
	}{
		Counter: 1,
		Requests: map[string]*protocolRequest{
			"req_1": {
				ID: "req_1", Kind: "shutdown", Target: "frank",
				Status:    "pending",
				CreatedAt: time.Now().Add(-2 * time.Hour).Unix(),
				UpdatedAt: time.Now().Add(-2 * time.Hour).Unix(),
			},
		},
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal seed data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, protocolFileName), data, 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}

	ps := NewProtocolStore(dir)
	if _, ok := ps.requests["req_1"]; ok {
		t.Error("stale pending request should be pruned on load")
	}
}
