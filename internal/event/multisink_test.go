package event

import (
	"sync"
	"testing"
)

// panicSink panics on every Emit.
type panicSink struct{}

func (panicSink) Emit(Event) { panic("boom") }

// recordingSink records every event it receives.
type recordingSink struct {
	mu    sync.Mutex
	count int
}

func (s *recordingSink) Emit(Event) {
	s.mu.Lock()
	s.count++
	s.mu.Unlock()
}

func (s *recordingSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// TestMultiSinkIsolatesPanic verifies that a panicking sink does not crash
// Emit, that the remaining sinks still receive the event, and that the failure
// counter is incremented.
func TestMultiSinkIsolatesPanic(t *testing.T) {
	good := &recordingSink{}
	ms := NewMultiSink(panicSink{}, good)

	// Must not panic.
	ms.Emit(Event{Type: AgentStarted, AgentID: "lead", SessionID: "s1"})

	if good.Count() != 1 {
		t.Fatalf("healthy sink should have received the event, got %d", good.Count())
	}
	if got := ms.Failures(); got != 1 {
		t.Fatalf("expected 1 failure recorded, got %d", got)
	}
}

// TestMultiSinkMultiplePanics verifies the counter accumulates across sinks and
// calls.
func TestMultiSinkMultiplePanics(t *testing.T) {
	good := &recordingSink{}
	ms := NewMultiSink(panicSink{}, panicSink{}, good)

	ms.Emit(Event{Type: AgentStarted})
	ms.Emit(Event{Type: ModelCalled})

	if good.Count() != 2 {
		t.Fatalf("healthy sink should have received both events, got %d", good.Count())
	}
	if got := ms.Failures(); got != 4 {
		t.Fatalf("expected 4 failures recorded, got %d", got)
	}
}
