package tool

import (
	"encoding/json"
	"strings"
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

func TestBashReportsNonZeroExitAsFailed(t *testing.T) {
	defs := shellTools(builtinDeps{})
	bash := defs[0]
	scope := &ToolScope{SessionID: "test", Workdir: t.TempDir()}

	failed := bash.Handler(scope, json.RawMessage(`{"command":"grep needle /dev/null"}`))
	if failed.Status != StatusFailed {
		t.Fatalf("status = %s, want failed (result: %+v)", failed.Status, failed)
	}
	if !strings.Contains(failed.Output, "exit status 1") {
		t.Fatalf("output = %q, want exit status detail", failed.Output)
	}

	withOutput := bash.Handler(scope, json.RawMessage(`{"command":"echo broken; grep needle /dev/null"}`))
	if withOutput.Status != StatusFailed || !strings.Contains(withOutput.Output, "broken") {
		t.Fatalf("failed result must keep command output: %+v", withOutput)
	}

	ok := bash.Handler(scope, json.RawMessage(`{"command":"echo fine"}`))
	if ok.Status != StatusSucceeded || ok.Output != "fine" {
		t.Fatalf("success path changed: %+v", ok)
	}
}

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
