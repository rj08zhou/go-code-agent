// Package event defines typed events for engine-UI decoupling.
package event

import (
	"encoding/json"
	"fmt"
	"go-code-agent/internal/llm"
	"go-code-agent/internal/logging"
	"sync/atomic"
	"time"
)

type EventType string

const (
	SessionActivated  EventType = "session_activated"
	AgentStarted      EventType = "agent_started"
	ModelCalled       EventType = "model_called"
	ModelRetry        EventType = "model_retry" // retry backoff wait / provider fallback
	ToolStarted       EventType = "tool_started"
	ToolFinished      EventType = "tool_finished"
	ApprovalRequested EventType = "approval_requested"
	ApprovalResolved  EventType = "approval_resolved"
	TaskChanged       EventType = "task_changed"
	AgentStopped      EventType = "agent_stopped"
	SessionClosed     EventType = "session_closed"

	// Decision events — matching original project's Decision* categories.
	PlanningDecision EventType = "planning"      // policy: plan gate / unplanned side-effect block
	ContextDecision  EventType = "context"       // micro / auto compaction
	TurnDecision     EventType = "turn"          // per-turn summar
	TurnComplete     EventType = "turn_complete" // final turn wrap-up
	MemoryDecision   EventType = "memory"        // auto-lesson
	JudgeDecision    EventType = "judge"         // judge verdict

	ReflectionTriggered EventType = "reflection_triggered" // self-correction / stuck / nag
)

type Event struct {
	Type       EventType     `json:"type"`
	TraceID    string        `json:"trace_id,omitempty"`
	ProjectID  string        `json:"project_id,omitempty"`
	SessionID  string        `json:"session_id,omitempty"`
	AgentID    string        `json:"agent_id,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolName   string        `json:"tool_name,omitempty"`
	ModelID    string        `json:"model_id,omitempty"`
	Timestamp  time.Time     `json:"timestamp"`
	Status     string        `json:"status,omitempty"`
	Outcome    string        `json:"outcome,omitempty"`
	Error      string        `json:"error,omitempty"`
	Duration   time.Duration `json:"duration,omitempty"`
	Output     string        `json:"output,omitempty"`
	Usage      *llm.Usage    `json:"usage,omitempty"`
	Payload    any           `json:"payload,omitempty"`
}

// MarshalJSON returns the JSON representation of the event.
func (e Event) MarshalJSON() ([]byte, error) {
	type alias struct {
		Type       EventType  `json:"type"`
		TraceID    string     `json:"trace_id,omitempty"`
		SessionID  string     `json:"session_id,omitempty"`
		AgentID    string     `json:"agent_id,omitempty"`
		ToolCallID string     `json:"tool_call_id,omitempty"`
		ToolName   string     `json:"tool_name,omitempty"`
		ModelID    string     `json:"model_id,omitempty"`
		Output     string     `json:"output,omitempty"`
		Timestamp  string     `json:"timestamp"`
		Status     string     `json:"status,omitempty"`
		Outcome    string     `json:"outcome,omitempty"`
		Error      string     `json:"error,omitempty"`
		Duration   float64    `json:"duration_sec,omitempty"`
		Usage      *llm.Usage `json:"usage,omitempty"`
		Payload    any        `json:"payload,omitempty"`
	}
	a := alias{
		Type:       e.Type,
		TraceID:    e.TraceID,
		SessionID:  e.SessionID,
		AgentID:    e.AgentID,
		ToolCallID: e.ToolCallID,
		ToolName:   e.ToolName,
		ModelID:    e.ModelID,
		Output:     e.Output,
		Timestamp:  e.Timestamp.Format(time.RFC3339Nano),
		Status:     e.Status,
		Outcome:    e.Outcome,
		Error:      e.Error,
		Duration:   e.Duration.Seconds(),
		Usage:      e.Usage,
		Payload:    e.Payload,
	}
	return json.Marshal(a)
}

// Sink consumes events in a fire-and-forget manner.
type Sink interface {
	Emit(ev Event)
}

// MultiSink broadcasts events to all registered sinks.
//
// Emit is fire-and-forget: a panic or blocking in any single sink must never
// take down the caller (the agent loop). Each sink is therefore invoked inside
// its own recovery scope; a failing sink is skipped, counted, and logged, and
// broadcasting continues to the remaining sinks.
type MultiSink struct {
	sinks    []Sink
	failures atomic.Int64
}

func NewMultiSink(sinks ...Sink) *MultiSink {
	return &MultiSink{sinks: sinks}
}

func (m *MultiSink) Emit(ev Event) {
	ev.Timestamp = time.Now()
	for _, s := range m.sinks {
		func(s Sink) {
			defer func() {
				if r := recover(); r != nil {
					m.failures.Add(1)
					logging.Default().Warn(
						fmt.Sprintf("event sink panicked and was skipped: %v", r))
				}
			}()
			s.Emit(ev)
		}(s)
	}
}

// Failures returns the number of sink invocations that panicked since the
// MultiSink was created. It is a coarse health signal: a non-zero value means
// at least one sink is misbehaving.
func (m *MultiSink) Failures() int64 {
	return m.failures.Load()
}

func (m *MultiSink) Add(s Sink) {
	m.sinks = append(m.sinks, s)
}
