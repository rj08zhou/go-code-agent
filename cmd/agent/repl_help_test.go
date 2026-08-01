package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-code-agent/internal/application"
	"go-code-agent/internal/hitlaudit"
	"go-code-agent/internal/security"
)

func TestRenderHelpListsEveryREPLCommand(t *testing.T) {
	help := renderHelp()
	commands := []string{
		"/help",
		"/task clear",
		"/task reset",
		"/tasks",
		"/dag",
		"/memory",
		"/mcp",
		"/mcp pending",
		"/mcp approve <name>",
		"/mcp connect <name> <cmd> [args...]",
		"/mcp disconnect <name>",
		"/team",
		"/team spawn <name> <role> <prompt>",
		"/team shutdown <name>",
		"/team message <name> <content>",
		"/team inbox",
		"/session",
		"/session list",
		"/session switch <id>",
		"/session new",
		"/session rename <title>",
		"/session archive",
		"/judge",
		"/approval",
		"/approval manual",
		"/approval safe-auto",
		"/approval all-auto confirm",
		"/approval reject",
		"/approval notify-only",
		"/approve ...",
		"/hitl ...",
		"/inbox",
		"/search <query>",
		"/permissions",
		"/permissions reload",
		"/usage",
		"/security",
		"/security test-bash <command>",
		"/decisions",
		"/compact",
		"/exit, /quit",
	}

	for _, command := range commands {
		if !strings.Contains(help, command) {
			t.Errorf("help does not list %q", command)
		}
	}
}

func TestRenderHelpExplainsApprovalSafety(t *testing.T) {
	help := renderHelp()
	for _, want := range []string{
		"Approval starts in safe-auto mode",
		"all-auto requires an explicit \"confirm\"",
		"hard Bash deny rules and permissions.json still apply",
		"without executing the command",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help does not contain safety guidance %q", want)
		}
	}
}

func TestHandleApprovalRequiresConfirmationForAllAuto(t *testing.T) {
	r := newApprovalTestRepl()

	got := r.handleApproval([]string{"/approval", "all-auto"})
	if !strings.Contains(got, "/approval all-auto confirm") {
		t.Fatalf("missing confirmation guidance: %q", got)
	}
	if mode := effectiveApprovalMode(r.built); mode != "safe-auto" {
		t.Fatalf("unconfirmed command changed mode to %q", mode)
	}

	got = r.handleApproval([]string{"/approval", "all-auto", "confirm"})
	if !strings.Contains(got, "Approval mode: all-auto") {
		t.Fatalf("confirmed response = %q", got)
	}
	if mode := effectiveApprovalMode(r.built); mode != "all-auto" {
		t.Fatalf("confirmed mode = %q, want all-auto", mode)
	}
	if r.built.Security.Approval.ShouldPreviewDiff() {
		t.Fatal("all-auto should skip diff previews")
	}
}

func TestHandleApprovalCanonicalModes(t *testing.T) {
	r := newApprovalTestRepl()
	cases := []struct {
		command []string
		want    string
		preview string
	}{
		{[]string{"/approval", "manual"}, "manual", "enabled"},
		{[]string{"/approval", "safe-auto"}, "safe-auto", "enabled"},
		{[]string{"/approval", "reject"}, "reject", "skipped"},
		{[]string{"/approval", "notify-only"}, "notify-only", "skipped"},
	}
	for _, tc := range cases {
		if got := r.handleApproval(tc.command); !strings.Contains(got, "Approval mode: "+tc.want) {
			t.Errorf("handleApproval(%v) = %q", tc.command, got)
		}
		if got := effectiveApprovalMode(r.built); got != tc.want {
			t.Errorf("mode after %v = %q, want %q", tc.command, got, tc.want)
		}
		status := r.handleApproval([]string{"/approval"})
		if !strings.Contains(status, "Diff preview: "+tc.preview) {
			t.Errorf("status after %v = %q, want preview %s", tc.command, status, tc.preview)
		}
	}
}

func TestLegacyApprovalAliasesUseCanonicalSafety(t *testing.T) {
	r := newApprovalTestRepl()
	if got := r.handleApproval([]string{"/approve", "off"}); !strings.Contains(got, "use /approval manual") {
		t.Fatalf("legacy manual alias = %q", got)
	}
	if mode := effectiveApprovalMode(r.built); mode != "manual" {
		t.Fatalf("legacy /approve off mode = %q", mode)
	}

	got := r.handleApproval([]string{"/hitl", "off"})
	if !strings.Contains(got, "/approval all-auto confirm") {
		t.Fatalf("legacy HITL off bypassed confirmation: %q", got)
	}
	if mode := effectiveApprovalMode(r.built); mode != "manual" {
		t.Fatalf("unconfirmed legacy HITL off changed mode to %q", mode)
	}
}

func newApprovalTestRepl() *repl {
	hitl := hitlaudit.NewHITLManager(nil)
	hitl.SetEnabled(true)
	hitl.SetMode(hitlaudit.HITLModeSafeOnly)
	approval := security.NewApprovalState()
	approval.ApplyPreset("safe-auto")
	return &repl{built: &application.BuiltRunner{Security: application.SecurityFacade{
		HITL: hitl, Approval: approval,
	}}}
}

type stubWebService struct {
	output string
	err    error
}

func (s stubWebService) Fetch(context.Context, string) (string, error) { return "", nil }
func (s stubWebService) Search(context.Context, string) (string, error) {
	return s.output, s.err
}

func TestRunSearchAlwaysReturnsFeedback(t *testing.T) {
	tests := []struct {
		name string
		web  stubWebService
		set  bool
		want string
	}{
		{name: "unavailable", want: "Web search is unavailable."},
		{name: "failure", set: true, web: stubWebService{err: errors.New("backend down")}, want: "Search failed: backend down"},
		{name: "empty", set: true, web: stubWebService{output: " \n"}, want: "No search results."},
		{name: "results", set: true, web: stubWebService{output: " result \n"}, want: "result"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &repl{built: &application.BuiltRunner{}}
			if tc.set {
				r.built.Runtime.Web = tc.web
			}
			if got := r.runSearch(context.Background(), "query"); got != tc.want {
				t.Fatalf("runSearch() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatInboxMessagesIsReadableAndTerminalSafe(t *testing.T) {
	if got := formatInboxMessages(nil); got != "Inbox is empty." {
		t.Fatalf("empty inbox = %q", got)
	}
	got := formatInboxMessages([]map[string]any{{
		"from":       "alice",
		"type":       "plan_request",
		"request_id": "plan-1",
		"content":    "review this\n\x1b[31mplan",
	}})
	for _, want := range []string{"Inbox (1):", "[plan_request]", "from alice", "request_id=plan-1", `review this\n\x1b[31mplan`} {
		if !strings.Contains(got, want) {
			t.Fatalf("inbox output %q does not contain %q", got, want)
		}
	}
	if strings.ContainsRune(got, '\x1b') || strings.Contains(got, `{"from"`) {
		t.Fatalf("inbox output is unsafe or raw JSON: %q", got)
	}
}
