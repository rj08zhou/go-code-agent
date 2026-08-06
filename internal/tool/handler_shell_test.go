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

func TestShellRejectsHostRepoPathsFromAWorktree(t *testing.T) {
	host := t.TempDir()
	worktree := t.TempDir()
	bg := &recordingBackgroundService{}
	defs := shellTools(builtinDeps{bgSvc: bg})
	bash, background := defs[0], defs[1]
	isolated := &ToolScope{SessionID: "test", Workdir: worktree, SourceWorkdir: host}

	denied := bash.Handler(isolated, mustCommand(t, "cat "+host+"/internal/agent/runner.go"))
	if denied.Status != StatusDenied || !strings.Contains(denied.Output, "worktree") {
		t.Fatalf("host path not blocked: %+v", denied)
	}
	if got := background.Handler(isolated, mustCommand(t, "go build "+host+"/...")); got.Status != StatusDenied {
		t.Fatalf("background host path not blocked: %+v", got)
	}
	if bg.called {
		t.Fatal("background service ran a command aimed at the host repo")
	}

	// Relative work inside the worktree stays allowed.
	if got := bash.Handler(isolated, mustCommand(t, "echo fine")); got.Status != StatusSucceeded {
		t.Fatalf("relative command blocked: %+v", got)
	}
	// The lead has no worktree, so nothing is remapped and nothing is blocked.
	lead := &ToolScope{SessionID: "test", Workdir: host}
	if got := bash.Handler(lead, mustCommand(t, "echo "+host)); got.Status != StatusSucceeded {
		t.Fatalf("lead command blocked: %+v", got)
	}
}

func mustCommand(t *testing.T, command string) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatal(err)
	}
	return args
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
