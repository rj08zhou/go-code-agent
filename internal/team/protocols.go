package team

import (
	"fmt"
	"sync"
	"time"
)

type planStatus string

const (
	planPending  planStatus = "pending"
	planApproved planStatus = "approved"
	planRejected planStatus = "rejected"
)

// planRequest wraps a teammate's plan submission.
type planRequest struct {
	ID        string
	Teammate  string
	Plan      string
	Status    planStatus
	CreatedAt time.Time
}

// ProtocolStore manages plan approval and shutdown protocols.
type ProtocolStore struct {
	mu              sync.RWMutex
	plans           map[string]*planRequest
	byID            map[string]*planRequest
	sequence        uint64
	planTTL         time.Duration
	approvedPlanTTL time.Duration
	bus             *MessageBus
}

func NewProtocolStore(bus *MessageBus) *ProtocolStore {
	return &ProtocolStore{
		plans:           make(map[string]*planRequest),
		byID:            make(map[string]*planRequest),
		planTTL:         30 * time.Minute,
		approvedPlanTTL: 24 * time.Hour,
		bus:             bus,
	}
}

// SubmitPlanWithID posts a plan and returns a stable request ID for approval.
func SubmitPlanWithID(ps *ProtocolStore, bus *MessageBus, name, plan string) (string, string) {
	if ps == nil || bus == nil {
		return "", "Error: protocol service unavailable"
	}
	ps.mu.Lock()
	ps.sequence++
	id := fmt.Sprintf("plan_%d", ps.sequence)
	request := &planRequest{
		ID:        id,
		Teammate:  name,
		Plan:      plan,
		Status:    planPending,
		CreatedAt: time.Now(),
	}
	ps.plans[name] = request
	ps.byID[id] = request
	ps.mu.Unlock()
	bus.Send(name, "lead", fmt.Sprintf("Plan submitted (request_id=%s) from %s:\n%s", id, name, plan), "plan_request", map[string]any{"request_id": id})
	return id, fmt.Sprintf("Plan %s submitted to lead for approval.", id)
}

// SubmitPlan posts a plan for the lead agent to review.
func SubmitPlan(ps *ProtocolStore, bus *MessageBus, name, plan string) string {
	_, message := SubmitPlanWithID(ps, bus, name, plan)
	return message
}

// HasApprovedPlan reports whether a teammate currently holds an approved plan.
func HasApprovedPlan(ps *ProtocolStore, name string) bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	pr, exists := ps.plans[name]
	if !exists || pr.Status != planApproved {
		return false
	}
	if time.Since(pr.CreatedAt) > ps.approvedPlanTTL {
		return false
	}
	return true
}

// ReviewPlan resolves a plan by request ID and notifies the submitting teammate.
func ReviewPlan(ps *ProtocolStore, bus *MessageBus, requestID string, approve bool, feedback string) string {
	if ps == nil || bus == nil {
		return "Error: protocol service unavailable"
	}
	ps.mu.Lock()
	pr, exists := ps.byID[requestID]
	if !exists {
		ps.mu.Unlock()
		return fmt.Sprintf("No plan found for request %s", requestID)
	}
	if pr.Status != planPending {
		status := pr.Status
		ps.mu.Unlock()
		return fmt.Sprintf("Plan %s is already %s", requestID, status)
	}
	if approve {
		pr.Status = planApproved
	} else {
		pr.Status = planRejected
	}
	teammate := pr.Teammate
	status := pr.Status
	ps.mu.Unlock()
	bus.Send("lead", teammate, feedback, "plan_approval_response", map[string]any{
		"request_id": requestID,
		"approve":    approve,
		"feedback":   feedback,
	})
	return fmt.Sprintf("Plan %s for %s", status, teammate)
}

// ApprovePlan marks a teammate's latest plan as approved. Kept for compatibility.
func ApprovePlan(ps *ProtocolStore, name string) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	pr, exists := ps.plans[name]
	if !exists {
		return fmt.Sprintf("No pending plan from %s", name)
	}
	if pr.Status != planPending {
		return fmt.Sprintf("Plan from %s is already %s", name, pr.Status)
	}
	pr.Status = planApproved
	return fmt.Sprintf("Plan from %s approved", name)
}

// RejectPlan marks a teammate's latest plan as rejected. Kept for compatibility.
func RejectPlan(ps *ProtocolStore, name, reason string) string {
	ps.mu.Lock()
	pr, exists := ps.plans[name]
	if exists && pr.Status == planPending {
		pr.Status = planRejected
	}
	ps.mu.Unlock()
	if !exists {
		return fmt.Sprintf("No pending plan from %s", name)
	}
	return fmt.Sprintf("Plan from %s rejected: %s", name, reason)
}

// PostShutdownRequest sends a shutdown message to a teammate.
func PostShutdownRequest(ps *ProtocolStore, bus *MessageBus, name string) string {
	bus.Send("lead", name, "shutdown", "shutdown_request", nil)
	return fmt.Sprintf("Shutdown request sent to %s", name)
}

// --- tool.TeamProtocolService methods ---

// ShutdownRequest sends a shutdown message to a teammate.
func (ps *ProtocolStore) ShutdownRequest(teammate string) string {
	if ps.bus == nil {
		return "Error: protocol bus unavailable"
	}
	ps.bus.Send("lead", teammate, "shutdown", "shutdown_request", nil)
	return fmt.Sprintf("Shutdown request sent to %s", teammate)
}

// SubmitPlan posts a plan for the lead agent to review.
func (ps *ProtocolStore) SubmitPlan(agent, plan string) string {
	return SubmitPlan(ps, ps.bus, agent, plan)
}

// ReviewPlan resolves a plan by request ID and notifies the submitting teammate.
func (ps *ProtocolStore) ReviewPlan(requestID string, approve bool, feedback string) string {
	return ReviewPlan(ps, ps.bus, requestID, approve, feedback)
}
