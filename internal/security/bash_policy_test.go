package security

import "testing"

func TestIsReadOnlyBash(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		// --- should be allowed (read-only) ---
		{"ls", "ls -la", true},
		{"cat", "cat README.md", true},
		{"find", "find . -name *.go", true},
		{"grep", "grep -r foo .", true},
		{"head", "head -n 50 file.log", true},
		{"wc", "wc -l file.log", true},
		{"echo plain", "echo hello", true},
		{"sort plain", "sort file.txt", true},
		{"sed no -i", "sed -n 1,10p file.txt", true},

		// --- should be rejected ---
		{"empty", "", false},
		{"whitespace", "   ", false},
		{"unknown cmd", "make build", false},
		{"rm", "rm -rf foo", false},
		{"redirect out", "ls > out.txt", false},
		{"redirect append", "echo x >> log", false},
		{"redirect in", "cat < input", false},
		{"pipe", "ls | grep foo", false},
		{"semicolon", "ls; rm foo", false},
		{"and", "ls && rm foo", false},
		{"backtick", "echo `whoami`", false},
		{"command sub", "echo $(pwd)", false},
		{"sed -i", "sed -i s/foo/bar/ file.txt", false},
		{"sed -i.bak", "sed -i.bak s/x/y/ a.txt", false},
		{"awk -i inplace", "awk -i inplace {print} file", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsReadOnlyBash(tc.cmd)
			if got != tc.want {
				t.Errorf("IsReadOnlyBash(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestValidate_UserRulesCannotBypassDangerPatterns is the core security
// guarantee of the permission-rules feature: a user `allow` rule must
// NEVER let a hard-blacklisted command through. DangerPatterns is
// checked before user rules in Validate, so even a broad "allow *"
// leaves "rm -rf /" blocked.
func TestValidate_UserRulesCannotBypassDangerPatterns(t *testing.T) {
	GlobalPermissions.Set([]PermissionRule{
		{Tool: "*", Pattern: "*", Action: "allow"}, // maximally permissive
	})
	defer GlobalPermissions.Set(nil)

	danger := []string{"rm -rf /", "sudo rm foo", "shutdown now"}
	for _, cmd := range danger {
		allowed, _, reason := DefaultBashPolicy.Validate(cmd)
		if allowed {
			t.Errorf("Validate(%q) allowed=true despite allow-all rule; DangerPatterns must win (reason=%q)", cmd, reason)
		}
	}
}

// TestValidate_UserRuleActions checks deny/allow/ask are honored for
// allowlisted, non-dangerous commands.
func TestValidate_UserRuleActions(t *testing.T) {
	GlobalPermissions.Set([]PermissionRule{
		{Tool: "bash", Pattern: "git push --force*", Action: "deny"},
		{Tool: "bash", Pattern: "git commit -m *", Action: "allow"},
		{Tool: "bash", Pattern: "git *", Action: "ask"},
	})
	defer GlobalPermissions.Set(nil)

	// deny
	if allowed, _, _ := DefaultBashPolicy.Validate("git push --force origin main"); allowed {
		t.Errorf("force-push should be denied by rule")
	}
	// allow -> allowed, no confirm
	if allowed, confirm, _ := DefaultBashPolicy.Validate("git commit -m 'x'"); !allowed || confirm {
		t.Errorf("git commit -m should be allowed without confirm; got allowed=%v confirm=%v", allowed, confirm)
	}
	// ask -> allowed but needs confirm
	if allowed, confirm, _ := DefaultBashPolicy.Validate("git status"); !allowed || !confirm {
		t.Errorf("git status should be ask (allowed+confirm); got allowed=%v confirm=%v", allowed, confirm)
	}
}
