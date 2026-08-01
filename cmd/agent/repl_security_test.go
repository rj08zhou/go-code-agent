package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-code-agent/internal/security"
)

func TestHandleSecurityCommand(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "status",
			raw:  "/security",
			want: []string{"Security Status:", "/security test-bash <command>"},
		},
		{
			name: "missing command",
			raw:  "/security test-bash",
			want: []string{"Usage: /security test-bash <command>"},
		},
		{
			name: "safe command",
			raw:  "/security test-bash git status",
			want: []string{"Command: git status", "Risk: safe", "Decision: allow", "Reason: read-only/inspection-only"},
		},
		{
			name: "command whitespace is preserved",
			raw:  "/security   test-bash   printf 'a  b'",
			want: []string{"Command: printf 'a  b'", "Risk: safe", "Decision: allow"},
		},
		{
			name: "dangerous command requires confirmation",
			raw:  "/security test-bash rm file.txt",
			want: []string{"Risk: danger", "Decision: confirm", "Reason: command matches a potentially dangerous pattern"},
		},
		{
			name: "denied command",
			raw:  "/security test-bash sudo ls",
			want: []string{"Risk: deny", "Decision: deny", "Reason: dangerous command blocked"},
		},
		{
			name: "unknown subcommand",
			raw:  "/security unknown",
			want: []string{"Unknown security command: unknown", "Usage: /security test-bash <command>"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := handleSecurityCommand(tc.raw, strings.Fields(tc.raw), security.NewPermissions())
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("output %q does not contain %q", got, want)
				}
			}
		})
	}
}

func TestHandleSecurityCommandAppliesPermissionRules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")
	if err := os.WriteFile(path, []byte(`[{"tool":"bash","level":"block","pattern":"git status"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	perms := security.NewPermissions()
	if err := perms.Load(dir); err != nil {
		t.Fatal(err)
	}

	got := handleSecurityCommand("/security test-bash git status", []string{"/security", "test-bash", "git", "status"}, perms)
	for _, want := range []string{"Risk: safe", "Decision: deny", "Reason: blocked by user permission rule"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q does not contain %q", got, want)
		}
	}
}
