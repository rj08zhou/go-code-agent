package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything at all", true},
		{"*", "", true},
		{"git commit -m *", "git commit -m 'fix bug'", true},
		{"git commit -m *", "git commit --amend", false},
		{"git push --force*", "git push --force origin main", true},
		{"git push --force*", "git push origin main", false},
		// '*' must span '/' (unlike path.Match) - key requirement.
		{"rm -rf *", "rm -rf /tmp/some/dir", true},
		{"*/migrations/*", "db/migrations/001_init.sql", true},
		{"*/migrations/*", "db/models/user.go", false},
		{"*.env", ".env", true},
		{"*.env", "config/.env", true},
		{"*.env", "env.example", false},
		{"?at", "cat", true},
		{"?at", "at", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pattern, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestToolMatches(t *testing.T) {
	cases := []struct {
		ruleTool, tool string
		want           bool
	}{
		{"*", "bash", true},
		{"bash", "bash", true},
		{"bash", "edit_file", false},
		{"mcp__*", "mcp__github__create_issue", true},
		{"mcp__*", "bash", false},
	}
	for _, c := range cases {
		if got := toolMatches(c.ruleTool, c.tool); got != c.want {
			t.Errorf("toolMatches(%q, %q) = %v, want %v", c.ruleTool, c.tool, got, c.want)
		}
	}
}

func TestPermissionMatch_FirstMatchWins(t *testing.T) {
	pr := &PermissionRules{}
	pr.Set([]PermissionRule{
		{Tool: "bash", Pattern: "git push --force*", Action: "deny"},
		{Tool: "bash", Pattern: "git push*", Action: "allow"},
		{Tool: "bash", Pattern: "*", Action: "ask"},
	})

	// First rule (deny) wins over the later allow/ask.
	if a, _ := pr.Match("bash", "git push --force origin main"); a != PermDeny {
		t.Errorf("force-push: got %v, want deny", a)
	}
	// Second rule (allow) wins over the catch-all ask.
	if a, _ := pr.Match("bash", "git push origin main"); a != PermAllow {
		t.Errorf("normal push: got %v, want allow", a)
	}
	// Falls through to catch-all ask.
	if a, _ := pr.Match("bash", "ls -la"); a != PermAsk {
		t.Errorf("ls: got %v, want ask", a)
	}
	// A tool with no matching rule.
	if a, _ := pr.Match("read_file", "foo.go"); a != PermNoMatch {
		t.Errorf("read_file: got %v, want no-match", a)
	}
}

func TestPermissionActionParsing(t *testing.T) {
	cases := map[string]PermAction{
		"allow": PermAllow, "ALLOW": PermAllow, " deny ": PermDeny,
		"ask": PermAsk, "bogus": PermNoMatch, "": PermNoMatch,
	}
	for in, want := range cases {
		if got := (PermissionRule{Action: in}).action(); got != want {
			t.Errorf("action(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPermissionLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.json")

	// Missing file: no error, no rules.
	if w, err := GlobalPermissions.Load(path); err != nil || w != "" {
		t.Fatalf("missing file: warning=%q err=%v", w, err)
	}
	if GlobalPermissions.Count() != 0 {
		t.Fatalf("expected 0 rules for missing file, got %d", GlobalPermissions.Count())
	}

	// Valid file with one bogus rule (dropped) + two good ones.
	content := `{"rules":[
		{"tool":"bash","pattern":"git push --force*","action":"deny"},
		{"tool":"write_file","pattern":"*.env","action":"deny"},
		{"tool":"bash","pattern":"","action":"allow"}
	]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := GlobalPermissions.Load(path)
	if err != nil {
		t.Fatalf("load valid: %v", err)
	}
	if w == "" {
		t.Errorf("expected a warning about the dropped empty-pattern rule")
	}
	if GlobalPermissions.Count() != 2 {
		t.Fatalf("expected 2 valid rules, got %d", GlobalPermissions.Count())
	}
	if a, _ := GlobalPermissions.Match("write_file", "config/.env"); a != PermDeny {
		t.Errorf("write_file .env: got %v, want deny", a)
	}

	// Malformed JSON: error, previous rules untouched.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GlobalPermissions.Load(path); err == nil {
		t.Errorf("expected error for malformed JSON")
	}
	if GlobalPermissions.Count() != 2 {
		t.Errorf("rules should be untouched after malformed load, got %d", GlobalPermissions.Count())
	}

	// Reset the singleton so other tests aren't affected.
	GlobalPermissions.Set(nil)
}
