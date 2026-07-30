package event

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionLogSinkCreatesParentDir(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions", "s1", "session.log")

	sink, err := NewSessionLogSink(path)
	if err != nil {
		t.Fatalf("NewSessionLogSink: %v", err)
	}
	defer sink.Close()

	sink.Emit(Event{Type: ModelCalled, AgentID: "lead", SessionID: "s1"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read session.log: %v", err)
	}
	if !strings.Contains(string(data), `"model_called"`) {
		t.Fatalf("expected model_called in log, got %q", data)
	}
}

func TestSessionLogSinkPreservesStructuredDecisionFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log")
	sink, err := NewSessionLogSink(path)
	if err != nil {
		t.Fatal(err)
	}
	sink.Emit(Event{
		Type:       PlanningDecision,
		ToolCallID: "call-1",
		ToolName:   "bash",
		Status:     "blocked",
		Payload:    map[string]string{"action": "block_unplanned_side_effect"},
	})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tool_call_id":"call-1"`, `"status":"blocked"`, `"payload":{"action":"block_unplanned_side_effect"}`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("session.log missing %q: %s", want, data)
		}
	}
}

func TestSessionLogSinkRecoversAfterDirRemoved(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions", "s1")
	path := filepath.Join(dir, "session.log")

	sink, err := NewSessionLogSink(path)
	if err != nil {
		t.Fatalf("NewSessionLogSink: %v", err)
	}
	defer sink.Close()

	sink.Emit(Event{Type: AgentStarted, AgentID: "lead", SessionID: "s1"})
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove session dir: %v", err)
	}

	sink.Emit(Event{Type: ModelCalled, AgentID: "lead", SessionID: "s1"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recreated session.log: %v", err)
	}
	if !strings.Contains(string(data), `"model_called"`) {
		t.Fatalf("expected model_called after reopen, got %q", data)
	}
}
