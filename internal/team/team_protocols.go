package team

import (
	"encoding/json"
	"fmt"
	"go-code-agent/infra"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ProtocolStore - shutdown + plan approval protocol state
//
// Persistence:
//   State is mirrored to {sessionDir}/team/protocols.json so that
//   pending/approved requests survive process restarts.
//
// TTL:
//   Pending requests are pruned after planRequestTTL.
//   Approved/rejected requests are pruned after approvedPlanTTL (much
//   longer, so teammates can still use their approval for follow-up
//   work).
//
// Ownership:
//   Each Session owns its own ProtocolStore (see session.go).
//   Tool handlers reach it through app.Protocols. When the active
//   session changes, app.Protocols is rebound atomically by
//   Session.Activate.

const protocolFileName = "protocols.json"

type protocolRequest struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`           // "shutdown" | "plan"
	From      string `json:"from,omitempty"` // teammate name (for plan) or "lead" (for shutdown)
	Target    string `json:"target,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Status    string `json:"status"` // pending | approved | rejected
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type ProtocolStore struct {
	mu       sync.Mutex
	path     string
	counter  int
	requests map[string]*protocolRequest
}

// NewProtocolStore constructs a ProtocolStore rooted at teamDir/protocols.json
// and rehydrates its state from disk (pruning expired entries).
func NewProtocolStore(teamDir string) *ProtocolStore {
	ps := &ProtocolStore{
		path:     filepath.Join(teamDir, protocolFileName),
		requests: map[string]*protocolRequest{},
	}
	ps.loadFromDisk()
	return ps
}

// loadFromDisk rehydrates state from the JSON file (if it exists) and prunes expired entries.
func (ps *ProtocolStore) loadFromDisk() {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	data, err := os.ReadFile(ps.path)
	if err != nil {
		return
	}
	var saved struct {
		Counter  int                         `json:"counter"`
		Requests map[string]*protocolRequest `json:"requests"`
	}
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	ps.counter = saved.Counter
	if saved.Requests != nil {
		ps.requests = saved.Requests
	}
	ps.pruneExpiredLocked()
	ps.saveLocked()
}

// saveLocked writes current state to disk. Caller must hold ps.mu.
func (ps *ProtocolStore) saveLocked() {
	if ps.path == "" {
		return
	}
	payload := struct {
		Counter  int                         `json:"counter"`
		Requests map[string]*protocolRequest `json:"requests"`
	}{Counter: ps.counter, Requests: ps.requests}
	data, _ := json.MarshalIndent(payload, "", "  ")
	os.WriteFile(ps.path, data, 0o644)
}

// pruneExpiredLocked drops stale requests. Caller must hold ps.mu.
func (ps *ProtocolStore) pruneExpiredLocked() {
	now := time.Now().Unix()
	for id, req := range ps.requests {
		age := now - req.UpdatedAt
		switch req.Status {
		case "pending":
			if age > int64(infra.PlanRequestTTL.Seconds()) {
				delete(ps.requests, id)
			}
		case "approved", "rejected":
			if age > int64(infra.ApprovedPlanTTL.Seconds()) {
				delete(ps.requests, id)
			}
		}
	}
}

// nextReqIDLocked allocates a new request id. Caller must hold ps.mu.
func (ps *ProtocolStore) nextReqIDLocked() string {
	ps.counter++
	return fmt.Sprintf("req_%d", ps.counter)
}

// HandleShutdownReq posts a shutdown request for a teammate.
// Returns the request id in a human-readable message, or an error string.
func HandleShutdownReq(ps *ProtocolStore, b *MessageBus, teammate string) string {
	if ps == nil || b == nil {
		return "Error: no active session"
	}
	id := PostShutdownRequest(ps, b, teammate)
	return fmt.Sprintf("Shutdown request %s sent to '%s'", id, teammate)
}

// PostShutdownRequest records a shutdown request on the given
// ProtocolStore and dispatches the corresponding bus message. Kept as
// a free function so Session.Deactivate can target a non-active
// session's store without touching the active globals.
func PostShutdownRequest(ps *ProtocolStore, b *MessageBus, teammate string) string {
	ps.mu.Lock()
	id := ps.nextReqIDLocked()
	now := time.Now().Unix()
	ps.requests[id] = &protocolRequest{
		ID: id, Kind: "shutdown", Target: teammate,
		Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	ps.saveLocked()
	ps.mu.Unlock()

	b.Send("lead", teammate, "Please shut down.", "shutdown_request", map[string]any{"request_id": id})
	return id
}

// SubmitPlan is called by a teammate to submit a plan for lead approval.
func SubmitPlan(ps *ProtocolStore, b *MessageBus, from, planContent string) string {
	if ps == nil || b == nil {
		return "Error: no active session"
	}
	ps.mu.Lock()
	id := ps.nextReqIDLocked()
	now := time.Now().Unix()
	ps.requests[id] = &protocolRequest{
		ID: id, Kind: "plan", From: from, Plan: planContent,
		Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	ps.saveLocked()
	ps.mu.Unlock()

	b.Send(from, "lead",
		fmt.Sprintf("Plan submitted (request_id=%s):\n%s", id, planContent),
		"plan_submission", map[string]any{"request_id": id})
	return fmt.Sprintf("Plan %s submitted. Waiting for lead approval.", id)
}

// HandlePlanReview approves or rejects a pending plan request.
func HandlePlanReview(ps *ProtocolStore, b *MessageBus, reqID string, approve bool, feedback string) string {
	if ps == nil || b == nil {
		return "Error: no active session"
	}
	ps.mu.Lock()
	req, ok := ps.requests[reqID]
	if !ok || req.Kind != "plan" {
		ps.mu.Unlock()
		return "Error: Unknown plan request_id"
	}
	if approve {
		req.Status = "approved"
	} else {
		req.Status = "rejected"
	}
	req.UpdatedAt = time.Now().Unix()
	from := req.From
	status := req.Status
	ps.saveLocked()
	ps.mu.Unlock()

	b.Send("lead", from, feedback, "plan_approval_response",
		map[string]any{"request_id": reqID, "approve": approve, "feedback": feedback})
	return fmt.Sprintf("Plan %s for '%s'", status, from)
}

// HasApprovedPlan checks if a teammate has at least one non-expired
// approved plan in the given ProtocolStore.
func HasApprovedPlan(ps *ProtocolStore, teammate string) bool {
	if ps == nil {
		return false
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.pruneExpiredLocked()
	for _, req := range ps.requests {
		if req.Kind == "plan" && req.From == teammate && req.Status == "approved" {
			return true
		}
	}
	return false
}