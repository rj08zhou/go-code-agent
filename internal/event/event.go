// Package event defines typed events for engine-UI decoupling.
package event

import (
	"encoding/json"
	"go-code-agent-refactor/internal/llm"
	"time"
)

type EventType string

const (
	SessionActivated  EventType = "session_activated"
	AgentStarted      EventType = "agent_started"
	ModelCalled       EventType = "model_called"
	ToolStarted       EventType = "tool_started"
	ToolFinished      EventType = "tool_finished"
	ApprovalRequested EventType = "approval_requested"
	ApprovalResolved  EventType = "approval_resolved"
	TaskChanged       EventType = "task_changed"
	AgentStopped      EventType = "agent_stopped"
	SessionClosed     EventType = "session_closed"

	// Decision events — matching original project's Decision* categories.
	PlanningDecision EventType = "planning"      // plan gate / DAG nudge
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
		Type      EventType  `json:"type"`
		TraceID   string     `json:"trace_id,omitempty"`
		SessionID string     `json:"session_id,omitempty"`
		AgentID   string     `json:"agent_id,omitempty"`
		ToolName  string     `json:"tool_name,omitempty"`
		ModelID   string     `json:"model_id,omitempty"`
		Output    string     `json:"output,omitempty"`
		Timestamp string     `json:"timestamp"`
		Outcome   string     `json:"outcome,omitempty"`
		Error     string     `json:"error,omitempty"`
		Duration  float64    `json:"duration_sec,omitempty"`
		Usage     *llm.Usage `json:"usage,omitempty"`
	}
	a := alias{
		Type:      e.Type,
		TraceID:   e.TraceID,
		SessionID: e.SessionID,
		AgentID:   e.AgentID,
		ToolName:  e.ToolName,
		ModelID:   e.ModelID,
		Output:    e.Output,
		Timestamp: e.Timestamp.Format(time.RFC3339Nano),
		Outcome:   e.Outcome,
		Error:     e.Error,
		Duration:  e.Duration.Seconds(),
		Usage:     e.Usage,
	}
	return json.Marshal(a)
}

// Sink consumes events in a fire-and-forget manner.
type Sink interface {
	Emit(ev Event)
}

// MultiSink broadcasts events to all registered sinks.
type MultiSink struct {
	sinks []Sink
}

func NewMultiSink(sinks ...Sink) *MultiSink {
	return &MultiSink{sinks: sinks}
}

func (m *MultiSink) Emit(ev Event) {
	ev.Timestamp = time.Now()
	for _, s := range m.sinks {
		s.Emit(ev)
	}
}

func (m *MultiSink) Add(s Sink) {
	m.sinks = append(m.sinks, s)
}

// NopSink discards all events.
type NopSink struct{}

func (NopSink) Emit(Event) {}
