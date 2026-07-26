package tool

import (
	"encoding/json"
	"testing"
)

type recordingBackgroundService struct {
	called bool
}

func (s *recordingBackgroundService) Run(_, _ string, _ int) string {
	s.called = true
	return "started"
}

func (*recordingBackgroundService) Check(string) string { return "" }

func TestBackgroundRunAppliesBashPolicy(t *testing.T) {
	bg := &recordingBackgroundService{}
	defs := shellTools(builtinDeps{bgSvc: bg})
	background := defs[1]

	result := background.Handler(
		&ToolScope{SessionID: "test"},
		json.RawMessage(`{"command":"rm -rf /"}`),
	)
	if result.Status != StatusDenied {
		t.Fatalf("status = %s, want denied (result: %+v)", result.Status, result)
	}
	if bg.called {
		t.Fatal("background service was called for a blocked command")
	}
}
